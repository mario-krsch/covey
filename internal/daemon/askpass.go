package daemon

// Credentials for git, without a token on the disk (spec/30, stage 3; #606).
//
// A project's dependencies often come from a private repository: a git:
// dependency in pubspec.yaml, package.json or composer.json that points into a
// company GitLab. In a sandbox git has neither a credential nor a terminal, and
// the clone ends at "could not read Username". Writing the token into
// ~/.gitconfig would hand it to everything the agent runs, the install scripts
// of the repository under test included.
//
// So the route git itself provides: GIT_ASKPASS. git asks a program, the program
// asks the daemon over the action port, and the daemon brokers per call:
//
//   - only among the systems this agent may use anyway, matched by the host of
//     their base URL — it makes an existing access usable for a second tool and
//     grants no new one;
//   - through the guard rails, as `<system>:git_credential`, so an organisation
//     can gate or forbid it like any action;
//   - recorded, the refusals included, because the refusal is what a run stops
//     on.
//
// What this does not protect against is said plainly: the answer reaches the
// process that asked. Code in the repository under test that calls the helper
// itself gets the token, as it would get any credential its process holds. The
// recording shows every such call.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/benjaminLedel/covey-plugin-sdk/target"
)

type askpassRequest struct {
	Prompt string `json:"prompt"`
}

type askpassResponse struct {
	Value string `json:"value,omitempty"`
	Error string `json:"error,omitempty"`
}

// askpassUser is the user name handed to git next to a token. GitLab, GitHub
// and Gitea accept any non-empty name with a personal access token; a fixed one
// keeps the token out of the URL git echoes into its messages.
const askpassUser = "covey"

func (p *actionProxy) handleAskpass(w http.ResponseWriter, r *http.Request) {
	var in askpassRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&in); err != nil {
		writeJSON(w, askpassResponse{Error: "unreadable request"})
		return
	}
	writeJSON(w, p.askpass(r.Context(), in.Prompt))
}

func (p *actionProxy) askpass(ctx context.Context, prompt string) askpassResponse {
	host, wantUser := parseAskpassPrompt(prompt)
	if host == "" {
		// git uses the same helper for an SSH passphrase and the like; a
		// question that is not understood gets no answer rather than a guess.
		return askpassResponse{Error: "not a question for a repository credential"}
	}
	system, cred, checked, err := p.credentialForHost(ctx, host)
	if err != nil {
		p.recordAskpass(system, host, false, err.Error())
		return askpassResponse{Error: err.Error()}
	}
	if system == "" {
		reason := refusalReason(host, checked)
		p.recordAskpass("", host, false, reason)
		return askpassResponse{Error: reason}
	}
	params, _ := json.Marshal(map[string]string{"host": host})
	dec, err := p.decide(ctx, p.taskID, system+":git_credential", params)
	switch {
	case err != nil:
		p.recordAskpass(system, host, false, err.Error())
		return askpassResponse{Error: err.Error()}
	case dec.Status == "denied":
		p.recordAskpass(system, host, false, "denied: "+dec.Reason)
		return askpassResponse{Error: "denied by a guard rail on " + system + ":git_credential: " + dec.Reason}
	case dec.Status == "pending":
		// A clone cannot wait for a human. The approval stays; the agent
		// repeats the install once it is granted.
		p.recordAskpass(system, host, false, "approval pending")
		return askpassResponse{Error: system + ":git_credential needs an approval (" + dec.CorrelationKey + "); repeat the install once it is granted"}
	}
	if wantUser {
		return askpassResponse{Value: askpassUser}
	}
	p.recordAskpass(system, host, true, "")
	return askpassResponse{Value: cred.Token}
}

// refusalReason names the hosts that were checked, not only the one wanted. A
// system pointing at the wrong instance is one line of configuration, and this
// list is what makes it visible from the run.
func refusalReason(host string, checked []string) string {
	if len(checked) == 0 {
		return "no access to " + host + " — this agent has no target system with a credential. " +
			"A dependency from a private host needs that host's system in its ACCESS.md."
	}
	return "no access to " + host + " — none of this agent's target systems live on that host " +
		"(checked: " + strings.Join(checked, ", ") + "). Either the dependency comes from somewhere " +
		"the agent may not reach, or a system points at another instance."
}

func (p *actionProxy) recordAskpass(system, host string, granted bool, reason string) {
	if system == "" {
		system = "git"
	}
	audit := map[string]any{"action": system + ":git_credential", "params": map[string]string{"host": host}, "ok": granted}
	if reason != "" {
		audit["reason"] = reason
	}
	payload, _ := json.Marshal(audit)
	if p.client != nil {
		_ = p.client.send(TypeEvent, Event{TaskID: p.taskID, Kind: "action", Payload: payload})
	}
}

// parseAskpassPrompt reads git's two questions:
//
//	Username for 'https://gitlab.example.org':
//	Password for 'https://covey@gitlab.example.org':
//
// Anything else is not answered.
func parseAskpassPrompt(prompt string) (host string, wantUser bool) {
	s := strings.TrimSpace(prompt)
	switch {
	case strings.HasPrefix(s, "Username for '"):
		wantUser = true
	case strings.HasPrefix(s, "Password for '"):
	default:
		return "", false
	}
	i, j := strings.Index(s, "'"), strings.LastIndex(s, "'")
	if i < 0 || j <= i {
		return "", false
	}
	u, err := url.Parse(s[i+1 : j])
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") {
		return "", false
	}
	return strings.ToLower(u.Hostname()), wantUser
}

// credentialForHost finds the agent's target system whose base URL lives on the
// host and brokers its credential. Only the systems the control plane put into
// this run's tool list are considered. GitHub's API host api.github.com stands
// for github.com.
func (p *actionProxy) credentialForHost(ctx context.Context, host string) (string, InjectCredentials, []string, error) {
	var checked []string
	seen := map[string]bool{}
	for _, tool := range p.tools {
		name := tool.Name
		if name == "" || name == "covey" || seen[name] {
			continue
		}
		seen[name] = true
		if d, ok := target.Describe(name); ok && d.NoCredentials {
			continue
		}
		cred, err := p.brokerCredential(ctx, name)
		if err != nil || !cred.Granted || cred.BaseURL == "" || cred.Token == "" {
			continue
		}
		u, err := url.Parse(cred.BaseURL)
		if err != nil {
			continue
		}
		have := strings.ToLower(u.Hostname())
		if have == host || (have == "api.github.com" && host == "github.com") {
			return name, cred, checked, nil
		}
		checked = append(checked, name+" → "+have)
	}
	return "", InjectCredentials{}, checked, nil
}

// --- the helper side: coveyd askpass "<prompt>", called by git ---------------

// Askpass answers git's question on stdout. It prints nothing when there is no
// answer: git then fails with its own message instead of trying an empty
// password and reporting an authentication error that leads nowhere.
func Askpass(prompt string) int {
	port := os.Getenv("COVEY_ACTION_PORT")
	if port == "" {
		fmt.Fprintln(os.Stderr, "covey askpass: no action port — run this inside a covey sandbox run")
		return 1
	}
	body, _ := json.Marshal(askpassRequest{Prompt: prompt})
	resp, err := (&http.Client{Timeout: 45 * time.Second}).Post(
		"http://127.0.0.1:"+port+"/askpass", "application/json", bytes.NewReader(body))
	if err != nil {
		fmt.Fprintln(os.Stderr, "covey askpass: "+err.Error())
		return 1
	}
	defer resp.Body.Close()
	var out askpassResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil || out.Value == "" {
		if out.Error != "" {
			fmt.Fprintln(os.Stderr, "covey askpass: "+out.Error)
		}
		return 1
	}
	fmt.Println(out.Value)
	return 0
}

// askpassHelper writes the script git calls and returns its path. A script and
// not the binary, because git passes the prompt as the program's only argument
// and `coveyd askpass` is a subcommand. In /tmp, not in the home: it belongs to
// this sandbox's daemon binary, and the home outlives the image.
func askpassHelper() string {
	exe, err := os.Executable()
	if err != nil {
		return ""
	}
	path := "/tmp/covey-askpass"
	script := "#!/bin/sh\nexec " + shellQuote(exe) + " askpass \"$1\"\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		return ""
	}
	return path
}

func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }
