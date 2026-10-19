package orchestrator

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"covey/internal/agents"
	"covey/internal/daemon"
	"covey/internal/sandbox"
)

// slowStarter stands in for a host that is pulling an image: StartServices
// returns only when release is closed.
type slowStarter struct {
	fakeSandbox
	release chan struct{}
	calls   atomic.Int32
	err     error
}

func (s *slowStarter) StartServices(ctx context.Context, services []sandbox.Service) ([]sandbox.ServiceRun, error) {
	s.calls.Add(1)
	select {
	case <-s.release:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	if s.err != nil {
		return nil, s.err
	}
	runs := make([]sandbox.ServiceRun, 0, len(services))
	for _, svc := range services {
		runs = append(runs, sandbox.ServiceRun{Name: svc.Name, Image: svc.Image})
	}
	return runs, nil
}

const dbCompose = "services:\n  db:\n    image: postgres:16\n"

func serviceHarness(t *testing.T, sb Sandbox) (*Orchestrator, agents.Agent, func(op string) map[string]any) {
	t.Helper()
	agent := agents.Agent{ID: uuid.New(), OrgID: uuid.New()}
	o := &Orchestrator{sessions: map[uuid.UUID]*session{agent.ID: {sandbox: sb}}}
	call := func(op string) map[string]any {
		t.Helper()
		ok := func(v any) daemon.InjectHiring {
			raw, _ := json.Marshal(v)
			return daemon.InjectHiring{OK: true, Data: raw}
		}
		fail := func(format string, args ...any) daemon.InjectHiring {
			return daemon.InjectHiring{Error: fmt.Sprintf(format, args...)}
		}
		var resp daemon.InjectHiring
		switch op {
		case "start_services":
			resp = o.startServices(context.Background(), agent, uuid.New(),
				daemon.RequestHiring{Op: op, Compose: dbCompose}, ok, fail)
		case "services_status":
			resp = o.servicesStatus(agent, ok, fail)
		}
		if !resp.OK {
			return map[string]any{"error": resp.Error}
		}
		var out map[string]any
		if err := json.Unmarshal(resp.Data, &out); err != nil {
			t.Fatal(err)
		}
		return out
	}
	return o, agent, call
}

func shortServiceWait(t *testing.T) {
	t.Helper()
	old := serviceAnswerWait
	serviceAnswerWait = 50 * time.Millisecond
	t.Cleanup(func() { serviceAnswerWait = old })
}

func waitState(t *testing.T, call func(string) map[string]any, want string) map[string]any {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		st := call("services_status")
		if st["state"] == want {
			return st
		}
		if time.Now().After(deadline) {
			t.Fatalf("services_status never reached %q: %+v", want, st)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// A pull longer than the daemon's deadline must not turn into an error while
// the services come up anyway (#603): the agent gets "provisioning" in time,
// a retry does not pull a second time, and services_status reports the result.
func TestStartServicesAnswersProvisioningOnASlowHost(t *testing.T) {
	shortServiceWait(t)
	sb := &slowStarter{release: make(chan struct{})}
	o, agent, call := serviceHarness(t, sb)

	first := call("start_services")
	if first["state"] != "provisioning" || fmt.Sprint(first["pending"]) != "[db]" {
		t.Fatalf("a slow host must answer provisioning naming what is pending: %+v", first)
	}
	if call("services_status")["state"] != "provisioning" {
		t.Fatal("services_status must report the start that is still running")
	}

	again := call("start_services")
	if again["state"] != "provisioning" {
		t.Fatalf("a retry while provisioning must report the running start: %+v", again)
	}
	if n := sb.calls.Load(); n != 1 {
		t.Fatalf("a retry while provisioning started the services again (%d calls)", n)
	}

	close(sb.release)
	st := waitState(t, call, "ready")
	if fmt.Sprint(st["services"]) != "[map[image:postgres:16 name:db]]" {
		t.Fatalf("services_status must list what came up: %+v", st)
	}
	o.mu.Lock()
	n := len(o.sessions[agent.ID].services)
	o.mu.Unlock()
	if n != 1 {
		t.Fatalf("the session must carry the started service, has %d", n)
	}
}

// The common case, a warm image, still answers in one call.
func TestStartServicesAnswersReadyWhenTheHostIsQuick(t *testing.T) {
	sb := &slowStarter{release: make(chan struct{})}
	close(sb.release)
	_, _, call := serviceHarness(t, sb)

	got := call("start_services")
	if got["state"] != "ready" || fmt.Sprint(got["started"]) != "[map[image:postgres:16 name:db]]" {
		t.Fatalf("a quick host must answer ready with what started: %+v", got)
	}
}

// A start that fails after the answer went out is reported, not lost.
func TestServicesStatusReportsALateFailure(t *testing.T) {
	shortServiceWait(t)
	sb := &slowStarter{release: make(chan struct{}), err: errors.New("pull access denied")}
	_, _, call := serviceHarness(t, sb)

	if call("start_services")["state"] != "provisioning" {
		t.Fatal("expected provisioning")
	}
	close(sb.release)
	st := waitState(t, call, "failed")
	if st["error"] != "pull access denied" {
		t.Fatalf("the failure must carry the host's error: %+v", st)
	}

	// After a failure the agent may try again.
	sb.err = nil
	if got := call("start_services"); got["state"] != "ready" {
		t.Fatalf("a new start after a failure must run: %+v", got)
	}
}

func TestServicesStatusWithoutASandbox(t *testing.T) {
	o := &Orchestrator{sessions: map[uuid.UUID]*session{}}
	resp := o.servicesStatus(agents.Agent{ID: uuid.New()},
		func(any) daemon.InjectHiring { return daemon.InjectHiring{OK: true} },
		func(string, ...any) daemon.InjectHiring { return daemon.InjectHiring{} })
	if resp.OK {
		t.Fatal("services_status without a running sandbox must fail")
	}
}

func recipeCall(t *testing.T, o *Orchestrator, agent agents.Agent, reset bool, decl ...daemon.ServiceDecl) map[string]any {
	t.Helper()
	ok := func(v any) daemon.InjectHiring {
		raw, _ := json.Marshal(v)
		return daemon.InjectHiring{OK: true, Data: raw}
	}
	fail := func(format string, args ...any) daemon.InjectHiring {
		return daemon.InjectHiring{Error: fmt.Sprintf(format, args...)}
	}
	resp := o.recipeServices(context.Background(), agent, uuid.New(),
		daemon.RequestHiring{Op: "env_up", Services: decl}, reset, ok, fail)
	if !resp.OK {
		return map[string]any{"error": resp.Error}
	}
	var out map[string]any
	_ = json.Unmarshal(resp.Data, &out)
	return out
}

// env_up on a warm sandbox leaves the database alone; env_reset brings it back
// empty. Both keep one entry per name (spec/30).
func TestRecipeServicesKeepOnUpAndRecreateOnReset(t *testing.T) {
	sb := &slowStarter{release: make(chan struct{})}
	close(sb.release)
	o, agent, _ := serviceHarness(t, sb)
	db := daemon.ServiceDecl{Name: "db", Image: "postgres:16"}

	if got := recipeCall(t, o, agent, false, db); got["state"] != "ready" || sb.calls.Load() != 1 {
		t.Fatalf("first env_up must start the service: %+v", got)
	}
	got := recipeCall(t, o, agent, false, db)
	if sb.calls.Load() != 1 || fmt.Sprint(got["kept"]) != "[db]" {
		t.Fatalf("a second env_up must keep what runs: calls=%d %+v", sb.calls.Load(), got)
	}
	recipeCall(t, o, agent, true, db)
	if sb.calls.Load() != 2 {
		t.Fatal("env_reset must recreate the service")
	}
	// Another image under the same name is not the same service.
	recipeCall(t, o, agent, false, daemon.ServiceDecl{Name: "db", Image: "postgres:17"})
	if sb.calls.Load() != 3 {
		t.Fatal("env_up with a changed image must start it")
	}
	o.mu.Lock()
	running := o.sessions[agent.ID].services
	o.mu.Unlock()
	if len(running) != 1 || running[0].Image != "postgres:17" {
		t.Fatalf("one entry per name, the latest: %+v", running)
	}
}

func TestRecipeServicesAreValidated(t *testing.T) {
	sb := &slowStarter{release: make(chan struct{})}
	close(sb.release)
	o, agent, _ := serviceHarness(t, sb)
	got := recipeCall(t, o, agent, false, daemon.ServiceDecl{Name: "Not A Host", Image: "postgres:16"})
	if got["error"] == nil || sb.calls.Load() != 0 {
		t.Fatalf("an invalid service name must be refused before anything starts: %+v", got)
	}
}
