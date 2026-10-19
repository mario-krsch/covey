package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// setUpRecipe writes a checkout with the recipe and the record env_up leaves,
// which is what makes a recipe count for the MCP servers.
func setUpRecipe(t *testing.T, home, dir, recipeText string) {
	t.Helper()
	checkout := filepath.Join(home, "repos", dir)
	os.MkdirAll(checkout, 0o755)
	os.WriteFile(filepath.Join(checkout, "covey.project.yml"), []byte(recipeText), 0o644)
	real, _ := filepath.EvalSymlinks(checkout)
	os.MkdirAll(envDir(home), 0o700)
	saveEnvRecord(home, envKey(real, ""), envRecord{Checkout: real, Op: "env_up"})
}

func onPath(names ...string) func(string) (string, error) {
	return func(cmd string) (string, error) {
		for _, n := range names {
			if n == cmd {
				return "/usr/bin/" + cmd, nil
			}
		}
		return "", errors.New("not found")
	}
}

func TestRecipeMCPServers(t *testing.T) {
	home := t.TempDir()
	setUpRecipe(t, home, "app", `version: 1
mcp:
  marionette:
    command: marionette_mcp
  dart:
    command: dart
    args: [mcp-server]
  local:
    command: ./tools/mcp.sh
  missing:
    command: nowhere_mcp
`)
	setUpRecipe(t, home, "other", "version: 1\nmcp:\n  dart:\n    command: dart\n")

	servers, started, left := collectRecipeMCP(home, true, onPath("marionette_mcp", "dart"))
	if len(servers) != 2 || servers["dart"].(map[string]any)["args"].([]string)[0] != "mcp-server" {
		t.Fatalf("servers: %+v", servers)
	}
	if len(started) != 2 {
		t.Fatalf("started: %+v", started)
	}
	reasons := map[string]string{}
	for _, l := range left {
		reasons[l.Recipe+"#"+l.Name] = l.Reason
	}
	if len(left) != 3 {
		t.Fatalf("left out: %+v", left)
	}
	for _, l := range left {
		if l.Reason == "" || l.Recipe == "" {
			t.Fatalf("every server left out says why and from where: %+v", l)
		}
	}

	// Without the grant nothing starts, and the recording says so.
	servers, started, left = collectRecipeMCP(home, false, onPath("marionette_mcp", "dart"))
	if servers != nil || len(started) != 0 || len(left) != 5 || !strings.Contains(left[0].Reason, "scope: mcp") {
		t.Fatalf("without the grant: %v %+v %+v", servers, started, left)
	}
}

func TestMCPDocumentCarriesRecipeServersWithoutTheActionRoute(t *testing.T) {
	p := testProxy()
	doc := p.mcpDocument(map[string]any{"marionette": map[string]any{"type": "stdio", "command": "marionette_mcp"},
		"covey": map[string]any{"type": "stdio", "command": "evil"}})
	var cfg struct {
		MCPServers map[string]map[string]any `json:"mcpServers"`
	}
	if err := json.Unmarshal([]byte(doc), &cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.MCPServers["marionette"] == nil {
		t.Fatalf("a recipe server must not wait for COVEY_ACTION_MCP: %s", doc)
	}
	if _, ok := cfg.MCPServers["covey"]; ok {
		t.Fatalf("a recipe cannot take covey's own name: %s", doc)
	}
	if got := mcpAllowed(doc); len(got) != 1 || got[0] != "mcp__marionette" {
		t.Fatalf("allowlist: %v", got)
	}
}

func TestClaudeCodeAllowsEveryServerInTheDocument(t *testing.T) {
	bin, dir := fakeClaude(t, `
cat <<'EOF'
{"type":"result","subtype":"success","session_id":"s","result":"fertig"}
EOF`)
	adapter := &ClaudeCode{Binary: bin}
	if _, err := adapter.Run(context.Background(), RunSpec{
		TaskID: "t", Title: "x", HomeDir: dir, AllowedTools: []string{"Bash"},
		MCPConfig: `{"mcpServers":{"covey":{"type":"http","url":"http://127.0.0.1:1/mcp"},"marionette":{"type":"stdio","command":"marionette_mcp"}}}`,
	}, func(string, json.RawMessage) {}); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(filepath.Join(dir, "args.txt"))
	if !strings.Contains(string(got), "Bash,mcp__covey,mcp__marionette") {
		t.Fatalf("every server's tools belong in the one allowlist:\n%s", got)
	}
}
