package daemon

// The stdio MCP servers a project recipe names (spec/30, "Servers out of the
// recipe: mcp:").
//
// A Flutter application renders into a canvas; the browser finds nothing in
// it. What drives it is its VM service, and the tools that speak that are stdio
// servers — processes beside the application, inside the sandbox, which is why
// this is not the MCP target system (internal/target/mcp, HTTP only).
//
// Four rules, each load-bearing:
//
//   - The grant is per agent: `- system: dev scope: mcp`. The control plane
//     decides it from the agent's access and says so in InjectConfig.RecipeMCP;
//     without it no server of any recipe is started.
//   - Which servers come from where goes into the recording. The grant is the
//     agent's, but the list is a file in a repository.
//   - The list is fixed when the run starts: the runtime takes its servers at
//     launch. Recipes count once env_up has set their project up in this home,
//     and from the next run on.
//   - A server is spawned without a shell, so its command has to be on the
//     image PATH. One that is not is left out and named — the alternative is an
//     absence, and nobody investigates an absence.

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
)

// recipeMCPServer is one entry of the recording.
type recipeMCPServer struct {
	Name      string `json:"name"`
	Command   string `json:"command,omitempty"`
	Recipe    string `json:"recipe"`
	Component string `json:"component,omitempty"`
	Reason    string `json:"reason,omitempty"`
}

// recipeMCP collects the servers of the recipes env_up set up in this home and
// records what it did with them. The map is what goes into the runtime's MCP
// document; nil when there is nothing to start.
func (c *Client) recipeMCP(taskID string, granted bool) map[string]any {
	servers, started, left := collectRecipeMCP(c.homeDir, granted, exec.LookPath)
	if len(started) == 0 && len(left) == 0 {
		return nil
	}
	payload, _ := json.Marshal(map[string]any{"kind": "recipe_mcp", "started": started, "left_out": left})
	_ = c.send(TypeEvent, Event{TaskID: taskID, Kind: "mcp", Payload: payload})
	return servers
}

func collectRecipeMCP(home string, granted bool, lookPath func(string) (string, error)) (map[string]any, []recipeMCPServer, []recipeMCPServer) {
	records, _ := filepath.Glob(filepath.Join(envDir(home), "*.json"))
	sort.Strings(records)
	servers := map[string]any{}
	var started, left []recipeMCPServer
	for _, file := range records {
		data, err := os.ReadFile(file)
		if err != nil {
			continue
		}
		var rec envRecord
		if json.Unmarshal(data, &rec) != nil || rec.Checkout == "" {
			continue
		}
		root, err := checkoutRoot(home, rec.Checkout)
		if err != nil {
			continue // a checkout that was removed since
		}
		r, file, _, err := loadRecipe(root)
		if err != nil {
			continue
		}
		stack, _, err := r.Select(rec.Component)
		if err != nil {
			continue
		}
		names := make([]string, 0, len(stack.MCP))
		for name := range stack.MCP {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			srv := stack.MCP[name]
			entry := recipeMCPServer{Name: name, Command: srv.Command, Recipe: file, Component: rec.Component}
			switch {
			case !granted:
				entry.Reason = "the agent's access does not grant `- system: dev scope: mcp`"
			case servers[name] != nil:
				entry.Reason = "another recipe already starts a server under this name"
			case strings.ContainsRune(srv.Command, '/'):
				entry.Reason = "the command is a path; a server is started without a shell and has to be a command on the image PATH"
			default:
				if _, err := lookPath(srv.Command); err != nil {
					entry.Reason = "the command is not on the PATH of the image — a path exported in .bashrc reaches dev exec, not the runtime"
				}
			}
			if entry.Reason != "" {
				left = append(left, entry)
				continue
			}
			doc := map[string]any{"type": "stdio", "command": srv.Command}
			if len(srv.Args) > 0 {
				doc["args"] = srv.Args
			}
			if len(srv.Env) > 0 {
				doc["env"] = srv.Env
			}
			servers[name] = doc
			started = append(started, entry)
		}
	}
	if len(servers) == 0 {
		servers = nil
	}
	return servers, started, left
}
