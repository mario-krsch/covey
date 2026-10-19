package integration

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"covey/internal/backlog"
	"covey/internal/observability"
	"covey/internal/orchestrator"
)

/* The project brings its own recipe (spec/30, #604).

   The whole path: the daemon reads covey.project.yml out of the agent's
   checkout, the control plane checks scope and allowlist and has the host start
   the services, the broker answers the secret the recipe names, and the daemon
   writes the application's environment into the checkout. */

const projectRecipe = `version: 1
services:
  - name: db
    image: postgres:16
    env: { POSTGRES_PASSWORD: test }
secrets: [acme_token]
env:
  write_to: .env
  vars:
    DB_HOST: db
    ACME_TOKEN: "{{ secret.acme_token }}"
commands:
  migrate: ./migrate.sh
`

func TestTheRecipeSetsTheProjectUp(t *testing.T) {
	ctx := context.Background()
	s := newStackWith(t, stackOpts{
		provider: func(homeBase string, log *slog.Logger) orchestrator.SandboxProvider {
			return &addingProvider{inner: &inprocProvider{homeBase: homeBase, log: log}}
		},
	})

	agent := s.newSupportAgent("qa-with-a-recipe")
	if _, err := s.registry.SaveConfig(ctx, agent.ID, map[string]string{
		"SOUL.md":   "# QA\n\n## Rolle\nNimmt ab.",
		"ACCESS.md": "- system: covey scope: services:write",
	}, &s.adminID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.workplaces.AddServicePattern(ctx, s.orgID, "postgres:*", ""); err != nil {
		t.Fatal(err)
	}
	if err := s.secrets.PutAgent(ctx, s.orgID, agent.ID, "acme_token", "tok-123"); err != nil {
		t.Fatal(err)
	}
	checkout := filepath.Join(s.homeBase, agent.ID.String(), "repos", "acme")
	if err := os.MkdirAll(checkout, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(checkout, "covey.project.yml"), []byte(projectRecipe), 0o644); err != nil {
		t.Fatal(err)
	}

	task, err := s.backlog.Create(ctx, s.orgID, agent.ID, "Projekt aufsetzen",
		`[mock:action covey/env_up {"checkout":"~/repos/acme"}]`, "manual", 3)
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, "task done", 40*time.Second, func() bool {
		return s.taskState(task.ID) == backlog.StateDone
	})

	env, err := os.ReadFile(filepath.Join(checkout, ".env"))
	if err != nil {
		t.Fatalf("the environment was not written: %v", err)
	}
	if !strings.Contains(string(env), "ACME_TOKEN=tok-123") || !strings.Contains(string(env), "DB_HOST=db") {
		t.Fatalf(".env:\n%s", env)
	}

	events, err := s.obs.Events(ctx, agent.ID, &task.ID, 0, 500)
	if err != nil {
		t.Fatal(err)
	}
	var service, action string
	for _, e := range events {
		switch {
		case e.Kind == observability.KindService && strings.Contains(string(e.Payload), `"recipe"`):
			service = string(e.Payload)
		case e.Kind == observability.KindAction && strings.Contains(string(e.Payload), "covey:env_up"):
			action = string(e.Payload)
		}
	}
	if service == "" || !strings.Contains(service, "postgres:16") {
		t.Fatalf("the recipe's service was not started and recorded: %q", service)
	}
	// The recording names the secret; its value appears nowhere in it.
	for _, e := range events {
		if strings.Contains(string(e.Payload), "tok-123") {
			t.Fatalf("the secret value reached the recording: %s", e.Payload)
		}
	}
	var audit struct {
		Secrets []string `json:"secrets"`
	}
	if err := json.Unmarshal([]byte(action), &audit); err != nil || len(audit.Secrets) != 1 || audit.Secrets[0] != "acme_token" {
		t.Fatalf("the action must be recorded with the secret's name: %q", action)
	}
}

// The recipe's MCP servers start from the next run on, and only with the dev
// scope mcp; either way the recording says what happened (spec/30, #605).
func TestRecipeMCPServersFollowTheGrant(t *testing.T) {
	for _, tc := range []struct {
		access  string
		started bool
	}{
		{"- system: covey scope: services:write\n- system: dev scope: exec,mcp", true},
		{"- system: covey scope: services:write\n- system: dev scope: exec", false},
	} {
		ctx := context.Background()
		s := newStack(t)
		agent := s.newSupportAgent("qa-mcp")
		if _, err := s.registry.SaveConfig(ctx, agent.ID, map[string]string{
			"SOUL.md": "# QA\n\n## Rolle\nNimmt ab.", "ACCESS.md": tc.access,
		}, &s.adminID); err != nil {
			t.Fatal(err)
		}
		checkout := filepath.Join(s.homeBase, agent.ID.String(), "repos", "app")
		os.MkdirAll(checkout, 0o755)
		// `sh` stands in for a server binary: it is on every PATH.
		os.WriteFile(filepath.Join(checkout, "covey.project.yml"),
			[]byte("version: 1\nmcp:\n  probe:\n    command: sh\n"), 0o644)

		first, _ := s.backlog.Create(ctx, s.orgID, agent.ID, "aufsetzen",
			`[mock:action covey/env_up {"checkout":"~/repos/app"}]`, "manual", 3)
		waitFor(t, "env_up done", 40*time.Second, func() bool { return s.taskState(first.ID) == backlog.StateDone })
		second, _ := s.backlog.Create(ctx, s.orgID, agent.ID, "testen", "[mock:prompt]", "manual", 3)
		waitFor(t, "next run done", 40*time.Second, func() bool { return s.taskState(second.ID) == backlog.StateDone })

		events, err := s.obs.Events(ctx, agent.ID, &second.ID, 0, 500)
		if err != nil {
			t.Fatal(err)
		}
		var payload string
		for _, e := range events {
			if strings.Contains(string(e.Payload), "recipe_mcp") {
				payload = string(e.Payload)
			}
		}
		var got struct {
			Started []struct{ Name string }   `json:"started"`
			Left    []struct{ Reason string } `json:"left_out"`
		}
		if err := json.Unmarshal([]byte(payload), &got); err != nil {
			t.Fatalf("%s: the run did not record its recipe servers: %q", tc.access, payload)
		}
		if tc.started && (len(got.Started) != 1 || got.Started[0].Name != "probe") {
			t.Fatalf("with the grant the server must start: %s", payload)
		}
		if !tc.started && (len(got.Started) != 0 || len(got.Left) != 1 || !strings.Contains(got.Left[0].Reason, "mcp")) {
			t.Fatalf("without the grant the server must be left out, named: %s", payload)
		}
	}
}

// The install gets the registry credential, and only the install (#606).
func TestEnvInstallCarriesTheRegistryCredential(t *testing.T) {
	ctx := context.Background()
	s := newStack(t)
	agent := s.newSupportAgent("qa-install")
	if _, err := s.registry.SaveConfig(ctx, agent.ID, map[string]string{
		"SOUL.md": "# QA\n\n## Rolle\nNimmt ab.", "ACCESS.md": "- system: covey scope: services:write",
	}, &s.adminID); err != nil {
		t.Fatal(err)
	}
	if err := s.secrets.PutAgent(ctx, s.orgID, agent.ID, "npm_token", "npm-tok-789"); err != nil {
		t.Fatal(err)
	}
	home := filepath.Join(s.homeBase, agent.ID.String())
	checkout := filepath.Join(home, "repos", "web")
	os.MkdirAll(checkout, 0o755)
	os.WriteFile(filepath.Join(checkout, "covey.project.yml"), []byte(`version: 1
secrets: [npm_token]
install_env: { NPM_TOKEN: "{{ secret.npm_token }}" }
commands:
  install: test -n "$NPM_TOKEN" && echo "token $NPM_TOKEN" > /dev/null && echo installed
`), 0o644)

	task, _ := s.backlog.Create(ctx, s.orgID, agent.ID, "installieren",
		`[mock:action covey/env_install {"checkout":"~/repos/web"}]`, "manual", 3)
	waitFor(t, "task done", 40*time.Second, func() bool { return s.taskState(task.ID) == backlog.StateDone })

	var status map[string]any
	waitFor(t, "install finished", 20*time.Second, func() bool {
		raw, err := os.ReadFile(filepath.Join(home, ".covey", "env", "web.install.json"))
		if err != nil || json.Unmarshal(raw, &status) != nil {
			return false
		}
		return status["state"] != "running"
	})
	if status["state"] != "succeeded" {
		t.Fatalf("the install did not get its credential: %+v", status)
	}
	events, _ := s.obs.Events(ctx, agent.ID, &task.ID, 0, 500)
	for _, e := range events {
		if strings.Contains(string(e.Payload), "npm-tok-789") {
			t.Fatalf("the credential reached the recording: %s", e.Payload)
		}
	}
}
