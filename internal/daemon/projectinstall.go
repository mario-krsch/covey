package daemon

// covey/env_install: the recipe's install, with registry credentials for the
// install alone (spec/30, stage 3; #606).
//
// npm and Composer want a credential before the first private package
// resolves, and both can take it from the environment: an .npmrc line
// `//host/:_authToken=${NPM_TOKEN}`, the COMPOSER_AUTH variable. So the recipe
// names them under install_env, with secrets by name, and the daemon runs
// commands.install with those values in that one process's environment —
// nothing written to a file, nothing left in the home, the values masked in
// the log.
//
// This is weaker than the askpass route for git, and it is said so: the
// install's own scripts run in that environment and can read it. A postinstall
// script of a foreign dependency is exactly such a script. What the route
// does guarantee is that the credential is not lying around for the next
// process, or the next run.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"covey/internal/recipe"
)

// installTimeout bounds one install. A dependency install that has not
// finished in an hour is stuck, and a stuck process keeps its credential.
var installTimeout = 60 * time.Minute

// installStatus is what env_status reports about the last install of a stack,
// kept beside the env record so a later wake still knows it.
type installStatus struct {
	State    string   `json:"state"` // running | succeeded | failed
	Command  string   `json:"command"`
	Log      string   `json:"log"`
	Secrets  []string `json:"secrets,omitempty"`
	Started  string   `json:"started"`
	Finished string   `json:"finished,omitempty"`
	ExitCode *int     `json:"exit_code,omitempty"`
	Error    string   `json:"error,omitempty"`
}

var (
	installsMu sync.Mutex
	installs   = map[string]*installStatus{} // envKey → the install running in this daemon
)

func installStatusFile(home, key string) string {
	return filepath.Join(envDir(home), key+".install.json")
}

func (p *actionProxy) envInstall(ctx context.Context, home, root, dir, component string, stack recipe.Stack) any {
	cmdline := strings.TrimSpace(stack.Commands["install"])
	if cmdline == "" {
		return map[string]string{"status": "error", "error": "the recipe has no commands.install"}
	}
	key := envKey(root, component)
	installsMu.Lock()
	if run := installs[key]; run != nil && run.State == "running" {
		st := *run
		installsMu.Unlock()
		return map[string]any{"status": "ok", "data": map[string]any{"install": st,
			"note": "an install of this checkout is already running — read it with covey/env_status"}}
	}
	installsMu.Unlock()

	// The gate is the control plane's: scope and guard rails, before any
	// secret is fetched.
	resp, err := p.askHiring(ctx, RequestHiring{Op: "env_install", TaskID: p.taskID})
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

	names := stack.InstallSecretNames()
	values := map[string]string{}
	for _, name := range names {
		sec, err := p.fetchSecret(ctx, name, p.taskID)
		if err != nil {
			return map[string]string{"status": "error", "error": fmt.Sprintf("the install needs the secret %q: %v — it has to be stored for this agent (or assigned to it) under exactly that key", name, err)}
		}
		values[name] = sec.Value
	}
	extra, err := recipe.ResolveVars(stack.InstallEnv, values)
	if err != nil {
		return map[string]string{"status": "error", "error": err.Error()}
	}
	var mask []string
	for _, v := range values {
		mask = append(mask, v)
	}
	for _, v := range extra {
		mask = append(mask, v)
	}

	if err := os.MkdirAll(envDir(home), 0o700); err != nil {
		return map[string]string{"status": "error", "error": err.Error()}
	}
	logPath := filepath.Join(envDir(home), key+".install.log")
	st := &installStatus{State: "running", Command: cmdline, Log: logPath, Secrets: names,
		Started: time.Now().UTC().Format(time.RFC3339)}
	if err := startInstall(home, key, dir, cmdline, installEnv(extra), mask, st); err != nil {
		return map[string]string{"status": "error", "error": err.Error()}
	}

	audit, _ := json.Marshal(map[string]any{"action": "covey:env_install", "checkout": root,
		"component": component, "secrets": names})
	if p.client != nil {
		_ = p.client.send(TypeEvent, Event{TaskID: p.taskID, Kind: "action", Payload: audit})
	}
	installsMu.Lock()
	out := *st
	installsMu.Unlock()
	return map[string]any{"status": "ok", "data": map[string]any{"install": out,
		"next": "the install runs as a job and outlives this call; ask covey/env_status for its state and the tail of its log"}}
}

// installEnv is the daemon's environment as a dev process gets it — without
// the daemon's own COVEY_* variables, the action port kept — plus the
// recipe's install_env.
func installEnv(extra map[string]string) []string {
	var env []string
	for _, kv := range os.Environ() {
		if strings.HasPrefix(kv, "COVEY_") && !strings.HasPrefix(kv, "COVEY_ACTION_PORT=") {
			continue
		}
		env = append(env, kv)
	}
	for k, v := range extra {
		env = append(env, k+"="+v)
	}
	return env
}

func startInstall(home, key, dir, cmdline string, env, mask []string, st *installStatus) error {
	logFile, err := os.OpenFile(st.Log, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	out := &maskWriter{w: logFile, secrets: mask}
	ctx, cancel := context.WithTimeout(context.Background(), installTimeout)
	cmd := exec.CommandContext(ctx, "sh", "-c", cmdline)
	cmd.Dir = dir
	cmd.Env = env
	cmd.Stdout, cmd.Stderr = out, out
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	if err := cmd.Start(); err != nil {
		cancel()
		logFile.Close()
		return fmt.Errorf("start the install: %w", err)
	}
	installsMu.Lock()
	installs[key] = st
	installsMu.Unlock()
	saveInstallStatus(home, key, st)
	go func() {
		defer cancel()
		err := cmd.Wait()
		out.flush()
		logFile.Close()
		installsMu.Lock()
		code := cmd.ProcessState.ExitCode()
		st.ExitCode = &code
		st.Finished = time.Now().UTC().Format(time.RFC3339)
		switch {
		case ctx.Err() == context.DeadlineExceeded:
			st.State, st.Error = "failed", fmt.Sprintf("stopped after %s", installTimeout)
		case err != nil:
			st.State = "failed"
		default:
			st.State = "succeeded"
		}
		snapshot := *st
		installsMu.Unlock()
		saveInstallStatus(home, key, &snapshot)
	}()
	return nil
}

func saveInstallStatus(home, key string, st *installStatus) {
	installsMu.Lock()
	data, _ := json.MarshalIndent(st, "", "  ")
	installsMu.Unlock()
	_ = writePrivate(installStatusFile(home, key), data)
}

// lastInstall is the install of this stack as env_status reports it: the one
// running in this daemon, or the record of the last one, with the tail of its
// log.
func lastInstall(home, key string) map[string]any {
	installsMu.Lock()
	var st installStatus
	if run := installs[key]; run != nil {
		st = *run
	}
	installsMu.Unlock()
	if st.State == "" {
		data, err := os.ReadFile(installStatusFile(home, key))
		if err != nil || json.Unmarshal(data, &st) != nil {
			return nil
		}
		if st.State == "running" {
			// Recorded as running, but not running in this daemon: the
			// sandbox was restarted underneath it.
			st.State, st.Error = "failed", "interrupted — the sandbox was restarted while it ran"
		}
	}
	out := map[string]any{"install": st}
	if tail, err := readTail(st.Log, 8<<10); err == nil {
		lines := strings.Split(strings.TrimRight(string(tail), "\n"), "\n")
		if len(lines) > 40 {
			lines = lines[len(lines)-40:]
		}
		out["log_tail"] = strings.Join(lines, "\n")
	}
	return out
}

// maskWriter replaces secret values in what the install prints before it
// reaches the log. Line by line, so a value is never split across two writes.
type maskWriter struct {
	mu      sync.Mutex
	w       io.Writer
	secrets []string
	buf     []byte
}

const maskedValue = "[secret]"

func (m *maskWriter) Write(p []byte) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.buf = append(m.buf, p...)
	if i := bytes.LastIndexByte(m.buf, '\n'); i >= 0 {
		m.emit(m.buf[:i+1])
		m.buf = append(m.buf[:0], m.buf[i+1:]...)
	} else if len(m.buf) > 64<<10 {
		m.emit(m.buf)
		m.buf = m.buf[:0]
	}
	return len(p), nil
}

func (m *maskWriter) flush() {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.buf) > 0 {
		m.emit(m.buf)
		m.buf = nil
	}
}

func (m *maskWriter) emit(b []byte) {
	s := string(b)
	for _, v := range m.secrets {
		if len(v) >= 4 {
			s = strings.ReplaceAll(s, v, maskedValue)
		}
	}
	_, _ = io.WriteString(m.w, s)
}
