package orchestrator

import (
	"context"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"

	"covey/internal/agents"
	"covey/internal/daemon"
	"covey/internal/observability"
	"covey/internal/sandbox"
)

// The agent brings up the services its project needs (spec/16, #121).
//
// This is the half that makes the mechanism usable rather than merely correct.
// A declaration typed by a manager assumes somebody knew, before the run, which
// database this project wants. For a QA agent accepting merge requests across
// several projects that assumption is wrong on the second project — and the
// answer has been in the repository all along, in its own `docker-compose.yml`.
//
// So the agent reads that file (it is in the checkout it has just made, on the
// machine only it can see) and sends the CONTENT. The control plane parses the
// subset that may run beside a sandbox, asks the organisation's allowlist, and
// tells the host holding this sandbox to bring up what is left.
//
// Three lines are drawn deliberately:
//
//   - The file is READ, never executed. `docker compose up` on a runner would
//     mean the project decides what runs on somebody else's machine.
//   - The agent does not choose IMAGES, it chooses among images the
//     organisation has allowed. That is what makes the whole thing safe to
//     hand over: the privileged act is extending the allowlist, not naming a
//     reference.
//   - Partial is normal here, and it is the difference from the declared set at
//     the wake. A compose file legitimately contains services that must not run
//     beside a sandbox — the project's own application above all, which the
//     agent builds INSIDE its sandbox. Refusing the file because of it would
//     make the mechanism useless for the files it exists for. So it starts what
//     it may and reports the rest, service by service, with the reason.

// serviceResult is what the agent gets back. Three lists, because the three
// mean different things to it: what it may now talk to, what this file does not
// offer it, and what its organisation does not allow. Only the third is
// somebody else's decision to change.
//
// State is "ready" when the host has answered, and "provisioning" when it has
// not answered within serviceAnswerWait (#603). Pending then names what is
// still coming up, and services_status reports the outcome.
type serviceResult struct {
	State   string               `json:"state"`
	Started []sandbox.ServiceRun `json:"started"`
	Pending []string             `json:"pending,omitempty"`
	// Kept are services that already ran under the same name and image and
	// were left alone (env_up on a warm sandbox).
	Kept    []string              `json:"kept,omitempty"`
	Skipped []sandbox.ComposeSkip `json:"skipped,omitempty"`
	Refused []serviceRefusal      `json:"refused,omitempty"`
	Hint    string                `json:"hint,omitempty"`
}

// serviceAnswerWait is how long start_services waits for the host before it
// answers "provisioning". The daemon gives every request to the control plane
// 30 seconds (internal/daemon/daemon.go), and a cold image pull takes longer
// than that; an answer after the deadline reaches nobody, while the services
// come up all the same (#603). So the start goes on in the background and the
// agent asks again. The wait is also how long the job's message loop stands
// still for this request — bounded, and well below the deadline.
var serviceAnswerWait = 20 * time.Second

// provisioningHint is what the agent reads with a "provisioning" answer.
const provisioningHint = "the host is still bringing these services up (an image pull can take minutes). " +
	"Ask with covey/services_status until state is ready or failed; do not send the compose file again."

// serviceProvision is one start_services that went to the host. It outlives the
// request that started it: the host answers whenever the pull is done, and the
// result is kept on the session for services_status.
type serviceProvision struct {
	since   time.Time
	pending []string
	done    chan struct{}

	// Set before done is closed, read only after.
	started []sandbox.ServiceRun
	err     error
}

func (p *serviceProvision) finished() bool {
	select {
	case <-p.done:
		return true
	default:
		return false
	}
}

type serviceRefusal struct {
	Name   string `json:"name"`
	Image  string `json:"image"`
	Reason string `json:"reason"`
}

// startServices serves covey/start_services.
func (o *Orchestrator) startServices(
	ctx context.Context, agent agents.Agent, taskID uuid.UUID, req daemon.RequestHiring,
	ok func(any) daemon.InjectHiring, fail func(string, ...any) daemon.InjectHiring,
) daemon.InjectHiring {
	raw := strings.TrimSpace(req.Compose)
	if raw == "" {
		return fail("send the content of the project's docker-compose.yml as `compose` — read it out of your checkout")
	}
	parsed, err := sandbox.ParseCompose([]byte(raw))
	if err != nil {
		return fail("%v", err)
	}

	wanted := parsed.Services
	skipped := parsed.Skipped
	// An agent that only wants the database says so. The ones it did not ask
	// for are reported as skipped rather than silently dropped — otherwise a
	// typo in the filter looks like a compose file that does not contain what
	// it plainly contains.
	if len(req.Only) > 0 {
		var keep []sandbox.Service
		for _, svc := range wanted {
			if slices.Contains(req.Only, svc.Name) {
				keep = append(keep, svc)
				continue
			}
			skipped = append(skipped, sandbox.ComposeSkip{
				Name: svc.Name, Reason: "you did not ask for it (`only`)",
			})
		}
		wanted = keep
	}

	return o.launchServices(ctx, agent, taskID, "compose", wanted, skipped, false, ok, fail)
}

// recipeServices serves covey/env_up and covey/env_reset (spec/30): the
// services of a project recipe, which the daemon read out of the checkout.
// Either the content of the compose file the recipe names, or the services it
// declares itself.
//
// env_up keeps what already runs under the same name and image. That is what a
// warm sandbox is for, and bringing a database up again would empty it.
// env_reset recreates them: the runner replaces a container asked for under a
// name it already has, and a service keeps no volume, so it comes back empty.
func (o *Orchestrator) recipeServices(
	ctx context.Context, agent agents.Agent, taskID uuid.UUID, req daemon.RequestHiring, reset bool,
	ok func(any) daemon.InjectHiring, fail func(string, ...any) daemon.InjectHiring,
) daemon.InjectHiring {
	var wanted []sandbox.Service
	var skipped []sandbox.ComposeSkip
	if raw := strings.TrimSpace(req.Compose); raw != "" {
		parsed, err := sandbox.ParseCompose([]byte(raw))
		if err != nil {
			return fail("%v", err)
		}
		wanted, skipped = parsed.Services, parsed.Skipped
	} else if len(req.Services) > 0 {
		decl := make([]sandbox.Service, 0, len(req.Services))
		for _, d := range req.Services {
			decl = append(decl, sandbox.Service{Name: d.Name, Image: d.Image, Env: d.Env})
		}
		valid, err := sandbox.ValidateServices(decl)
		if err != nil {
			return fail("covey.project.yml: %v", err)
		}
		wanted = valid
	}
	if len(wanted) == 0 && len(skipped) == 0 {
		// A recipe without services: nothing to start, nothing to record.
		return ok(serviceResult{State: "ready", Started: []sandbox.ServiceRun{}})
	}
	return o.launchServices(ctx, agent, taskID, "recipe", wanted, skipped, reset, ok, fail)
}

// launchServices is the part every source shares: the allowlist, the start on
// the host that holds the sandbox, and the answer within the daemon's deadline.
func (o *Orchestrator) launchServices(
	ctx context.Context, agent agents.Agent, taskID uuid.UUID, source string,
	wanted []sandbox.Service, skipped []sandbox.ComposeSkip, reset bool,
	ok func(any) daemon.InjectHiring, fail func(string, ...any) daemon.InjectHiring,
) daemon.InjectHiring {
	// The organisation's allowlist. Per service, not for the whole file: see
	// the note above about partial being normal here.
	var allowed []sandbox.Service
	var refused []serviceRefusal
	if o.Workplaces != nil {
		patterns, err := o.Workplaces.ServicePatterns(ctx, agent.OrgID)
		if err != nil {
			return fail("the organisation's allowlist is not readable: %v", err)
		}
		for _, svc := range wanted {
			if sandbox.ImageAllowed(patterns, svc.Image) {
				allowed = append(allowed, svc)
				continue
			}
			// The remedy travels with the refusal — but to the HUMAN, not to
			// the agent: it cannot add the pattern itself, and telling it how
			// would only invite it to try.
			refused = append(refused, serviceRefusal{
				Name:  svc.Name,
				Image: svc.Image,
				Reason: "this organisation does not allow the image " + svc.Image +
					" beside a sandbox. Somebody with administrative rights has to allow it; " +
					"report it and carry on without this service, or say that you cannot.",
			})
		}
	} else {
		allowed = wanted
	}

	result := serviceResult{State: "ready", Started: []sandbox.ServiceRun{}, Skipped: skipped, Refused: refused}
	if !reset {
		allowed, result.Kept = o.withoutRunning(agent.ID, allowed)
	}
	if len(allowed) == 0 {
		o.recordServiceRequest(ctx, agent, taskID, source, "started", nil, refused, nil)
		return ok(result)
	}
	starter, canStart := o.sandboxOf(agent.ID).(ServiceStarter)
	if !canStart {
		return fail("this sandbox cannot take services while it runs")
	}

	// One start at a time per sandbox. A second request while the first is
	// still pulling is almost always the agent retrying after a timeout; it
	// gets the state of the first rather than a second pull of the same image.
	o.mu.Lock()
	sess := o.sessions[agent.ID]
	if sess != nil && sess.provision != nil && !sess.provision.finished() {
		p := sess.provision
		o.mu.Unlock()
		result.State = "provisioning"
		result.Pending = p.pending
		result.Hint = provisioningHint
		return ok(result)
	}
	p := &serviceProvision{since: time.Now(), done: make(chan struct{})}
	for _, svc := range allowed {
		p.pending = append(p.pending, svc.Name)
	}
	if sess != nil {
		sess.provision = p
	}
	o.mu.Unlock()

	// Detached from the request: the request may give up waiting, the start
	// does not. The host bounds it with its own start timeout.
	bg := context.WithoutCancel(ctx)
	go func() {
		started, err := starter.StartServices(bg, allowed)
		if err != nil {
			// Recorded even in failure: an agent that could not get its
			// database is the most common reason for a run that reads like the
			// project is broken.
			o.recordServiceRequest(bg, agent, taskID, source, "failed", nil, refused, err)
		} else {
			o.mu.Lock()
			if s := o.sessions[agent.ID]; s != nil {
				s.services = mergeServices(s.services, started)
			}
			o.mu.Unlock()
			o.recordServiceRequest(bg, agent, taskID, source, "started", started, refused, nil)
		}
		p.started, p.err = started, err
		close(p.done)
	}()

	select {
	case <-p.done:
		if p.err != nil {
			return fail("%v", p.err)
		}
		result.Started = p.started
		return ok(result)
	case <-time.After(serviceAnswerWait):
	case <-ctx.Done():
	}
	result.State = "provisioning"
	result.Pending = p.pending
	result.Hint = provisioningHint
	return ok(result)
}

// servicesStatus serves covey/services_status: what stands beside the sandbox
// now, and how the last start_services went. It is the second half of a start
// that answered "provisioning" (#603).
func (o *Orchestrator) servicesStatus(
	agent agents.Agent, ok func(any) daemon.InjectHiring, fail func(string, ...any) daemon.InjectHiring,
) daemon.InjectHiring {
	o.mu.Lock()
	sess := o.sessions[agent.ID]
	if sess == nil {
		o.mu.Unlock()
		return fail("no sandbox is running for this agent")
	}
	running := slices.Clone(sess.services)
	p := sess.provision
	o.mu.Unlock()

	out := map[string]any{"state": "ready", "services": running}
	if running == nil {
		out["services"] = []sandbox.ServiceRun{}
	}
	if p == nil {
		return ok(out)
	}
	out["since"] = p.since.UTC().Format(time.RFC3339)
	switch {
	case !p.finished():
		out["state"] = "provisioning"
		out["pending"] = p.pending
		out["waited_seconds"] = int(time.Since(p.since).Seconds())
	case p.err != nil:
		out["state"] = "failed"
		out["error"] = p.err.Error()
	}
	return ok(out)
}

// withoutRunning drops the services that already run beside the sandbox under
// the same name and image, and names them.
func (o *Orchestrator) withoutRunning(agentID uuid.UUID, wanted []sandbox.Service) ([]sandbox.Service, []string) {
	o.mu.Lock()
	var running []sandbox.ServiceRun
	if s := o.sessions[agentID]; s != nil {
		running = s.services
	}
	o.mu.Unlock()
	var start []sandbox.Service
	var kept []string
	for _, svc := range wanted {
		if slices.ContainsFunc(running, func(r sandbox.ServiceRun) bool {
			return r.Name == svc.Name && r.Image == svc.Image
		}) {
			kept = append(kept, svc.Name)
			continue
		}
		start = append(start, svc)
	}
	return start, kept
}

// mergeServices adds what started to what runs. A name started again replaces
// its entry: the runner replaced the container, and two entries for one
// address would report a service twice.
func mergeServices(have, started []sandbox.ServiceRun) []sandbox.ServiceRun {
	out := slices.Clone(have)
	for _, st := range started {
		if i := slices.IndexFunc(out, func(r sandbox.ServiceRun) bool { return r.Name == st.Name }); i >= 0 {
			out[i] = st
			continue
		}
		out = append(out, st)
	}
	return out
}

// sandboxOf is the sandbox of a running session, or nil.
func (o *Orchestrator) sandboxOf(agentID uuid.UUID) Sandbox {
	o.mu.Lock()
	defer o.mu.Unlock()
	if s := o.sessions[agentID]; s != nil {
		return s.sandbox
	}
	return nil
}

// recordServiceRequest writes what the agent asked for and what came of it —
// against the TASK, because this happened during a run and belongs to it.
func (o *Orchestrator) recordServiceRequest(
	ctx context.Context, agent agents.Agent, taskID uuid.UUID, source, status string,
	started []sandbox.ServiceRun, refused []serviceRefusal, err error,
) {
	if o.Obs == nil {
		return
	}
	payload := map[string]any{"status": status, "source": source}
	if len(started) > 0 {
		payload["services"] = started
	}
	if len(refused) > 0 {
		payload["refused"] = refused
	}
	if err != nil {
		payload["error"] = err.Error()
	}
	_ = o.Obs.Record(ctx, agent.OrgID, agent.ID, &taskID, observability.KindService, payload)
}

// mayStartServices: the scope that unlocks this, and nothing else. Deliberately
// not `agents:write` — that one lets an agent draft colleagues, and a QA agent
// that wants a database has no business with it.
func (o *Orchestrator) mayStartServices(ctx context.Context, agent agents.Agent) bool {
	return o.mayUsecovey(ctx, agent, scopeServices)
}
