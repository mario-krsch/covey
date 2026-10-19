package daemon

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const testRecipe = `version: 1
services:
  - name: db
    image: postgres:16
    env: { POSTGRES_PASSWORD: test }
secrets: [acme_token]
env:
  write_to: .env
  base: .env.example
  vars:
    DB_HOST: db
    ACME_TOKEN: "{{ secret.acme_token }}"
commands:
  install: npm ci
  migrate: npm run migrate
start:
  web:
    cmd: npm run dev
    port: 5173
app:
  url: http://localhost:5173
`

type envHarness struct {
	p        *actionProxy
	home     string
	checkout string
	asked    []RequestHiring
}

func newEnvHarness(t *testing.T, recipeText string, secrets map[string]InjectSecret) *envHarness {
	t.Helper()
	home := t.TempDir()
	checkout := filepath.Join(home, "repos", "acme")
	if err := os.MkdirAll(checkout, 0o755); err != nil {
		t.Fatal(err)
	}
	if recipeText != "" {
		if err := os.WriteFile(filepath.Join(checkout, "covey.project.yml"), []byte(recipeText), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	h := &envHarness{home: home, checkout: checkout}
	h.p = newTestProxy(secrets)
	h.p.client = &Client{homeDir: home}
	h.p.askHiring = func(_ context.Context, req RequestHiring) (InjectHiring, error) {
		h.asked = append(h.asked, req)
		data, _ := json.Marshal(map[string]any{"state": "ready", "started": []map[string]string{{"name": "db"}}})
		return InjectHiring{OK: true, Data: data}, nil
	}
	return h
}

func (h *envHarness) run(t *testing.T, action string, params map[string]string) map[string]any {
	t.Helper()
	raw, _ := json.Marshal(params)
	out, _ := json.Marshal(h.p.controlPlane(context.Background(), action, raw))
	var got map[string]any
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	return got
}

func TestEnvUpWritesTheEnvironment(t *testing.T) {
	h := newEnvHarness(t, testRecipe, map[string]InjectSecret{"acme_token": {Granted: true, Value: "tok en"}})
	if err := os.WriteFile(filepath.Join(h.checkout, ".env.example"), []byte("APP_NAME=Acme\nDB_HOST=127.0.0.1\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	got := h.run(t, "env_up", map[string]string{"checkout": "~/repos/acme"})
	if got["status"] != "ok" {
		t.Fatalf("env_up: %+v", got)
	}
	if len(h.asked) != 1 || h.asked[0].Op != "env_up" || len(h.asked[0].Services) != 1 ||
		h.asked[0].Services[0].Image != "postgres:16" {
		t.Fatalf("the recipe's services must go to the control plane: %+v", h.asked)
	}

	env, err := os.ReadFile(filepath.Join(h.checkout, ".env"))
	if err != nil {
		t.Fatal(err)
	}
	want := "APP_NAME=Acme\nDB_HOST=db\n\n# covey.project.yml\nACME_TOKEN='tok en'\n"
	if string(env) != want {
		t.Fatalf(".env:\n%s\nwant:\n%s", env, want)
	}
	if info, _ := os.Stat(filepath.Join(h.checkout, ".env")); info.Mode().Perm() != 0o600 {
		t.Fatalf(".env holds a secret and must be owner-only, is %v", info.Mode().Perm())
	}

	// The answer names the secret, never its value.
	raw, _ := json.Marshal(got)
	if strings.Contains(string(raw), "tok en") {
		t.Fatalf("a secret value reached the answer: %s", raw)
	}
	data := got["data"].(map[string]any)
	start := data["start"].(map[string]any)["web"].(map[string]any)["dev_start"].(map[string]any)
	if start["cwd"] != h.checkoutReal(t) || start["cmd"] != "npm run dev" {
		t.Fatalf("dev_start: %+v", start)
	}

	// And env_status can say it later, by name.
	st := h.run(t, "env_status", map[string]string{"checkout": h.checkout})
	rec := st["data"].(map[string]any)["env"].(map[string]any)
	if rec["written_to"] != filepath.Join(h.checkoutReal(t), ".env") || rec["secrets"].([]any)[0] != "acme_token" {
		t.Fatalf("env_status: %+v", st)
	}
}

func (h *envHarness) checkoutReal(t *testing.T) string {
	real, err := filepath.EvalSymlinks(h.checkout)
	if err != nil {
		t.Fatal(err)
	}
	return real
}

func TestEnvUpEnvironmentFormSourcesAFileOutsideTheCheckout(t *testing.T) {
	recipeText := strings.Replace(testRecipe, "write_to: .env\n  base: .env.example", "write_to: environment", 1)
	h := newEnvHarness(t, recipeText, map[string]InjectSecret{"acme_token": {Granted: true, Value: "it's"}})

	got := h.run(t, "env_up", map[string]string{"checkout": h.checkout})
	if got["status"] != "ok" {
		t.Fatalf("env_up: %+v", got)
	}
	if _, err := os.Stat(filepath.Join(h.checkout, ".env")); err == nil {
		t.Fatal("the environment form must not write into the checkout")
	}
	file := filepath.Join(h.home, ".covey", "env", "acme.env")
	content, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(content), `ACME_TOKEN='it'\''s'`) {
		t.Fatalf("shell quoting: %s", content)
	}
	start := got["data"].(map[string]any)["start"].(map[string]any)["web"].(map[string]any)["dev_start"].(map[string]any)
	if !strings.HasPrefix(start["cmd"].(string), "set -a && . ") || !strings.HasSuffix(start["cmd"].(string), "&& npm run dev") {
		t.Fatalf("the start command must source the file: %v", start["cmd"])
	}
}

func TestEnvUpFailsNamingAMissingSecret(t *testing.T) {
	h := newEnvHarness(t, testRecipe, nil)
	os.WriteFile(filepath.Join(h.checkout, ".env.example"), nil, 0o644)
	got := h.run(t, "env_up", map[string]string{"checkout": h.checkout})
	if got["status"] != "error" || !strings.Contains(got["error"].(string), `"acme_token"`) {
		t.Fatalf("a missing secret must fail env_up by name: %+v", got)
	}
	if _, err := os.Stat(filepath.Join(h.checkout, ".env")); err == nil {
		t.Fatal("nothing may be written when a secret is missing")
	}
}

func TestEnvUpRefusesWhatLeadsOut(t *testing.T) {
	t.Run("symlinked .env", func(t *testing.T) {
		h := newEnvHarness(t, strings.Replace(testRecipe, "  base: .env.example\n", "", 1),
			map[string]InjectSecret{"acme_token": {Granted: true, Value: "v"}})
		outside := filepath.Join(h.home, "bashrc")
		os.WriteFile(outside, []byte("keep\n"), 0o644)
		if err := os.Symlink(outside, filepath.Join(h.checkout, ".env")); err != nil {
			t.Fatal(err)
		}
		got := h.run(t, "env_up", map[string]string{"checkout": h.checkout})
		if got["status"] != "error" {
			t.Fatalf("writing through a symlink must be refused: %+v", got)
		}
		if b, _ := os.ReadFile(outside); string(b) != "keep\n" {
			t.Fatal("the file behind the symlink was changed")
		}
	})
	t.Run("checkout outside the home", func(t *testing.T) {
		h := newEnvHarness(t, testRecipe, nil)
		got := h.run(t, "env_up", map[string]string{"checkout": t.TempDir()})
		if got["status"] != "error" || !strings.Contains(got["error"].(string), "outside your home") {
			t.Fatalf("%+v", got)
		}
	})
	t.Run("no recipe", func(t *testing.T) {
		h := newEnvHarness(t, "", nil)
		got := h.run(t, "env_up", map[string]string{"checkout": h.checkout})
		if got["status"] != "error" || !strings.Contains(got["error"].(string), "covey.project.yml") {
			t.Fatalf("without a recipe the error must name the file: %+v", got)
		}
		if len(h.asked) != 0 {
			t.Fatal("nothing may be started without a recipe")
		}
	})
}

// The gate is the control plane's. Refused there, nothing is fetched or written.
func TestEnvUpWritesNothingWhenTheControlPlaneRefuses(t *testing.T) {
	fetched := false
	h := newEnvHarness(t, testRecipe, nil)
	h.p.fetchSecret = func(context.Context, string, string) (InjectSecret, error) {
		fetched = true
		return InjectSecret{Granted: true}, nil
	}
	h.p.askHiring = func(context.Context, RequestHiring) (InjectHiring, error) {
		return InjectHiring{Error: "this agent has no access"}, nil
	}
	got := h.run(t, "env_reset", map[string]string{"checkout": h.checkout})
	if got["status"] != "error" || fetched {
		t.Fatalf("refused: %+v, secret fetched: %v", got, fetched)
	}
}

func TestEnvUpSendsTheComposeFileTheRecipeNames(t *testing.T) {
	h := newEnvHarness(t, "version: 1\ncompose: docker-compose.test.yml\n", nil)
	os.WriteFile(filepath.Join(h.checkout, "docker-compose.test.yml"), []byte("services:\n  db:\n    image: postgres:16\n"), 0o644)
	if got := h.run(t, "env_up", map[string]string{"checkout": h.checkout}); got["status"] != "ok" {
		t.Fatalf("%+v", got)
	}
	if !strings.Contains(h.asked[0].Compose, "postgres:16") {
		t.Fatalf("the compose content must travel: %+v", h.asked[0])
	}
}

func TestEnvUpFallsBackToTheDevcontainersComposeFile(t *testing.T) {
	h := newEnvHarness(t, "", nil)
	os.MkdirAll(filepath.Join(h.checkout, ".devcontainer"), 0o755)
	os.WriteFile(filepath.Join(h.checkout, ".devcontainer", "devcontainer.json"), []byte(`{
  // the team's container
  "name": "acme // not a comment",
  "dockerComposeFile": "../docker-compose.yml", /* relative to this file */
  "forwardPorts": [8000,],
}`), 0o644)
	os.WriteFile(filepath.Join(h.checkout, "docker-compose.yml"), []byte("services:\n  db:\n    image: postgres:16\n"), 0o644)

	got := h.run(t, "env_up", map[string]string{"checkout": h.checkout})
	if got["status"] != "ok" || got["data"].(map[string]any)["from"] != "devcontainer" {
		t.Fatalf("a devcontainer's compose file must be used: %+v", got)
	}
	if !strings.Contains(h.asked[0].Compose, "postgres:16") {
		t.Fatalf("the compose content must travel: %+v", h.asked[0])
	}
}

func TestEnvStatusProbesTheAppAndReadsTheControlURI(t *testing.T) {
	app := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		http.NotFound(w, r)
	}))
	defer app.Close()
	recipeText := "version: 1\napp:\n  url: " + app.URL + "\n  ready: \"GET /health 204\"\n" +
		"start:\n  desktop:\n    cmd: flutter run -d linux\n    control: flutter\n"
	h := newEnvHarness(t, recipeText, nil)

	st := h.run(t, "env_status", map[string]string{"checkout": h.checkout})["data"].(map[string]any)
	if st["app"].(map[string]any)["ready"] != true {
		t.Fatalf("app.ready must pass: %+v", st["app"])
	}
	if c := st["control"].(map[string]any)["desktop"].(map[string]any); c["uri"] != nil {
		t.Fatalf("without a log there is no URI: %+v", c)
	}

	jobs := filepath.Join(h.home, ".covey", "jobs")
	os.MkdirAll(jobs, 0o755)
	os.WriteFile(filepath.Join(jobs, "desktop.log"), []byte(
		"A Dart VM Service on Linux is available at: http://127.0.0.1:1111/old=/\n"+
			"Restarted.\nA Dart VM Service on Linux is available at: http://127.0.0.1:2222/new=/\n"), 0o644)
	st = h.run(t, "env_status", map[string]string{"checkout": h.checkout})["data"].(map[string]any)
	if uri := st["control"].(map[string]any)["desktop"].(map[string]any)["uri"]; uri != "http://127.0.0.1:2222/new=/" {
		t.Fatalf("the latest VM service URI must be reported: %v", uri)
	}

	app.Close()
	st = h.run(t, "env_status", map[string]string{"checkout": h.checkout})["data"].(map[string]any)
	if st["app"].(map[string]any)["ready"] != false {
		t.Fatalf("a closed app must not pass: %+v", st["app"])
	}
}

const installRecipe = `version: 1
secrets: [npm_token]
install_env:
  NPM_TOKEN: "{{ secret.npm_token }}"
commands:
  install: echo "using $NPM_TOKEN"; echo "action port ${COVEY_ACTION_PORT:-none} daemon ${COVEY_DAEMON_TOKEN:-none}"; echo done
`

func waitInstall(t *testing.T, h *envHarness, want string) map[string]any {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		st := h.run(t, "env_status", map[string]string{"checkout": h.checkout})["data"].(map[string]any)
		if inst, ok := st["install"].(map[string]any); ok && inst["install"].(map[string]any)["state"] == want {
			return inst
		}
		if time.Now().After(deadline) {
			t.Fatalf("install never reached %q: %+v", want, st["install"])
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestEnvInstallGivesTheCredentialToTheInstallAlone(t *testing.T) {
	t.Setenv("COVEY_DAEMON_TOKEN", "daemon-secret")
	t.Setenv("COVEY_ACTION_PORT", "4711")
	h := newEnvHarness(t, installRecipe, map[string]InjectSecret{"npm_token": {Granted: true, Value: "npm-tok-123"}})

	got := h.run(t, "env_install", map[string]string{"checkout": h.checkout})
	if got["status"] != "ok" {
		t.Fatalf("env_install: %+v", got)
	}
	if len(h.asked) != 1 || h.asked[0].Op != "env_install" {
		t.Fatalf("the control plane must gate the install: %+v", h.asked)
	}
	inst := waitInstall(t, h, "succeeded")
	tail := inst["log_tail"].(string)
	if strings.Contains(tail, "npm-tok-123") || !strings.Contains(tail, "using [secret]") {
		t.Fatalf("the secret value must be masked in the log:\n%s", tail)
	}
	if !strings.Contains(tail, "action port 4711 daemon none") {
		t.Fatalf("the install gets the action port and not the daemon's credentials:\n%s", tail)
	}
	// Nothing with the value is left in the home.
	filepath.Walk(h.home, func(path string, info os.FileInfo, err error) error {
		if err == nil && info.Mode().IsRegular() {
			if b, _ := os.ReadFile(path); strings.Contains(string(b), "npm-tok-123") {
				t.Errorf("the credential was written to %s", path)
			}
		}
		return nil
	})
}

func TestEnvInstallRunsOnceAtATimeAndReportsFailure(t *testing.T) {
	h := newEnvHarness(t, "version: 1\ncommands:\n  install: sleep 0.3; exit 3\n", nil)
	first := h.run(t, "env_install", map[string]string{"checkout": h.checkout})
	second := h.run(t, "env_install", map[string]string{"checkout": h.checkout})
	if first["status"] != "ok" || second["data"].(map[string]any)["note"] == nil {
		t.Fatalf("a second install while one runs must report the running one: %+v", second)
	}
	inst := waitInstall(t, h, "failed")["install"].(map[string]any)
	if inst["exit_code"].(float64) != 3 {
		t.Fatalf("exit code: %+v", inst)
	}
}

func TestEnvInstallFetchesNothingWhenRefused(t *testing.T) {
	fetched := false
	h := newEnvHarness(t, installRecipe, nil)
	h.p.fetchSecret = func(context.Context, string, string) (InjectSecret, error) {
		fetched = true
		return InjectSecret{Granted: true}, nil
	}
	h.p.askHiring = func(context.Context, RequestHiring) (InjectHiring, error) {
		return InjectHiring{Error: "no access"}, nil
	}
	if got := h.run(t, "env_install", map[string]string{"checkout": h.checkout}); got["status"] != "error" || fetched {
		t.Fatalf("refused: %+v, fetched %v", got, fetched)
	}
}
