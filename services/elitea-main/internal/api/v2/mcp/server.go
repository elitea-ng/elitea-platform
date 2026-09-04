package mcp

// The MCP streamable-HTTP transport and protocol dispatch — issue 252 P2/P3.
//
//	GET|POST /app/{projectID}/mcp[/{category}|/{entity}/{entityVersionID}]
//
// Authentication and tenant scope are the router's (`apimw.Auth` plus
// `apimw.RequireProjectAccess`), which is why nothing in this file re-derives
// them: by the time a handler here runs, the caller is an authenticated
// principal with a role in {projectID}. pylon performs the same two checks
// inline (`auth.current_user()` then `list_user_projects`).

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth"
)

// maxRequestBytes bounds one JSON-RPC message. MCP messages are tool names and
// arguments; a megabyte is far above any real `tools/call` payload and far
// below anything that would matter to the process.
const maxRequestBytes = 1 << 20

// supportedProtocolVersions are the MCP revisions this server will name in an
// initialize result, newest first.
//
// pylon pins "2024-11-05" unconditionally. Echoing the client's version when it
// is one we speak is what the specification asks for, and it matters in
// practice: a client that requested 2025-06-18 and is answered 2024-11-05 is
// told, correctly, that the server is older than it is, and several clients
// then downgrade behaviour they did not need to. Everything this server
// implements — initialize, tools/list, tools/call — is identical across all
// three revisions.
var supportedProtocolVersions = []string{"2025-06-18", "2025-03-26", "2024-11-05"}

// sseUnavailableMessage is pylon's exact wording for a GET on this path.
const sseUnavailableMessage = "The server does not offer an SSE stream at this endpoint"

// Endpoint serves every MCP protocol request, in all three scopes.
func (h *Handler) Endpoint(w http.ResponseWriter, r *http.Request) {
	if !h.mcpEnabled(r.Context()) {
		writeJSON(w, http.StatusForbidden, map[string]any{"error": mcpDisabledMessage})
		return
	}
	schema, ok := projectSchema(chi.URLParam(r, "projectID"))
	if !ok {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid project id"})
		return
	}

	// A GET on a streamable-HTTP endpoint is the client asking to open the
	// server→client notification stream. This server never initiates a
	// message: it has no subscriptions, no progress notifications and no
	// listChanged events (see the capabilities below), so there is nothing to
	// stream and 405 is the specification's answer for exactly that case. pylon
	// refuses the same way, with this same sentence.
	if r.Method == http.MethodGet {
		w.Header().Set("Allow", http.MethodPost)
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": sseUnavailableMessage})
		return
	}

	requestScope, err := parseScope(chi.URLParam(r, "*"))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, maxRequestBytes+1))
	if err != nil {
		writeRPC(w, http.StatusOK, newError(nil, codeParseError, "could not read request body"))
		return
	}
	if len(body) > maxRequestBytes {
		writeRPC(w, http.StatusOK, newError(nil, codeInvalidRequest, "request body too large"))
		return
	}

	message, err := decodeMessage(body)
	if err != nil {
		code, text := codeParseError, "invalid JSON"
		if errors.Is(err, errBatchUnsupported) {
			code, text = codeInvalidRequest,
				"JSON-RPC batches are not supported; send one message per request"
		}
		writeRPC(w, http.StatusOK, newError(nil, code, text))
		return
	}

	// A notification carries no id, so there is no response to correlate. The
	// specification requires 202 with an empty body; pylon answers 200 with
	// `{}`, which clients tolerate but which is indistinguishable from a
	// successful request-response and confuses any client that checks. This is
	// the one place this port deliberately does not copy pylon's status code.
	//
	// The message is not otherwise acted on: the only notification a client
	// sends this server is `notifications/initialized`, which is an
	// acknowledgement, and a stateless server has no handshake state to
	// advance with it.
	if message.isNotification() {
		w.WriteHeader(http.StatusAccepted)
		return
	}

	if message.JSONRPC != "2.0" || message.Method == "" {
		writeRPC(w, http.StatusOK, newError(message.ID, codeInvalidRequest, "not a JSON-RPC 2.0 request"))
		return
	}

	writeRPC(w, http.StatusOK, h.dispatch(r, schema, requestScope, message))
}

// dispatch routes one JSON-RPC request to its method handler.
func (h *Handler) dispatch(r *http.Request, schema string, s scope, message rpcMessage) rpcResponse {
	switch message.Method {
	case "initialize":
		return newResult(message.ID, initializeResult(s, message.Params))
	case "ping":
		// An empty result object is the whole of the ping contract.
		return newResult(message.ID, map[string]any{})
	case "tools/list":
		return h.listTools(r, schema, s, message)
	case "tools/call":
		return h.callTool(r, schema, s, message)
	default:
		return newError(message.ID, codeMethodNotFound,
			"'"+message.Method+"' is not supported by this server")
	}
}

// initializeResult answers the handshake.
//
// The capabilities are the ones this server actually has, which is a shorter
// list than pylon's. pylon declares `logging`, `resources`, `prompts` and
// `tools.listChanged: true`; it implements none of them — `logging/setLevel`
// returns an empty result and nothing is ever logged to the client, there is no
// resource or prompt handler at all, and no listChanged notification is ever
// sent (it could not be: the HTTP path has no open stream to send it on).
// Declaring a capability is a promise a client acts on — it will send
// `resources/list`, or wait for a `listChanged` that never arrives — so only
// `tools` is declared here, with `listChanged: false`.
func initializeResult(s scope, params json.RawMessage) map[string]any {
	name, instructions := s.serverIdentity()
	return map[string]any{
		"protocolVersion": negotiateProtocolVersion(params),
		"capabilities": map[string]any{
			"tools": map[string]any{"listChanged": false},
		},
		"serverInfo": map[string]any{
			"name":    name,
			"version": "0.1.0",
		},
		"instructions": instructions,
	}
}

// negotiateProtocolVersion echoes the client's requested revision when this
// server speaks it, and otherwise names the newest revision it does speak —
// which is what the specification tells the client to check and, if it cannot
// live with, disconnect over.
func negotiateProtocolVersion(params json.RawMessage) string {
	var parsed struct {
		ProtocolVersion string `json:"protocolVersion"`
	}
	if len(params) > 0 {
		_ = json.Unmarshal(params, &parsed) // a malformed params object just means "no preference"
	}
	for _, version := range supportedProtocolVersions {
		if parsed.ProtocolVersion == version {
			return version
		}
	}
	return supportedProtocolVersions[0]
}

func (h *Handler) listTools(r *http.Request, schema string, s scope, message rpcMessage) rpcResponse {
	tools, err := h.source.tools(r.Context(), schema, s)
	if err != nil {
		// The cause is logged by the recovery/telemetry middleware; the wire
		// gets a typed message and never `err.Error()`.
		return newError(message.ID, codeInternalError, "tool listing is temporarily unavailable")
	}
	sortToolsByName(tools)
	if tools == nil {
		tools = []Tool{}
	}
	// `nextCursor` is omitted rather than sent as null: the field is optional
	// and a null value makes some clients paginate into a second request.
	return newResult(message.ID, map[string]any{"tools": tools})
}

// ToolExecutionUnavailableReason is what a `tools/call` gets back on a
// deployment with NO AGENT RUNTIME — `runtime.enabled` off, so the composition
// root has no `AgentStart` use case to hand this package and `h.start` is nil.
//
// Exported so the acceptance tests pin the stated reason, not just the shape.
//
// IT IS UNCHANGED, byte for byte, from before agent execution was wired, and
// that is deliberate rather than incidental: on a runtime-less deployment
// nothing about what this server can do has changed, so the sentence its
// clients already have must not change either. Both execution paths pylon has
// are still out of reach there:
//
//   - an AGENT tool runs `do_predict`, the pylon prediction entry point. The
//     transport that reached it from Go was removed in issue 126 and its
//     replacement — the Redis command stream and the Python worker — is not
//     running at all on such a deployment.
//   - a TOOLKIT tool runs `do_runtool`, which dispatches into the SDK toolkit
//     the worker holds. Same transport, same state.
//
// With the runtime ENABLED the two halves separate: an agent tool runs (see
// execute.go), and a toolkit tool gets ToolkitExecutionUnavailableReason, which
// refuses only the half that is genuinely still missing.
//
// It is returned as a CallToolResult with `isError: true` rather than a
// JSON-RPC error because that is what the specification reserves for a tool
// that ran and failed, and it is what puts the sentence in front of the model
// driving the client instead of only in the client's console.
const ToolExecutionUnavailableReason = "this MCP server can list this project's tools but cannot run them yet: " +
	"executing an agent tool requires the agent runtime and executing a toolkit tool requires the Python worker's " +
	"toolkit dispatch, and neither is reachable from this service. Nothing was executed and nothing was changed."

func (h *Handler) callTool(r *http.Request, schema string, s scope, message rpcMessage) rpcResponse {
	var params struct {
		Name      string         `json:"name"`
		Arguments map[string]any `json:"arguments"`
	}
	decoder := json.NewDecoder(bytes.NewReader(message.Params))
	decoder.UseNumber()
	if err := decoder.Decode(&params); err != nil || strings.TrimSpace(params.Name) == "" {
		return newError(message.ID, codeInvalidParams, "tools/call requires a 'name' parameter")
	}
	if params.Arguments == nil {
		params.Arguments = map[string]any{}
	}

	// The name is resolved against the SAME listing tools/list serves, in the
	// same scope. Without that, a call scoped to one toolkit could name a tool
	// belonging to another, and the eventual executor would have been handed a
	// tool the caller was never shown.
	tools, err := h.source.tools(r.Context(), schema, s)
	if err != nil {
		return newError(message.ID, codeInternalError, "tool lookup is temporarily unavailable")
	}
	var target Tool
	found := false
	for _, tool := range tools {
		if tool.Name == params.Name {
			target = tool
			found = true
			break
		}
	}
	if !found {
		// Unknown name is a caller error, so it is a protocol error rather than
		// an isError result — the distinction the specification draws is
		// "the request was wrong" versus "the tool ran and failed".
		return newError(message.ID, codeInvalidParams, "unknown tool: "+params.Name)
	}

	projectID, ok := runProjectID(r)
	if !ok {
		// Unreachable in practice: Endpoint already refused a project id that
		// is not a plain positive integer before this handler was reached.
		return newError(message.ID, codeInvalidParams, "invalid project id")
	}
	if target.internalApplicationOperation != "" {
		return newResult(message.ID, h.callInternalApplicationTool(r, projectID, target, params.Arguments))
	}
	if target.internalSkillOperation != "" {
		return newResult(message.ID, h.callInternalSkillTool(r, projectID, target, params.Arguments))
	}

	// NO RUNTIME. The composition root had no AgentStart use case to give this
	// handler, which is what `runtime.enabled` being off looks like from here.
	// The answer is the sentence this endpoint has always given, unchanged —
	// see ToolExecutionUnavailableReason.
	if h.start == nil {
		return newResult(message.ID, errorResult(ToolExecutionUnavailableReason))
	}
	// A TOOLKIT tool. The half this change does not attempt.
	if !target.runnableAgent() {
		return newResult(message.ID, errorResult(ToolkitExecutionUnavailableReason))
	}

	task, _ := params.Arguments["task"].(string)
	task = strings.TrimSpace(task)
	if task == "" {
		// `task` is `required` in the schema this very server published, so an
		// empty one is the request being wrong rather than the tool failing.
		return newError(message.ID, codeInvalidParams,
			"tools/call on an agent requires a non-empty 'task' argument")
	}

	actorUserID, refusal := h.authorizeRun(r, projectID)
	if refusal != nil {
		return newResult(message.ID, refusal)
	}

	return newResult(message.ID, h.runAgentTool(r.Context(), schema, projectID, actorUserID, target, task))
}

func (h *Handler) callInternalApplicationTool(
	r *http.Request,
	projectID int64,
	target Tool,
	arguments map[string]any,
) map[string]any {
	if h.internalApplications == nil {
		return errorResult("this deployment cannot execute internal application tools; nothing was executed")
	}
	return h.callInternalTool(r, projectID, target, arguments, "application", func(actorID int64) (internalApplicationExecution, error) {
		return h.internalApplications.Execute(
			r.Context(), projectID, actorID, target.internalApplicationOperation, arguments,
		)
	})
}

func (h *Handler) callInternalSkillTool(
	r *http.Request,
	projectID int64,
	target Tool,
	arguments map[string]any,
) map[string]any {
	if h.internalSkills == nil {
		return errorResult("this deployment cannot execute internal skill tools; nothing was executed")
	}
	return h.callInternalTool(r, projectID, target, arguments, "skill", func(actorID int64) (internalApplicationExecution, error) {
		return h.internalSkills.Execute(
			r.Context(), projectID, actorID, target.internalSkillOperation, arguments,
		)
	})
}

func (h *Handler) callInternalTool(
	r *http.Request,
	projectID int64,
	target Tool,
	arguments map[string]any,
	category string,
	execute func(int64) (internalApplicationExecution, error),
) map[string]any {
	if arguments == nil {
		arguments = make(map[string]any)
	}
	if supplied, present := arguments["project_id"]; present {
		text := scalarArgument(supplied)
		parsed, err := strconv.ParseInt(text, 10, 64)
		if err != nil || parsed <= 0 {
			return errorResult("project_id must be a positive integer; nothing was executed")
		}
		if parsed != projectID {
			return errorResult("project_id must match the project in this MCP endpoint; nothing was executed")
		}
	}
	// The path is the authority. Injection also lets an MCP client omit the
	// otherwise repetitive project field without creating a second defaulting
	// rule in each operation.
	arguments["project_id"] = json.Number(strconv.FormatInt(projectID, 10))

	if target.permission == "" {
		return errorResult("this internal MCP tool has no permission contract, so nothing was executed")
	}
	actorID, refusal := h.authorizePermission(
		r, projectID, target.permission, "using internal MCP tool '"+target.Name+"'",
	)
	if refusal != nil {
		return refusal
	}
	execution, err := execute(actorID)
	if err != nil {
		return errorResult("the internal " + category + " operation failed; nothing else was disclosed")
	}
	if execution.status >= http.StatusInternalServerError || !json.Valid(execution.body) {
		return errorResult("the internal " + category + " operation failed; nothing else was disclosed")
	}
	text := strings.TrimSpace(string(execution.body))
	if text == "" {
		text = "{}"
	}
	if execution.status < http.StatusOK || execution.status >= http.StatusMultipleChoices {
		return errorResult(text)
	}
	return map[string]any{
		"content": []map[string]any{{"type": "text", "text": text}},
	}
}

// authorizeRun decides whether this caller may EXECUTE in this project, and
// resolves the user the turn is attributed to.
//
// It returns either an actor id or a CallToolResult to send instead. The
// refusal is a result rather than a JSON-RPC error on purpose: the request was
// well formed and the model driving the client is the one that has to read why
// nothing ran.
//
// The permission is `models.chat.messages.create` in the DEFAULT mode — the
// same string and the same mode the chat start route requires
// (agentexecution.CurrentApplicationStartPermission /
// CurrentApplicationStartMode), restated here rather than imported because
// importing the route package from this one would point the dependency the
// wrong way. If those constants ever change, TestMCPRunUsesTheChatStartPermission
// fails.
func (h *Handler) authorizeRun(r *http.Request, projectID int64) (int64, map[string]any) {
	return h.authorizePermission(r, projectID, runPermission, "running an agent")
}

func (h *Handler) authorizePermission(
	r *http.Request,
	projectID int64,
	permission string,
	action string,
) (int64, map[string]any) {
	user, ok := auth.UserFromContext(r.Context())
	if !ok {
		return 0, errorResult(action + " requires an authenticated caller; nothing was executed")
	}
	if h.permissions == nil {
		// FAIL CLOSED. A handler with no resolver cannot decide, and "cannot
		// decide" must not mean "allowed" for the one capability on this
		// endpoint that spends money and drives tools.
		return 0, errorResult("this deployment cannot authorize " + action + ", so nothing was executed")
	}
	resolution, err := h.permissions.ResolvePermissions(
		r.Context(), user, runPermissionMode, strconv.FormatInt(projectID, 10),
	)
	if err != nil {
		return 0, errorResult("your permissions for this project could not be resolved, so nothing was executed")
	}
	allowed := false
	for _, heldPermission := range resolution.Permissions {
		if heldPermission == permission {
			allowed = true
			break
		}
	}
	if !allowed {
		return 0, errorResult(action + " in this project requires the '" + permission +
			"' permission, which this caller does not hold. Nothing was executed.")
	}
	if resolution.UserID <= 0 {
		// The resolver's contract is to populate this on success. A zero here
		// is a resolver bug, and admitting a turn authored by user 0 would
		// write chat rows nobody can be held to.
		return 0, errorResult("your user identity could not be resolved, so nothing was executed")
	}
	return resolution.UserID, nil
}

const (
	// runPermission and runPermissionMode are agentexecution's
	// CurrentApplicationStartPermission and CurrentApplicationStartMode. See
	// authorizeRun for why they are restated rather than imported.
	runPermission     = "models.chat.messages.create"
	runPermissionMode = auth.PermissionModeDefault
)

// runProjectID re-reads the project id from the URL as a number.
//
// The handler chain carries the tenant SCHEMA, which is a quoted identifier and
// cannot be turned back into an id; the admission contract needs the number.
// Both come from the same URL parameter through the same validation, so they
// cannot name different projects.
func runProjectID(r *http.Request) (int64, bool) {
	parsed, err := strconv.ParseInt(chi.URLParam(r, "projectID"), 10, 64)
	if err != nil || parsed <= 0 {
		return 0, false
	}
	return parsed, true
}

func writeRPC(w http.ResponseWriter, status int, response rpcResponse) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(response)
}
