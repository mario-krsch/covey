package daemon

// The project recipe in the sandbox (spec/30-project-recipes.md): covey/env_up,
// covey/env_status, covey/env_reset.
//
// The daemon does the reading and the writing, the control plane the deciding.
// The checkout is in the sandbox, and the daemon is already where
// {{secret:<key>}} placeholders are resolved before an action runs. So it parses
// covey.project.yml here, sends the services to the control plane the way the
// compose route sends its content (scope, guard rails, allowlist are decided
// there), asks the broker for the secrets the recipe names, and writes the
// application's environment into the checkout itself. The control plane learns
// the service list and the secret names; it never needs the file.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"covey/internal/recipe"
)

// envParams are what the agent sends: the checkout from the checkout result,
// and the component in a monorepo recipe.
type envParams struct {
	Checkout  string `json:"checkout"`
	Component string `json:"component"`
}

// envRecord is what env_up wrote, kept beside the home so env_status can say
// it on a later wake. Names only — never a value.
type envRecord struct {
	Checkout  string   `json:"checkout"`
	Component string   `json:"component,omitempty"`
	WrittenTo string   `json:"written_to,omitempty"`
	Vars      []string `json:"vars,omitempty"`
	Secrets   []string `json:"secrets,omitempty"`
	At        string   `json:"at"`
	Op        string   `json:"op"`
}

// envDir holds the records and the `environment` form of write_to: in the
// home, outside every checkout, so a rendered secret never sits in a tree an
// agent commits from.
func envDir(home string) string { return filepath.Join(home, ".covey", "env") }

func envKey(checkout, component string) string {
	key := filepath.Base(checkout)
	if component != "" {
		key += "--" + component
	}
	return key
}

func (p *actionProxy) projectEnv(ctx context.Context, action string, params json.RawMessage) any {
	var in envParams
	_ = json.Unmarshal(params, &in)
	home := p.client.homeDir
	root, err := checkoutRoot(home, in.Checkout)
	if err != nil {
		return map[string]string{"status": "error", "error": err.Error()}
	}
	file := filepath.Join(root, recipe.FileName)
	raw, err := readRecipe(file)
	if err != nil {
		return map[string]string{"status": "error", "error": err.Error()}
	}
	r, err := recipe.Parse(raw)
	if err != nil {
		return map[string]string{"status": "error", "error": err.Error()}
	}
	stack, rel, err := r.Select(strings.TrimSpace(in.Component))
	if err != nil {
		return map[string]string{"status": "error", "error": err.Error()}
	}
	dir, err := stackDir(root, rel)
	if err != nil {
		return map[string]string{"status": "error", "error": err.Error()}
	}

	if action == "env_status" {
		return p.envStatus(ctx, home, root, in.Component)
	}

	// The control plane first: it holds the gate (scope, guard rails), so
	// nothing is fetched or written for an agent that may not do this.
	req := RequestHiring{Op: action, TaskID: p.taskID}
	if stack.Compose != "" {
		path, err := insideDir(dir, stack.Compose)
		if err != nil {
			return map[string]string{"status": "error", "error": "compose: " + err.Error()}
		}
		content, err := os.ReadFile(path)
		if err != nil {
			return map[string]string{"status": "error", "error": fmt.Sprintf("compose: %s is named in the recipe but cannot be read: %v", stack.Compose, err)}
		}
		req.Compose = string(content)
	}
	for _, svc := range stack.Services {
		req.Services = append(req.Services, ServiceDecl{Name: svc.Name, Image: svc.Image, Env: svc.Env})
	}
	resp, err := p.askHiring(ctx, req)
	if err != nil {
		return map[string]string{"status": "error", "error": err.Error()}
	}
	if resp.Pending {
		return map[string]string{
			"status":          "pending_approval",
			"approval_id":     resp.ApprovalID,
			"correlation_key": resp.CorrelationKey,
		}
	}
	if !resp.OK {
		return map[string]string{"status": "error", "error": resp.Error}
	}
	var services map[string]any
	_ = json.Unmarshal(resp.Data, &services)

	// Then the secrets and the environment. A name the store does not answer
	// fails the whole call, naming it: an application that starts without
	// its token answers its health check and refuses every real request.
	written, envFile, err := p.writeEnv(ctx, home, root, dir, in.Component, stack)
	if err != nil {
		return map[string]any{"status": "error", "error": err.Error(), "services": services}
	}

	out := map[string]any{
		"state":    services["state"],
		"recipe":   file,
		"services": services,
		"dir":      dir,
	}
	if in.Component != "" {
		out["component"] = in.Component
	}
	if written != nil {
		out["env"] = written
		rec := envRecord{Checkout: root, Component: in.Component, WrittenTo: written.WrittenTo,
			Vars: written.Vars, Secrets: written.Secrets, At: time.Now().UTC().Format(time.RFC3339), Op: action}
		saveEnvRecord(home, envKey(root, in.Component), rec)
	}
	if len(stack.Commands) > 0 {
		out["commands"] = stack.Commands
	}
	if len(stack.Start) > 0 {
		out["start"] = startEntries(dir, envFile, stack.Start)
	}
	if stack.App != nil {
		out["app"] = stack.App
	}
	if stack.Baseline != nil {
		out["baseline"] = stack.Baseline
	}
	if len(stack.MCP) > 0 {
		names := make([]string, 0, len(stack.MCP))
		for n := range stack.MCP {
			names = append(names, n)
		}
		sort.Strings(names)
		out["mcp"] = map[string]any{"servers": names, "note": "declared, but not started by this covey version"}
	}
	out["next"] = envNext(action, stack)

	audit, _ := json.Marshal(map[string]any{"action": "covey:" + action, "checkout": root,
		"component": in.Component, "secrets": recordSecrets(written)})
	_ = p.client.send(TypeEvent, Event{TaskID: p.taskID, Kind: "action", Payload: audit})
	return map[string]any{"status": "ok", "data": out}
}

// envWritten is what the agent is told about the environment: where, and which
// names. Never a value.
type envWritten struct {
	WrittenTo string   `json:"written_to"`
	Vars      []string `json:"vars"`
	Secrets   []string `json:"secrets,omitempty"`
}

func recordSecrets(w *envWritten) []string {
	if w == nil {
		return nil
	}
	return w.Secrets
}

// writeEnv resolves the secrets and writes the variables where the recipe says.
// For the `environment` form it returns the file the start entries source.
func (p *actionProxy) writeEnv(ctx context.Context, home, root, dir, component string, stack recipe.Stack) (*envWritten, string, error) {
	if stack.Env == nil {
		return nil, "", nil
	}
	values := map[string]string{}
	names := stack.SecretNames()
	for _, name := range names {
		sec, err := p.fetchSecret(ctx, name, p.taskID)
		if err != nil {
			return nil, "", fmt.Errorf("the recipe needs the secret %q: %v — it has to be stored for this agent (or assigned to it) under exactly that key", name, err)
		}
		values[name] = sec.Value
	}
	vars, err := recipe.ResolveVars(stack.Env.Vars, values)
	if err != nil {
		return nil, "", err
	}
	varNames := make([]string, 0, len(vars))
	for k := range vars {
		varNames = append(varNames, k)
	}
	sort.Strings(varNames)
	w := &envWritten{Vars: varNames, Secrets: names}

	if stack.Env.WriteTo == recipe.EnvironmentTarget {
		if err := os.MkdirAll(envDir(home), 0o700); err != nil {
			return nil, "", err
		}
		file := filepath.Join(envDir(home), envKey(root, component)+".env")
		if err := writePrivate(file, recipe.RenderShell(vars)); err != nil {
			return nil, "", err
		}
		w.WrittenTo = file
		return w, file, nil
	}

	var base []byte
	if stack.Env.Base != "" {
		path, err := insideDir(dir, stack.Env.Base)
		if err != nil {
			return nil, "", fmt.Errorf("env.base: %v", err)
		}
		base, err = os.ReadFile(path)
		if err != nil {
			return nil, "", fmt.Errorf("env.base: %s is named in the recipe but cannot be read: %v", stack.Env.Base, err)
		}
	}
	target, err := insideDir(dir, stack.Env.WriteTo)
	if err != nil {
		return nil, "", fmt.Errorf("env.write_to: %v", err)
	}
	if err := writePrivate(target, recipe.RenderDotenv(base, vars)); err != nil {
		return nil, "", err
	}
	w.WrittenTo = target
	return w, "", nil
}

// startEntries turns the recipe's start map into what the agent hands to
// `dev start`, ready to use: the directory, and for the `environment` form the
// command prefixed with sourcing the rendered file.
func startEntries(dir, envFile string, start map[string]recipe.Process) map[string]any {
	out := map[string]any{}
	for name, proc := range start {
		cwd := dir
		if proc.Cwd != "" {
			cwd = filepath.Join(dir, filepath.FromSlash(proc.Cwd))
		}
		cmd := proc.Cmd
		if envFile != "" {
			cmd = "set -a && . " + recipe.ShellQuote(envFile) + " && set +a && " + cmd
		}
		entry := map[string]any{"cmd": proc.Cmd, "dev_start": map[string]string{"name": name, "cmd": cmd, "cwd": cwd}}
		if proc.Port > 0 {
			entry["port"] = proc.Port
		}
		out[name] = entry
	}
	return out
}

func envNext(action string, stack recipe.Stack) string {
	var steps []string
	if action == "env_reset" {
		steps = append(steps, "the services are empty now")
	}
	for _, c := range []string{"install", "migrate", "seed"} {
		if _, ok := stack.Commands[c]; ok && (action == "env_up" || c != "install") {
			steps = append(steps, "run commands."+c)
		}
	}
	if len(stack.Start) > 0 {
		steps = append(steps, "start each entry of `start` with dev start using its dev_start parameters")
	}
	if len(steps) == 0 {
		return "nothing further in the recipe."
	}
	return strings.Join(steps, "; ") + ". A service is running before it accepts connections: wait for its port."
}

func (p *actionProxy) envStatus(ctx context.Context, home, root, component string) any {
	resp, err := p.askHiring(ctx, RequestHiring{Op: "env_status", TaskID: p.taskID})
	if err != nil {
		return map[string]string{"status": "error", "error": err.Error()}
	}
	if resp.Pending {
		return map[string]string{"status": "pending_approval", "approval_id": resp.ApprovalID,
			"correlation_key": resp.CorrelationKey}
	}
	if !resp.OK {
		return map[string]string{"status": "error", "error": resp.Error}
	}
	var services map[string]any
	_ = json.Unmarshal(resp.Data, &services)
	out := map[string]any{"state": services["state"], "services": services}
	if rec, ok := loadEnvRecord(home, envKey(root, component)); ok {
		out["env"] = rec
	} else {
		out["env"] = nil
		out["note"] = "env_up has not written an environment for this checkout yet"
	}
	return map[string]any{"status": "ok", "data": out}
}

// checkoutRoot resolves the checkout the agent names: the path from the
// checkout result, inside its home.
func checkoutRoot(home, checkout string) (string, error) {
	c := strings.TrimSpace(checkout)
	if c == "" {
		return "", errors.New("checkout is missing — send the path from the checkout result, e.g. {\"checkout\":\"~/repos/<project>\"}")
	}
	switch {
	case c == "~":
		c = home
	case strings.HasPrefix(c, "~/"):
		c = filepath.Join(home, c[2:])
	case !filepath.IsAbs(c):
		c = filepath.Join(home, c)
	}
	realHome, err := filepath.EvalSymlinks(home)
	if err != nil {
		return "", fmt.Errorf("home: %v", err)
	}
	real, err := filepath.EvalSymlinks(c)
	if err != nil {
		return "", fmt.Errorf("checkout %s: not found", checkout)
	}
	if !within(realHome, real) || real == realHome {
		return "", fmt.Errorf("checkout %s lies outside your home", checkout)
	}
	return real, nil
}

// stackDir is the directory a stack lives in: the checkout itself, or a
// component's path inside it.
func stackDir(root, rel string) (string, error) {
	p, err := filepath.EvalSymlinks(filepath.Join(root, filepath.FromSlash(rel)))
	if err != nil {
		return "", fmt.Errorf("component path %q does not exist in the checkout", rel)
	}
	if !within(root, p) {
		return "", fmt.Errorf("component path %q leads out of the checkout", rel)
	}
	if info, err := os.Stat(p); err != nil || !info.IsDir() {
		return "", fmt.Errorf("component path %q is not a directory", rel)
	}
	return p, nil
}

func readRecipe(file string) ([]byte, error) {
	info, err := os.Lstat(file)
	if err != nil {
		return nil, fmt.Errorf("this checkout has no %s at its root. It is the project's recipe (how it is installed, started and tested); "+
			"without it there is nothing to set up from — write one as a merge request", recipe.FileName)
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("%s is not a regular file", recipe.FileName)
	}
	if info.Size() > recipe.MaxSize {
		return nil, fmt.Errorf("%s is larger than %d KB", recipe.FileName, recipe.MaxSize>>10)
	}
	return os.ReadFile(file)
}

// insideDir joins a recipe path onto dir and makes sure the result stays inside
// it, following symlinks for the part that exists. The recipe comes from a
// repository nobody at covey has read; a `.env` that is a symlink to a file in
// the home must not be written through.
func insideDir(dir, rel string) (string, error) {
	p := filepath.Join(dir, filepath.FromSlash(rel))
	if !within(dir, p) {
		return "", fmt.Errorf("%q leads out of the checkout", rel)
	}
	if info, err := os.Lstat(p); err == nil && info.Mode()&os.ModeSymlink != 0 {
		return "", fmt.Errorf("%q is a symbolic link; covey does not write or read through one", rel)
	}
	parent, err := filepath.EvalSymlinks(filepath.Dir(p))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", fmt.Errorf("the directory of %q does not exist", rel)
		}
		return "", err
	}
	if !within(dir, parent) {
		return "", fmt.Errorf("%q leads out of the checkout", rel)
	}
	return filepath.Join(parent, filepath.Base(p)), nil
}

func within(dir, p string) bool {
	rel, err := filepath.Rel(dir, p)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
}

// writePrivate writes a file that may hold a secret: owner-only, replaced
// atomically so a process reading it never sees half of it.
func writePrivate(path string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".covey-env-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

func saveEnvRecord(home, key string, rec envRecord) {
	if os.MkdirAll(envDir(home), 0o700) != nil {
		return
	}
	data, _ := json.MarshalIndent(rec, "", "  ")
	_ = writePrivate(filepath.Join(envDir(home), key+".json"), data)
}

func loadEnvRecord(home, key string) (envRecord, bool) {
	data, err := os.ReadFile(filepath.Join(envDir(home), key+".json"))
	if err != nil {
		return envRecord{}, false
	}
	var rec envRecord
	return rec, json.Unmarshal(data, &rec) == nil
}
