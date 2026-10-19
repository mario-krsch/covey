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
