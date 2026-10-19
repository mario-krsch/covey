package recipe

import (
	"strings"
	"testing"
)

const flat = `version: 1
name: acme-portal
services:
  - name: db
    image: postgres:16
    env: { POSTGRES_PASSWORD: test }
secrets:
  - acme_api_token
  - unused_key
env:
  write_to: .env
  base: .env.example
  vars:
    DB_HOST: db
    ACME_API_TOKEN: "{{ secret.acme_api_token }}"
commands:
  install: composer install
  migrate: php artisan migrate --force
start:
  api:
    cmd: php artisan serve --port 8000
    port: 8000
app:
  url: http://localhost:8000
  ready: "GET / 200"
mcp:
  marionette:
    command: marionette_mcp
baseline:
  ref: main
  known_failing: ["tests/LegacyTest.php::testOld"]
`

func TestParseFlat(t *testing.T) {
	r, err := Parse([]byte(flat))
	if err != nil {
		t.Fatal(err)
	}
	st, dir, err := r.Select("")
	if err != nil || dir != "." {
		t.Fatalf("select: %q %v", dir, err)
	}
	if len(st.Services) != 1 || st.Services[0].Env["POSTGRES_PASSWORD"] != "test" {
		t.Fatalf("services: %+v", st.Services)
	}
	if got := st.SecretNames(); len(got) != 1 || got[0] != "acme_api_token" {
		t.Fatalf("only referenced secrets are fetched: %v", got)
	}
	if st.Start["api"].Port != 8000 || st.App.Ready != "GET / 200" || st.Baseline.Ref != "main" {
		t.Fatalf("stack: %+v", st)
	}
	if _, _, err := r.Select("web"); err == nil {
		t.Fatal("a flat recipe must refuse a component")
	}
}

func TestParseComponents(t *testing.T) {
	r, err := Parse([]byte(`version: 1
components:
  backend:
    path: services/api
    compose: docker-compose.test.yml
    commands: { test: go test ./... }
  app:
    path: apps/mobile
    commands: { test: flutter test }
`))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := r.Select(""); err == nil || !strings.Contains(err.Error(), "app, backend") {
		t.Fatalf("without a component the error must list them: %v", err)
	}
	st, dir, err := r.Select("backend")
	if err != nil || dir != "services/api" || st.Compose != "docker-compose.test.yml" {
		t.Fatalf("select backend: %q %+v %v", dir, st, err)
	}
}

func TestParseRefuses(t *testing.T) {
	cases := map[string]string{
		"unknown key":          "version: 1\ncomand: { test: x }\n",
		"no version":           "commands: { test: x }\n",
		"wrong version":        "version: 2\n",
		"flat and components":  "version: 1\ncommands: { test: x }\ncomponents:\n  a: { path: a }\n",
		"compose and services": "version: 1\ncompose: dc.yml\nservices: [{name: db, image: postgres:16}]\n",
		"absolute path":        "version: 1\ncompose: /etc/dc.yml\n",
		"path out":             "version: 1\nenv: { write_to: ../.env }\n",
		"undeclared secret":    "version: 1\nenv: { write_to: .env, vars: { T: \"{{ secret.token }}\" } }\n",
		"secret value":         "version: 1\nsecrets: [\"token=abc def\"]\n",
		"no write_to":          "version: 1\nenv: { vars: { A: b } }\n",
		"start without cmd":    "version: 1\nstart: { api: { port: 80 } }\n",
		"bad start name":       "version: 1\nstart: { \"../x\": { cmd: x } }\n",
		"component no path":    "version: 1\ncomponents:\n  a: { commands: { test: x } }\n",
		"duplicate service":    "version: 1\nservices: [{name: db, image: a}, {name: db, image: b}]\n",
		"empty":                "",
	}
	for name, raw := range cases {
		if _, err := Parse([]byte(raw)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestResolveVars(t *testing.T) {
	got, err := ResolveVars(map[string]string{"A": "x-{{secret.k}}-{{ secret.k }}", "B": "plain"},
		map[string]string{"k": "v"})
	if err != nil || got["A"] != "x-v-v" || got["B"] != "plain" {
		t.Fatalf("%v %v", got, err)
	}
	if _, err := ResolveVars(map[string]string{"A": "{{ secret.gone }}"}, nil); err == nil ||
		!strings.Contains(err.Error(), "gone") {
		t.Fatalf("a missing value must be named: %v", err)
	}
}

func TestRenderDotenvKeepsTheProjectsFile(t *testing.T) {
	base := "APP_NAME=Acme\n# database\nDB_HOST=127.0.0.1\nexport DB_PORT=3306\nDB_HOST=again\n"
	got := string(RenderDotenv([]byte(base), map[string]string{
		"DB_HOST": "db", "DB_PORT": "5432", "TOKEN": "a b'c", "URL": "http://db:80/x",
	}))
	want := "APP_NAME=Acme\n# database\nDB_HOST=db\nDB_PORT=5432\n\n# covey.project.yml\nTOKEN=\"a b'c\"\nURL=http://db:80/x\n"
	if got != want {
		t.Fatalf("got:\n%s\nwant:\n%s", got, want)
	}
}

func TestQuoting(t *testing.T) {
	for v, want := range map[string]string{
		"plain":      "plain",
		"with space": "'with space'",
		"$dollar":    "'$dollar'",
		"it's":       `"it's"`,
		"two\nlines": `"two\nlines"`,
	} {
		if got := dotenvQuote(v); got != want {
			t.Errorf("dotenvQuote(%q) = %s, want %s", v, got, want)
		}
	}
	if got := ShellQuote("it's"); got != `'it'\''s'` {
		t.Errorf("ShellQuote: %s", got)
	}
}

func TestControlFlutterFindsTheVMService(t *testing.T) {
	re, err := ControlPattern(ControlFlutter)
	if err != nil {
		t.Fatal(err)
	}
	for line, want := range map[string]string{
		"Launching lib/main.dart on Linux in debug mode...\nA Dart VM Service on Linux is available at: http://127.0.0.1:41235/Qx9c=/\n": "http://127.0.0.1:41235/Qx9c=/",
		"An Observatory debugger and profiler on Linux is available at: http://127.0.0.1:8181/abc=/":                                     "http://127.0.0.1:8181/abc=/",
	} {
		m := re.FindStringSubmatch(line)
		if m == nil || m[1] != want {
			t.Errorf("%q: got %v", line, m)
		}
	}
}

func TestStageTwoValidation(t *testing.T) {
	for name, raw := range map[string]string{
		"bad ready":       "version: 1\napp: { url: http://localhost:80, ready: up }\n",
		"bad control":     "version: 1\nstart: { app: { cmd: x, control: \"(\" } }\n",
		"mcp named covey": "version: 1\nmcp: { covey: { command: x } }\n",
		"mcp bad name":    "version: 1\nmcp: { Marionette: { command: x } }\n",
	} {
		if _, err := Parse([]byte(raw)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	m, p, st, err := ParseReady("GET /health 204")
	if err != nil || m != "GET" || p != "/health" || st != 204 {
		t.Fatalf("ParseReady: %s %s %d %v", m, p, st, err)
	}
}
