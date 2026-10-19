// Package recipe reads a project's own recipe, covey.project.yml
// (spec/30-project-recipes.md): how the project is installed, started and
// tested, which services it needs, which secrets it names and what its
// application is told.
//
// The package is pure. It parses, validates and renders; it starts nothing and
// asks nobody. The daemon reads the file out of the agent's checkout, the
// control plane decides about the services, the broker about the secrets.
//
// Validation is strict on purpose. A recipe is reviewed like code and applies
// to every agent on the project, so a misspelt key that would silently do
// nothing is refused with its name rather than ignored.
package recipe

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"path"
	"regexp"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// FileName is where the recipe lives: the root of the repository.
const FileName = "covey.project.yml"

// MaxSize caps the file. A recipe that needs more than this has stopped
// declaring and started scripting, which belongs in a script it points at.
const MaxSize = 256 << 10

// EnvironmentTarget is the value of env.write_to that means "into the process
// environment of what start brings up" rather than a file in the checkout.
const EnvironmentTarget = "environment"

// Recipe is the whole file. Either the flat form (Stack inline) or
// Components, never both.
type Recipe struct {
	Version    int    `yaml:"version"`
	Name       string `yaml:"name"`
	Stack      `yaml:",inline"`
	Components map[string]*Component `yaml:"components"`
}

// Component is one part of a monorepo: a stack with a path of its own.
type Component struct {
	Path  string `yaml:"path"`
	Stack `yaml:",inline"`
}

// Stack is what one stack declares. The flat form is a single stack at the
// repository root.
type Stack struct {
	Compose  string               `yaml:"compose"`
	Services []Service            `yaml:"services"`
	Secrets  []string             `yaml:"secrets"`
	Env      *Env                 `yaml:"env"`
	Commands map[string]string    `yaml:"commands"`
	Start    map[string]Process   `yaml:"start"`
	App      *App                 `yaml:"app"`
	MCP      map[string]MCPServer `yaml:"mcp"`
	Baseline *Baseline            `yaml:"baseline"`
}

// Service is a service declared in the recipe, in the shape services beside
// the sandbox already have (spec/16).
type Service struct {
	Name  string            `yaml:"name" json:"name"`
	Image string            `yaml:"image" json:"image"`
	Env   map[string]string `yaml:"env" json:"env,omitempty"`
}

// Env is what the application is told.
type Env struct {
	// WriteTo is a dotenv file relative to the stack, or "environment".
	WriteTo string `yaml:"write_to"`
	// Base is the project's own env file, copied first and then overlaid.
	Base string            `yaml:"base"`
	Vars map[string]string `yaml:"vars"`
}

// Process is one long-running entry of start.
type Process struct {
	Cmd  string `yaml:"cmd" json:"cmd"`
	Port int    `yaml:"port" json:"port,omitempty"`
	// Cwd is relative to the stack; empty is the stack itself.
	Cwd string `yaml:"cwd" json:"cwd,omitempty"`
}

// App is where a person would open the application, and how to tell it is up.
type App struct {
	URL   string `yaml:"url" json:"url"`
	Ready string `yaml:"ready" json:"ready,omitempty"`
}

// MCPServer is a stdio MCP server the recipe asks for (spec/30, stage 2). It is
// read and reported, not yet started.
type MCPServer struct {
	Command string            `yaml:"command" json:"command"`
	Args    []string          `yaml:"args" json:"args,omitempty"`
	Env     map[string]string `yaml:"env" json:"env,omitempty"`
}

// Baseline is the known-red initial state at a named ref.
type Baseline struct {
	Ref          string   `yaml:"ref" json:"ref"`
	KnownFailing []string `yaml:"known_failing" json:"known_failing,omitempty"`
}

var (
	// secretName is the charset secret keys are created with, the same the
	// {{secret:<key>}} placeholder accepts (internal/daemon/actionproxy.go).
	secretName = regexp.MustCompile(`^[A-Za-z0-9_.-]+$`)
	envKey     = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	// entryName is a component, command or start entry. Start entries become
	// dev job names, so the charset is what a file name survives.
	entryName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,39}$`)
	// secretRef is how a variable names a secret: {{ secret.<name> }}.
	secretRef = regexp.MustCompile(`\{\{\s*secret\.([A-Za-z0-9_.-]+)\s*\}\}`)
)

// Parse reads and validates a recipe.
func Parse(raw []byte) (*Recipe, error) {
	if len(raw) > MaxSize {
		return nil, fmt.Errorf("%s is larger than %d KB — point at a script instead of inlining it", FileName, MaxSize>>10)
	}
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	dec.KnownFields(true)
	var r Recipe
	if err := dec.Decode(&r); err != nil {
		if errors.Is(err, io.EOF) {
			return nil, fmt.Errorf("%s is empty", FileName)
		}
		return nil, fmt.Errorf("%s: %w", FileName, err)
	}
	if err := r.validate(); err != nil {
		return nil, fmt.Errorf("%s: %w", FileName, err)
	}
	return &r, nil
}

func (r *Recipe) validate() error {
	if r.Version != 1 {
		return fmt.Errorf("version: %d is not a version this covey reads (write `version: 1`)", r.Version)
	}
	if len(r.Components) > 0 {
		if !r.Stack.empty() {
			return errors.New("the flat form and `components:` are mutually exclusive — move the top-level keys into a component")
		}
		for name, c := range r.Components {
			if !entryName.MatchString(name) {
				return fmt.Errorf("component %q: letters, digits, `.`, `_` and `-`, at most 40 characters", name)
			}
			if c == nil {
				return fmt.Errorf("component %q is empty", name)
			}
			if c.Path == "" {
				return fmt.Errorf("component %q has no `path`", name)
			}
			if err := relPath(c.Path); err != nil {
				return fmt.Errorf("component %q: path: %w", name, err)
			}
			if err := c.Stack.validate(); err != nil {
				return fmt.Errorf("component %q: %w", name, err)
			}
		}
		return nil
	}
	return r.Stack.validate()
}

func (s *Stack) empty() bool {
	return s.Compose == "" && len(s.Services) == 0 && len(s.Secrets) == 0 && s.Env == nil &&
		len(s.Commands) == 0 && len(s.Start) == 0 && s.App == nil && len(s.MCP) == 0 && s.Baseline == nil
}

func (s *Stack) validate() error {
	if s.Compose != "" && len(s.Services) > 0 {
		return errors.New("`compose` and `services` are mutually exclusive — name the project's compose file or declare the services, not both")
	}
	if s.Compose != "" {
		if err := relPath(s.Compose); err != nil {
			return fmt.Errorf("compose: %w", err)
		}
	}
	seen := map[string]bool{}
	for i, svc := range s.Services {
		if strings.TrimSpace(svc.Name) == "" {
			return fmt.Errorf("services[%d] has no name", i)
		}
		if strings.TrimSpace(svc.Image) == "" {
			return fmt.Errorf("service %q has no image", svc.Name)
		}
		if seen[svc.Name] {
			return fmt.Errorf("service %q is declared twice", svc.Name)
		}
		seen[svc.Name] = true
	}
	declared := map[string]bool{}
	for _, name := range s.Secrets {
		if !secretName.MatchString(name) {
			return fmt.Errorf("secrets: %q is not a secret name (letters, digits, `.`, `_`, `-`) — the recipe names secrets, it never holds them", name)
		}
		declared[name] = true
	}
	if s.Env != nil {
		if err := s.Env.validate(declared); err != nil {
			return fmt.Errorf("env: %w", err)
		}
	}
	for name, cmd := range s.Commands {
		if !entryName.MatchString(name) {
			return fmt.Errorf("commands: %q is not a command name", name)
		}
		if strings.TrimSpace(cmd) == "" {
			return fmt.Errorf("commands.%s is empty", name)
		}
	}
	for name, p := range s.Start {
		if !entryName.MatchString(name) {
			return fmt.Errorf("start: %q is not a process name (letters, digits, `.`, `_`, `-`, at most 40 characters)", name)
		}
		if strings.TrimSpace(p.Cmd) == "" {
			return fmt.Errorf("start.%s has no `cmd`", name)
		}
		if p.Port < 0 || p.Port > 65535 {
			return fmt.Errorf("start.%s: port %d is not a port", name, p.Port)
		}
		if p.Cwd != "" {
			if err := relPath(p.Cwd); err != nil {
				return fmt.Errorf("start.%s: cwd: %w", name, err)
			}
		}
	}
	if s.App != nil && strings.TrimSpace(s.App.URL) == "" {
		return errors.New("app has no `url`")
	}
	for name, m := range s.MCP {
		if !entryName.MatchString(name) {
			return fmt.Errorf("mcp: %q is not a server name", name)
		}
		if strings.TrimSpace(m.Command) == "" {
			return fmt.Errorf("mcp.%s has no `command`", name)
		}
	}
	if s.Baseline != nil && strings.TrimSpace(s.Baseline.Ref) == "" {
		return errors.New("baseline has no `ref`")
	}
	return nil
}

func (e *Env) validate(declared map[string]bool) error {
	switch {
	case e.WriteTo == "":
		return errors.New("`write_to` is missing — a dotenv path in the checkout, or `environment`")
	case e.WriteTo != EnvironmentTarget:
		if err := relPath(e.WriteTo); err != nil {
			return fmt.Errorf("write_to: %w", err)
		}
	}
	if e.Base != "" {
		if err := relPath(e.Base); err != nil {
			return fmt.Errorf("base: %w", err)
		}
	}
	for k, v := range e.Vars {
		if !envKey.MatchString(k) {
			return fmt.Errorf("vars: %q is not an environment variable name", k)
		}
		for _, m := range secretRef.FindAllStringSubmatch(v, -1) {
			if !declared[m[1]] {
				return fmt.Errorf("vars.%s names the secret %q, which is not listed under `secrets:`", k, m[1])
			}
		}
	}
	return nil
}

// relPath accepts a path inside the stack: relative, without a way out.
func relPath(p string) error {
	if strings.TrimSpace(p) == "" {
		return errors.New("empty")
	}
	if path.IsAbs(p) || strings.HasPrefix(p, "\\") {
		return fmt.Errorf("%q is absolute — paths in the recipe are relative to the repository", p)
	}
	clean := path.Clean(p)
	if clean == ".." || strings.HasPrefix(clean, "../") {
		return fmt.Errorf("%q leads out of the repository", p)
	}
	return nil
}

// ComponentNames are the components in a stable order.
func (r *Recipe) ComponentNames() []string {
	names := make([]string, 0, len(r.Components))
	for n := range r.Components {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// Select picks the stack to act on and the path it lives at, relative to the
// repository root. The flat form takes no component; the components form
// requires one.
func (r *Recipe) Select(component string) (Stack, string, error) {
	if len(r.Components) == 0 {
		if component != "" {
			return Stack{}, "", fmt.Errorf("this recipe has no components — leave `component` out")
		}
		return r.Stack, ".", nil
	}
	if component == "" {
		return Stack{}, "", fmt.Errorf("this recipe has components; name one with `component`: %s", strings.Join(r.ComponentNames(), ", "))
	}
	c, ok := r.Components[component]
	if !ok {
		return Stack{}, "", fmt.Errorf("no component %q in the recipe; it has: %s", component, strings.Join(r.ComponentNames(), ", "))
	}
	return c.Stack, path.Clean(c.Path), nil
}

// ResolveVars fills the {{ secret.<name> }} references in the variables with
// the values given. A reference without a value is an error naming it; the
// validation has already made sure every reference is a declared secret.
func ResolveVars(vars map[string]string, secrets map[string]string) (map[string]string, error) {
	out := make(map[string]string, len(vars))
	for k, v := range vars {
		var missing string
		resolved := secretRef.ReplaceAllStringFunc(v, func(m string) string {
			name := secretRef.FindStringSubmatch(m)[1]
			val, ok := secrets[name]
			if !ok && missing == "" {
				missing = name
			}
			return val
		})
		if missing != "" {
			return nil, fmt.Errorf("vars.%s: the secret %q has no value", k, missing)
		}
		out[k] = resolved
	}
	return out, nil
}

// SecretNames are the secrets a stack's variables actually reference, sorted.
// A secret listed but referenced nowhere is not fetched: there is nowhere for
// its value to go.
func (s *Stack) SecretNames() []string {
	if s.Env == nil {
		return nil
	}
	seen := map[string]bool{}
	for _, v := range s.Env.Vars {
		for _, m := range secretRef.FindAllStringSubmatch(v, -1) {
			seen[m[1]] = true
		}
	}
	names := make([]string, 0, len(seen))
	for n := range seen {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}
