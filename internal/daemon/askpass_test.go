package daemon

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// TestMain lets the test binary stand in for `coveyd askpass`: git calls the
// helper script, the script calls this binary, and this binary asks the proxy
// exactly the way coveyd would.
func TestMain(m *testing.M) {
	if os.Getenv("COVEY_TEST_AS_ASKPASS") == "1" {
		os.Exit(Askpass(os.Args[len(os.Args)-1]))
	}
	os.Exit(m.Run())
}

func TestParseAskpassPrompt(t *testing.T) {
	for prompt, want := range map[string]struct {
		host string
		user bool
	}{
		"Username for 'https://gitlab.example.org': ":       {"gitlab.example.org", true},
		"Password for 'https://covey@gitlab.example.org': ": {"gitlab.example.org", false},
		"Password for 'https://GitHub.com': ":               {"github.com", false},
		"Enter passphrase for key '/home/agent/.ssh/id':":   {"", false},
		"Password for 'ssh://git@gitlab.example.org': ":     {"", false},
		"anything else": {"", false},
	} {
		host, user := parseAskpassPrompt(prompt)
		if host != want.host || user != want.user {
			t.Errorf("%q: got %q %v, want %q %v", prompt, host, user, want.host, want.user)
		}
	}
}

type askpassStubs struct {
	brokered []string
	decision ApprovalDecision
	subject  string
}

func askpassProxy(st *askpassStubs) *actionProxy {
	creds := map[string]InjectCredentials{
		"gitlab": {Granted: true, BaseURL: "https://gitlab.example.org", Token: "glpat-secret"},
		"github": {Granted: true, BaseURL: "https://api.github.com", Token: "ghp-secret"},
		"jira":   {Granted: true, BaseURL: "https://acme.atlassian.net", Token: "jira-secret"},
	}
	p := &actionProxy{taskID: "task-1", tools: []ActionTool{{Name: "jira"}, {Name: "gitlab"}, {Name: "covey"}}}
	p.brokerCredential = func(_ context.Context, system string) (InjectCredentials, error) {
		st.brokered = append(st.brokered, system)
		c, ok := creds[system]
		if !ok {
			return InjectCredentials{}, errors.New("no access")
		}
		return c, nil
	}
	p.decide = func(_ context.Context, _, action string, _ json.RawMessage) (ApprovalDecision, error) {
		st.subject = action
		if st.decision.Status == "" {
			return ApprovalDecision{Status: "approved"}, nil
		}
		return st.decision, nil
	}
	return p
}

func TestAskpassAnswersOnlyForTheAgentsOwnSystems(t *testing.T) {
	st := &askpassStubs{}
	p := askpassProxy(st)

	if got := p.askpass(context.Background(), "Username for 'https://gitlab.example.org': "); got.Value != askpassUser {
		t.Fatalf("username: %+v", got)
	}
	got := p.askpass(context.Background(), "Password for 'https://covey@gitlab.example.org': ")
	if got.Value != "glpat-secret" || st.subject != "gitlab:git_credential" {
		t.Fatalf("password: %+v via %q", got, st.subject)
	}

	// github is a system the run does not carry: no answer, and the refusal
	// names what was checked.
	got = p.askpass(context.Background(), "Password for 'https://github.com': ")
	if got.Value != "" || !strings.Contains(got.Error, "jira → acme.atlassian.net") {
		t.Fatalf("a host outside the agent's systems must be refused, naming the checked ones: %+v", got)
	}
	for _, b := range st.brokered {
		if b == "github" || b == "covey" {
			t.Fatalf("brokered a system the run does not carry: %v", st.brokered)
		}
	}
}

func TestAskpassFollowsTheGuardRails(t *testing.T) {
	for _, dec := range []ApprovalDecision{{Status: "denied", Reason: "not here"}, {Status: "pending", CorrelationKey: "k1"}} {
		st := &askpassStubs{decision: dec}
		got := askpassProxy(st).askpass(context.Background(), "Password for 'https://gitlab.example.org': ")
		if got.Value != "" || got.Error == "" {
			t.Fatalf("%s must not hand out the token: %+v", dec.Status, got)
		}
	}
}

// The whole route with a real git: it gets a 401, asks GIT_ASKPASS, the helper
// asks the proxy, and the retry carries the brokered token.
func TestGitClonesWithTheBrokeredToken(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
	var mu sync.Mutex
	var auth string
	forge := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if a := r.Header.Get("Authorization"); a != "" {
			mu.Lock()
			auth = a
			mu.Unlock()
			http.NotFound(w, r)
			return
		}
		w.Header().Set("WWW-Authenticate", `Basic realm="forge"`)
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer forge.Close()

	st := &askpassStubs{}
	p := askpassProxy(st)
	p.brokerCredential = func(context.Context, string) (InjectCredentials, error) {
		return InjectCredentials{Granted: true, BaseURL: forge.URL, Token: "glpat-secret"}, nil
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /askpass", p.handleAskpass)
	srv := &http.Server{Handler: mux}
	go srv.Serve(ln)
	defer srv.Close()

	dir := t.TempDir()
	exe, _ := os.Executable()
	helper := filepath.Join(dir, "askpass")
	os.WriteFile(helper, []byte("#!/bin/sh\nCOVEY_TEST_AS_ASKPASS=1 exec "+shellQuote(exe)+" askpass \"$1\"\n"), 0o755)

	cmd := exec.Command("git", "ls-remote", forge.URL+"/acme/lib.git")
	cmd.Env = append(os.Environ(), "GIT_ASKPASS="+helper, "GIT_TERMINAL_PROMPT=0",
		"COVEY_ACTION_PORT="+portOf(ln), "HOME="+dir, "GIT_CONFIG_NOSYSTEM=1")
	out, _ := cmd.CombinedOutput()

	mu.Lock()
	defer mu.Unlock()
	want := "Basic " + base64.StdEncoding.EncodeToString([]byte(askpassUser+":glpat-secret"))
	if auth != want {
		t.Fatalf("git did not retry with the brokered token (got %q):\n%s", auth, out)
	}
}

func portOf(ln net.Listener) string {
	_, port, _ := net.SplitHostPort(ln.Addr().String())
	return port
}
