// Package mcpregistry is the durable store of MCP servers registered into a
// project, and the wire shape that `tools_list` serves from it (issue 335).
//
// # What the store replaces
//
// The legacy runtime keeps this data in process memory:
// `utils/mcp_servers_storage.py` holds two dictionaries on the plugin module,
// `project_id → name → McpServer` and `sid → project_id`. A socket.io client
// fills the first one when it connects, and a cron sweep drops entries whose
// socket has gone away. No row exists anywhere, in the legacy runtime or here,
// so the Go service had nothing to read and `tools_list` answered 501. The
// hybrid edge therefore sent worker traffic to the legacy runtime, and an agent
// with an MCP toolkit could not run without it.
//
// This package gives the same data a table. The store is written when the
// platform discovers a remote MCP server's tools, and read when a worker asks
// for the project's tool list.
//
// # What the store does NOT cover, and why that is not hidden
//
// The legacy dictionary has one writer this service cannot have: an Elitea MCP
// CLIENT that opens a socket.io connection and declares the servers it runs on
// the user's own machine. AGENTS.md's transport rules do not admit a socket.io
// server, and this service has never run one. Those registrations stay out of
// reach.
//
// That is why an empty list is now an honest answer where it was not before.
// Before this package, `[]` asserted "no client is connected", which this
// service could not know. Now `[]` reports the rows: no MCP server is
// registered in this project's store. A caller can act on that, because the
// same service that answers the read also owns the write.
package mcpregistry

import "regexp"

// Tool is one tool that an MCP server publishes.
//
// The JSON tags are the wire contract, not a preference. The Python SDK reads
// `name`, `description` and the camelCase `inputSchema` from each entry
// (`elitea_sdk/runtime/toolkits/tools.py:_init_single_mcp_tool`), and gives
// `inputSchema` straight to the tool's argument model.
type Tool struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"inputSchema"`
}

// Server is one registered MCP server, in the exact shape the legacy
// `tools_list` returns.
//
// The legacy runtime serialises the `McpServer` pydantic model
// (`elitea_core/models/mcp.py`) with `model_dump(mode='json')` and returns a
// bare JSON array of them. The SDK reads `name` and `tools`; the remaining
// fields are part of the response the legacy runtime produces, so they stay,
// and a client that does read them keeps working.
//
// SioSID is always null here. It names the socket a tool call would be
// dispatched on, and this service runs no socket.io server. Emitting null
// rather than dropping the field keeps the shape identical and states the fact:
// there is no socket behind this entry.
type Server struct {
	Name             string  `json:"name"`
	Tools            []Tool  `json:"tools"`
	ProjectID        *int64  `json:"project_id"`
	SioSID           *string `json:"sio_sid"`
	TimeoutToolsList int     `json:"timeout_tools_list"`
	TimeoutToolsCall int     `json:"timeout_tools_call"`
	Group            string  `json:"group"`
}

// DefaultTimeoutSeconds is the timeout the legacy model defaults both fields to.
const DefaultTimeoutSeconds = 90

// DefaultGroup is the group the legacy model defaults to.
const DefaultGroup = "Other"

// Registration is one write into the store: a server and the tools it
// publishes, as discovered.
type Registration struct {
	ProjectID        int64
	Name             string
	ServerURL        string
	Group            string
	TimeoutToolsList int
	TimeoutToolsCall int
	Tools            []Tool
}

// invalidNameChars mirrors the legacy `_sanitize_name` in `elitea_core/sio/mcp.py`.
// A worker matches a toolkit against a server name literally, so the two stacks
// must derive the same name from the same input or the same server gets
// different names on each.
var invalidNameChars = regexp.MustCompile(`[^A-Za-z0-9_\-]`)

// SanitizeName applies the legacy rule to a server name.
func SanitizeName(name string) string {
	return invalidNameChars.ReplaceAllString(name, "_")
}

// Merge combines the current project's servers with the caller's personal
// project's servers.
//
// The legacy runtime computes `{**current_servers,
// **private_servers}.values()`, so a personal-project server WINS a name
// collision with a current-project server. That order is preserved: a user's
// own registration overrides a shared one of the same name, which is what lets
// someone test a replacement server without changing the project everyone else uses.
//
// The result keeps the current-project order first, then the personal-only
// entries, so two reads of unchanged rows give the same list.
func Merge(current, personal []Server) []Server {
	byName := make(map[string]int, len(current)+len(personal))
	merged := make([]Server, 0, len(current)+len(personal))
	for _, server := range current {
		byName[server.Name] = len(merged)
		merged = append(merged, server)
	}
	for _, server := range personal {
		if index, collides := byName[server.Name]; collides {
			merged[index] = server
			continue
		}
		byName[server.Name] = len(merged)
		merged = append(merged, server)
	}
	return merged
}
