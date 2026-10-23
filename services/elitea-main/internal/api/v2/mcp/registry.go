package mcp

// `tools_list` and `tools_call` — legacy `elitea_core/api/v2/tools_list.py` and
// `tools_call.py`.
//
//	GET  /api/v2/elitea_core/tools_list/{projectID}
//	GET  /api/v2/elitea_core/tools_list/default/{projectID}
//	POST /api/v2/elitea_core/tools_call/default/{projectID}
//
// # What these two endpoints are
//
// They are NOT this platform listing its own tools — that is the MCP server in
// server.go. They are the REVERSE direction: a listing of MCP servers that have
// been registered INTO a project, and a way to invoke a tool on one of them.
//
// The Python execution worker reads the listing to build an agent's MCP tools
// (`elitea_sdk/runtime/clients/client.py:get_mcp_toolkits`). Until issue 335
// this service answered 501, so the hybrid edge sent that call to the legacy
// runtime and an agent with an MCP toolkit could not run without the legacy runtime.
//
// # Why the listing can now be served
//
// The legacy store is process memory: two dictionaries in
// `utils/mcp_servers_storage.py`, filled over socket.io and swept when a socket
// goes away. There was no table behind it, in the legacy runtime or here.
//
// There is one now — `elitea_mcp.registered_servers` and
// `elitea_mcp.registered_tools`, shared migration 0073, read and written by
// `internal/mcpregistry`. The write happens when the platform discovers a
// remote MCP server's tools over the streamable-HTTP transport, which is the
// `mcp_sync_tools` path in `internal/api/v2/eliteacore`. So the same service
// that answers this read owns the write.
//
// # Both mode shapes are served
//
// The legacy `api_tools.with_modes` registers a route both with and without the
// mode segment, so `/tools_list/1` and `/tools_list/default/1` are the same
// endpoint. The SDK builds the mode-less form
// (`client.py:105`). Registering only `/default/` would leave the caller that actually exists
// unserved, so both are registered in `internal/api/router.go`.
//
// # An empty list is now a fact about rows
//
// It was not before. `[]` asserts "no MCP server is registered in this
// project", and while the only store was a dictionary in another process this
// service had no way to know that. Now the claim is a read of its own table, so
// a project with no registered server correctly gets `[]` rather than an error.
//
// # tools_call still refuses, and the reason is different from before
//
// See ToolsCall.

import (
	"context"
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/mcpregistry"
)

// ToolRegistryUnavailableReason is the sentence `tools_call` gives.
//
// Exported so the acceptance tests assert the endpoint's stated reason rather
// than just its status code — a 501 with a different body would be a different
// refusal, and pinning the reason is what keeps a later edit from quietly
// turning it into a stub.
const ToolRegistryUnavailableReason = "tools_call dispatches a tool invocation to the MCP server that publishes the " +
	"tool. This service stores which servers a project has registered and which tools they publish, so tools_list " +
	"answers from its own rows, but it deliberately does not store the credentials that authenticate a call to " +
	"those servers, and it runs no socket.io server to reach a server that an Elitea MCP CLIENT hosts. So there is " +
	"nothing here to dispatch with. A remote MCP toolkit does not need this endpoint: the worker calls its server " +
	"directly. To expose THIS platform's agents and toolkits as MCP tools, use the MCP server at " +
	"/app/{project_id}/mcp."

// ToolsList serves the project's registered MCP servers from the durable store.
//
// The response is a bare JSON array of server objects, which is the legacy
// shape: `serialize(mcp_servers)` on a list of `McpServer` models, with no
// envelope. The SDK parses the body directly as a list, so an envelope would
// break every existing worker.
func (h *Handler) ToolsList(w http.ResponseWriter, r *http.Request) {
	// Gated on the master switch before anything is read, like every other
	// route in this package: which subsystems a deployment runs is itself
	// information about it.
	if !h.requireMCPEnabled(w, r) {
		return
	}

	projectID, valid := parseProjectID(chi.URLParam(r, "projectID"))
	if !valid {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid project id"})
		return
	}
	if h.registry == nil {
		writeJSON(w, http.StatusServiceUnavailable,
			map[string]any{"error": "MCP tool registry is not available"})
		return
	}

	ctx := r.Context()
	current, err := h.registry.ListForProject(ctx, projectID)
	if err != nil {
		// A read failure is not an empty project. Answering 200 with `[]` here
		// would tell the worker that the project has no MCP tools, and the
		// agent would run without them and produce a wrong answer rather than
		// fail.
		writeJSON(w, http.StatusServiceUnavailable,
			map[string]any{"error": "MCP tool registry read failed"})
		return
	}

	// The legacy runtime unions the caller's personal project with the current
	// one, and lets the personal entry win a name collision. A user registers a
	// server in their own project and it follows them into every project they work in.
	var personal []mcpregistry.Server
	if personalID := h.personalProjectID(ctx, r); personalID > 0 && personalID != projectID {
		personal, err = h.registry.ListForProject(ctx, personalID)
		if err != nil {
			writeJSON(w, http.StatusServiceUnavailable,
				map[string]any{"error": "MCP tool registry read failed"})
			return
		}
	}

	writeJSON(w, http.StatusOK, mcpregistry.Merge(current, personal))
}

// personalProjectID resolves the caller's personal project, or 0.
//
// A failure resolves to 0 rather than failing the request: the personal project
// is an ADDITION to the current project's listing, and a user whose personal
// project cannot be resolved must still receive the servers registered in the
// project they asked about.
func (h *Handler) personalProjectID(ctx context.Context, r *http.Request) int64 {
	if h.personal == nil {
		return 0
	}
	user, ok := auth.UserFromContext(r.Context())
	if !ok {
		return 0
	}
	userID := user.UserID
	if userID == "" {
		userID = user.ID
	}
	if userID == "" {
		return 0
	}
	resolved, err := h.personal.PersonalProjectID(ctx, userID)
	if err != nil || resolved <= 0 {
		return 0
	}
	return int64(resolved)
}

// ToolsCall serves `POST /elitea_core/tools_call/default/{projectID}`.
//
// It still refuses, and the reason has changed. Before issue 335 both endpoints
// refused because nothing in this stack held the data. The listing now has a
// table, so that half is served. The call does not follow, for two reasons that
// are not "no time":
//
//  1. A call to a remote MCP server carries the headers that authenticate it.
//     This service does not store them, and it must not: bearer credentials do
//     not enter this corpus (AGENTS.md). Dispatching without them would reach
//     every server that needs no authentication and fail on every server that
//     does, which is a worse contract than refusing.
//  2. A server that an Elitea MCP CLIENT hosts is reachable only down the
//     socket.io connection that registered it. This service runs no socket.io
//     server, and AGENTS.md's transport rules do not admit one.
//
// A remote MCP toolkit does not lose anything by this. The worker's remote tool
// calls its server directly over HTTP
// (`elitea_sdk/runtime/tools/mcp_remote_tool.py`) instead of asking the
// platform to relay.
//
// The body is not read. The legacy runtime validates it against
// `McpToolCallPostBody` before looking the server up, but a 400 for a malformed
// body would imply that a well-formed one would be dispatched somewhere, and none is.
func (h *Handler) ToolsCall(w http.ResponseWriter, r *http.Request) {
	if !h.requireMCPEnabled(w, r) {
		return
	}
	writeJSON(w, http.StatusNotImplemented, map[string]any{"error": ToolRegistryUnavailableReason})
}

// parseProjectID accepts only a plain positive integer, which is what the
// legacy runtime's `<int:project_id>` converter accepts.
func parseProjectID(raw string) (int64, bool) {
	value, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || value <= 0 {
		return 0, false
	}
	return value, true
}
