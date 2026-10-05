package mcp_test

// Acceptance for `GET /api/v2/elitea_core/tools_list/{projectID}` (issue 335).
//
// The endpoint used to answer 501. It now reads
// `elitea_mcp.registered_servers` and `elitea_mcp.registered_tools`, so every
// case here asserts what the endpoint says about ROWS.
//
// A status-code test would pass against a handler that answered a hardcoded
// list, or an empty one, forever. Issue 128 is this repository's file of
// exactly that defect, and a tool listing hides it well because "no tools"
// looks like a legitimate answer. So the discriminating assertions are:
//
//   - the served tool names, descriptions and schemas are the stored values;
//   - changing a stored row changes the response, and DELETING a stored row
//     removes it, so the handler cannot be returning a constant;
//   - a server stored in another project does not appear;
//   - the caller's personal project is unioned in, and wins a name collision;
//   - a project with no rows gets `[]`, not an error and not `null`.
//
// The final case drives the whole path with no pylon present: a remote MCP
// server, the platform's own discovery endpoint, the rows it writes, and the
// listing a worker then reads.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	apimw "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/middleware"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/eliteacore"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/mcp"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/mcpregistry"
)

/* ── helpers ───────────────────────────────────────────────────────────── */

// storeServer writes a registration through the production store, so the tests
// exercise the same writer the discovery path uses.
func storeServer(t *testing.T, pool *pgxpool.Pool, projectID int64, name string, tools ...mcpregistry.Tool) {
	t.Helper()
	if err := mcpregistry.NewStore(pool).Save(context.Background(), mcpregistry.Registration{
		ProjectID: projectID,
		Name:      name,
		ServerURL: "https://mcp.example.com/mcp",
		Tools:     tools,
	}); err != nil {
		t.Fatalf("store server %q in project %d: %v", name, projectID, err)
	}
}

func tool(name, description string, required ...string) mcpregistry.Tool {
	properties := map[string]any{}
	for _, field := range required {
		properties[field] = map[string]any{"type": "string"}
	}
	needed := make([]any, 0, len(required))
	for _, field := range required {
		needed = append(needed, field)
	}
	return mcpregistry.Tool{
		Name:        name,
		Description: description,
		InputSchema: map[string]any{"type": "object", "properties": properties, "required": needed},
	}
}

// listServers performs the real GET and returns the decoded array.
//
// The body is decoded as a slice, not a map. That is itself an assertion: the
// SDK parses this response directly as a list
// (`elitea_sdk/runtime/clients/client.py:get_mcp_toolkits`), so an envelope
// would break every worker, and this decode fails if one appears.
func listServers(t *testing.T, router chi.Router, target string) []map[string]any {
	t.Helper()
	recorder := do(t, router, http.MethodGet, target, "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("GET %s: status = %d (%s)", target, recorder.Code, recorder.Body.String())
	}
	var servers []map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &servers); err != nil {
		t.Fatalf("GET %s: body is not a JSON array: %v (%s)", target, err, recorder.Body.String())
	}
	return servers
}

func findServer(t *testing.T, servers []map[string]any, name string) map[string]any {
	t.Helper()
	for _, server := range servers {
		if server["name"] == name {
			return server
		}
	}
	t.Fatalf("server %q missing from %v", name, serverNames(servers))
	return nil
}

func serverNames(servers []map[string]any) []string {
	names := make([]string, 0, len(servers))
	for _, server := range servers {
		name, _ := server["name"].(string)
		names = append(names, name)
	}
	return names
}

func serverToolNames(t *testing.T, server map[string]any) []string {
	t.Helper()
	entries, ok := server["tools"].([]any)
	if !ok {
		t.Fatalf("server %v has no tools array", server["name"])
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		item, _ := entry.(map[string]any)
		name, _ := item["name"].(string)
		names = append(names, name)
	}
	return names
}

const toolsListTarget = "/api/v2/elitea_core/tools_list/"

/* ── the listing is the stored rows ────────────────────────────────────── */

// The core claim: what the endpoint serves is what the tables hold. Every
// field the SDK reads is compared against the value that was stored.
func TestToolsListServesTheStoredServerAndItsTools(t *testing.T) {
	pool := newMCPPool(t)
	router := newRouter(mcp.NewHandler(pool, apimw.NewDBPersonalProjectResolver(pool), nil, nil), callerUserID)

	storeServer(t, pool, 1, "mcp_github",
		tool("get_issue", "Read one issue", "issue_id"),
		tool("list_issues", "List the issues"))

	servers := listServers(t, router, toolsListTarget+homeProject)
	server := findServer(t, servers, "mcp_github")

	if got := serverToolNames(t, server); len(got) != 2 || got[0] != "get_issue" || got[1] != "list_issues" {
		t.Fatalf("tools = %v, want the two stored tools in stored order", got)
	}

	entries, _ := server["tools"].([]any)
	first, _ := entries[0].(map[string]any)
	if first["description"] != "Read one issue" {
		t.Fatalf("description = %v, want the stored description", first["description"])
	}
	schema, ok := first["inputSchema"].(map[string]any)
	if !ok {
		t.Fatalf("inputSchema missing or not an object: %v", first["inputSchema"])
	}
	properties, ok := schema["properties"].(map[string]any)
	if !ok || properties["issue_id"] == nil {
		t.Fatalf("inputSchema.properties = %v, want the stored property issue_id", schema["properties"])
	}

	// The whole point of the store is that a worker can reach it without
	// pylon. Nothing in this test starts a pylon process, opens a socket, or
	// seeds a fixture the handler could be reading instead of the tables.
	var storedTools int
	if err := pool.QueryRow(context.Background(), `
		SELECT count(*)
		FROM elitea_mcp.registered_tools AS tool
		JOIN elitea_mcp.registered_servers AS server ON server.id = tool.server_id
		WHERE server.project_id = 1 AND server.name = 'mcp_github'`).Scan(&storedTools); err != nil {
		t.Fatalf("count stored tools: %v", err)
	}
	if storedTools != 2 {
		t.Fatalf("stored tool rows = %d, want 2", storedTools)
	}
}

// Deleting a stored row must remove the tool from the response. This is what
// separates "reads the database" from "returns a plausible constant" — a
// hardcoded listing passes the test above and fails this one.
func TestToolsListFollowsTheStoredRows(t *testing.T) {
	pool := newMCPPool(t)
	router := newRouter(mcp.NewHandler(pool, apimw.NewDBPersonalProjectResolver(pool), nil, nil), callerUserID)
	ctx := context.Background()

	storeServer(t, pool, 1, "mcp_github",
		tool("get_issue", "Read one issue"),
		tool("list_issues", "List the issues"))

	before := serverToolNames(t, findServer(t,
		listServers(t, router, toolsListTarget+homeProject), "mcp_github"))
	if len(before) != 2 {
		t.Fatalf("before the edit: tools = %v, want 2", before)
	}

	if _, err := pool.Exec(ctx, `
		UPDATE elitea_mcp.registered_tools SET name = 'get_issue_renamed'
		WHERE name = 'get_issue'`); err != nil {
		t.Fatalf("rename tool row: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`DELETE FROM elitea_mcp.registered_tools WHERE name = 'list_issues'`); err != nil {
		t.Fatalf("delete tool row: %v", err)
	}

	after := serverToolNames(t, findServer(t,
		listServers(t, router, toolsListTarget+homeProject), "mcp_github"))
	if len(after) != 1 || after[0] != "get_issue_renamed" {
		t.Fatalf("after the edit: tools = %v, want only the renamed row", after)
	}
}

// A project with no registered server gets an empty ARRAY. Not an error, and
// not `null`: the SDK iterates the body, and `null` is not iterable.
func TestToolsListReturnsAnEmptyArrayForAProjectWithNoServer(t *testing.T) {
	pool := newMCPPool(t)
	router := newRouter(mcp.NewHandler(pool, apimw.NewDBPersonalProjectResolver(pool), nil, nil), callerUserID)

	recorder := do(t, router, http.MethodGet, toolsListTarget+homeProject, "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)", recorder.Code, recorder.Body.String())
	}
	if body := recorder.Body.String(); body != "[]\n" {
		t.Fatalf("body = %q, want an empty JSON array", body)
	}
}

// The mode-less URL is the one the Python execution worker builds, and the one
// the hybrid edge matches. Both shapes must reach the same handler, or the
// caller that actually exists gets a 404.
func TestToolsListServesBothModeShapes(t *testing.T) {
	pool := newMCPPool(t)
	router := newRouter(mcp.NewHandler(pool, apimw.NewDBPersonalProjectResolver(pool), nil, nil), callerUserID)
	storeServer(t, pool, 1, "mcp_github", tool("get_issue", "Read one issue"))

	for _, target := range []string{
		toolsListTarget + homeProject,
		toolsListTarget + "default/" + homeProject,
	} {
		names := serverToolNames(t, findServer(t, listServers(t, router, target), "mcp_github"))
		if len(names) != 1 || names[0] != "get_issue" {
			t.Fatalf("%s: tools = %v, want the stored tool", target, names)
		}
	}
}

// Rows belonging to another project must not appear. The store is one shared
// table, so the tenant boundary is a predicate rather than a schema, and a
// predicate is exactly the kind of thing that gets dropped.
func TestToolsListDoesNotServeAnotherProjectsServers(t *testing.T) {
	pool := newMCPPool(t)
	router := newRouter(mcp.NewHandler(pool, apimw.NewDBPersonalProjectResolver(pool), nil, nil), callerUserID)

	storeServer(t, pool, 2, "mcp_other_project", tool("secret_tool", "Not for project 1"))

	servers := listServers(t, router, toolsListTarget+homeProject)
	for _, server := range servers {
		if server["name"] == "mcp_other_project" {
			t.Fatalf("project 2's server leaked into project 1's listing: %v", serverNames(servers))
		}
	}

	// The row exists; it is the read that excludes it, not the write that
	// failed. Without this check the case would also pass if Save silently
	// stored nothing.
	var stored int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM elitea_mcp.registered_servers WHERE project_id = 2`).Scan(&stored); err != nil {
		t.Fatalf("count project 2 servers: %v", err)
	}
	if stored != 1 {
		t.Fatalf("project 2 server rows = %d, want 1", stored)
	}
}

/* ── the personal-project union ────────────────────────────────────────── */

// seedPersonalProject creates `project_user_<id>` and makes the user a member,
// which is what the resolver requires (it mirrors pylon's
// `projects_get_personal_project_id`).
func seedPersonalProject(t *testing.T, pool *pgxpool.Pool, userID int64) int64 {
	t.Helper()
	ctx := context.Background()
	seedUser(t, pool, userID)

	var projectID int64
	if err := pool.QueryRow(ctx, `
		INSERT INTO centry.project (name, owner_id, create_success)
		VALUES ($1, $2, true) RETURNING id`,
		fmt.Sprintf("project_user_%d", userID), userID).Scan(&projectID); err != nil {
		t.Fatalf("seed personal project: %v", err)
	}
	// The membership row needs a role of this project. The resolver only asks
	// whether the user holds ANY role there, so one editor role is enough.
	var roleID int64
	if err := pool.QueryRow(ctx, `
		INSERT INTO auth_core__project_role (project_id, name) VALUES ($1, 'editor')
		ON CONFLICT (project_id, name) DO UPDATE SET name = EXCLUDED.name
		RETURNING id`, projectID).Scan(&roleID); err != nil {
		t.Fatalf("seed project role: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO auth_core__project_user_role (project_id, user_id, role_id)
		VALUES ($1, $2, $3) ON CONFLICT DO NOTHING`, projectID, userID, roleID); err != nil {
		t.Fatalf("seed personal project membership: %v", err)
	}
	return projectID
}

// pylon merges the caller's personal project into every listing, and lets the
// personal entry WIN a name collision
// (`{**current_servers, **private_servers}`). Both halves are asserted: a
// personal-only server appears, and a colliding name serves the personal tools.
func TestToolsListUnionsThePersonalProjectAndPersonalWinsACollision(t *testing.T) {
	pool := newMCPPool(t)
	router := newRouter(mcp.NewHandler(pool, apimw.NewDBPersonalProjectResolver(pool), nil, nil), callerUserID)

	personalProject := seedPersonalProject(t, pool, callerUserID)
	if personalProject == 1 {
		t.Fatalf("personal project resolved to the current project; the case would not discriminate")
	}

	storeServer(t, pool, 1, "mcp_shared", tool("shared_tool", "From the current project"))
	storeServer(t, pool, personalProject, "mcp_shared", tool("personal_tool", "From the personal project"))
	storeServer(t, pool, personalProject, "mcp_private", tool("private_tool", "Personal only"))

	servers := listServers(t, router, toolsListTarget+homeProject)

	shared := serverToolNames(t, findServer(t, servers, "mcp_shared"))
	if len(shared) != 1 || shared[0] != "personal_tool" {
		t.Fatalf("mcp_shared tools = %v, want the personal project's tool to win", shared)
	}
	private := serverToolNames(t, findServer(t, servers, "mcp_private"))
	if len(private) != 1 || private[0] != "private_tool" {
		t.Fatalf("mcp_private tools = %v, want the personal-only server", private)
	}

	// Another user must not receive those personal servers.
	otherRouter := newRouter(mcp.NewHandler(pool, apimw.NewDBPersonalProjectResolver(pool), nil, nil), callerUserID+1)
	otherServers := listServers(t, otherRouter, toolsListTarget+homeProject)
	for _, server := range otherServers {
		if server["name"] == "mcp_private" {
			t.Fatalf("another user received the caller's personal server: %v", serverNames(otherServers))
		}
	}
	otherShared := serverToolNames(t, findServer(t, otherServers, "mcp_shared"))
	if len(otherShared) != 1 || otherShared[0] != "shared_tool" {
		t.Fatalf("another user's mcp_shared tools = %v, want the current project's tool", otherShared)
	}
}

/* ── the write path, end to end, with no pylon ─────────────────────────── */

// fakeMCPServer answers the streamable-HTTP handshake and one tools/list.
//
// It stands in for a real MCP server, which is the ONLY external component
// this path needs. There is no pylon process, no socket.io connection and no
// indexer worker anywhere in this test.
func fakeMCPServer(t *testing.T, tools string) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			ID     *int   `json:"id"`
			Method string `json:"method"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		switch request.Method {
		case "initialize":
			w.Header().Set("Mcp-Session-Id", "test-session")
			_, _ = fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%d,"result":{"protocolVersion":"2025-06-18",`+
				`"capabilities":{"tools":{}},"serverInfo":{"name":"fake","version":"1"}}}`, *request.ID)
		case "notifications/initialized":
			w.WriteHeader(http.StatusAccepted)
		case "tools/list":
			_, _ = fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%d,"result":{"tools":%s}}`, *request.ID, tools)
		default:
			_, _ = fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%d,"error":{"code":-32601,"message":"no such method"}}`,
				*request.ID)
		}
	}))
	t.Cleanup(server.Close)
	return server
}

// The acceptance criterion of issue 335, end to end and with no pylon: register
// an MCP toolkit in a project, then read the project's tool list.
//
// The assertion in the middle is the database, not the status code. If
// mcp_sync_tools answered 200 and stored nothing — this repository's recurring
// defect — the row count would be zero and this test would fail there, before
// the listing is ever read.
func TestMCPSyncToolsStoresTheToolsAndToolsListServesThem(t *testing.T) {
	pool := newMCPPool(t)
	ctx := context.Background()

	remote := fakeMCPServer(t, `[
		{"name":"create_page","description":"Create a page",
		 "inputSchema":{"type":"object","properties":{"title":{"type":"string"}},"required":["title"]}},
		{"name":"search","description":"Search pages",
		 "inputSchema":{"type":"object","properties":{"query":{"type":"string"}},"required":["query"]}}
	]`)

	syncRouter := chi.NewRouter()
	syncRouter.Post("/api/v2/elitea_core/mcp_sync_tools/prompt_lib/{projectID}",
		newLoopbackSyncHandler(pool).MCPSyncTools)

	body := fmt.Sprintf(`{"url":%q,"toolkit_type":"mcp_confluence"}`, remote.URL)
	recorder := do(t, syncRouter, http.MethodPost,
		"/api/v2/elitea_core/mcp_sync_tools/prompt_lib/"+homeProject, body)
	if recorder.Code != http.StatusOK {
		t.Fatalf("mcp_sync_tools: status = %d (%s)", recorder.Code, recorder.Body.String())
	}
	if decoded := decode(t, recorder); decoded["success"] != true {
		t.Fatalf("mcp_sync_tools: success = %v (%s)", decoded["success"], recorder.Body.String())
	}

	// THE DATABASE, NOT THE RESPONSE. A route that answered 200 while doing
	// nothing would pass every check above this line.
	rows, err := pool.Query(ctx, `
		SELECT tool.name, tool.description, tool.input_schema #>> '{properties,title,type}'
		FROM elitea_mcp.registered_tools AS tool
		JOIN elitea_mcp.registered_servers AS server ON server.id = tool.server_id
		WHERE server.project_id = 1 AND server.name = 'mcp_confluence'
		ORDER BY tool.ordinal`)
	if err != nil {
		t.Fatalf("read stored tools: %v", err)
	}
	defer rows.Close()
	type storedTool struct{ name, description, titleType string }
	var stored []storedTool
	for rows.Next() {
		var entry storedTool
		var titleType *string
		if err := rows.Scan(&entry.name, &entry.description, &titleType); err != nil {
			t.Fatalf("scan stored tool: %v", err)
		}
		if titleType != nil {
			entry.titleType = *titleType
		}
		stored = append(stored, entry)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate stored tools: %v", err)
	}
	if len(stored) != 2 {
		t.Fatalf("stored tools = %v, want the two the MCP server published", stored)
	}
	if stored[0].name != "create_page" || stored[0].description != "Create a page" {
		t.Fatalf("first stored tool = %+v, want create_page", stored[0])
	}
	// The argument schema is stored as the server sent it, not flattened.
	if stored[0].titleType != "string" {
		t.Fatalf("stored inputSchema lost properties.title.type: %+v", stored[0])
	}
	if stored[1].name != "search" {
		t.Fatalf("second stored tool = %+v, want search", stored[1])
	}

	// And the listing a worker reads serves exactly those rows.
	listRouter := newRouter(mcp.NewHandler(pool, apimw.NewDBPersonalProjectResolver(pool), nil, nil), callerUserID)
	names := serverToolNames(t, findServer(t,
		listServers(t, listRouter, toolsListTarget+homeProject), "mcp_confluence"))
	if len(names) != 2 || names[0] != "create_page" || names[1] != "search" {
		t.Fatalf("tools_list served %v, want the two stored tools", names)
	}
}

// A second discovery must REPLACE the tool set. A tool the server no longer
// publishes has to disappear, because a worker that still sees it builds a tool
// that cannot run.
func TestMCPSyncToolsReplacesTheStoredToolSet(t *testing.T) {
	pool := newMCPPool(t)
	ctx := context.Background()

	syncRouter := chi.NewRouter()
	syncRouter.Post("/api/v2/elitea_core/mcp_sync_tools/prompt_lib/{projectID}",
		newLoopbackSyncHandler(pool).MCPSyncTools)

	sync := func(tools string) {
		t.Helper()
		remote := fakeMCPServer(t, tools)
		recorder := do(t, syncRouter, http.MethodPost,
			"/api/v2/elitea_core/mcp_sync_tools/prompt_lib/"+homeProject,
			fmt.Sprintf(`{"url":%q,"toolkit_type":"mcp_confluence"}`, remote.URL))
		if recorder.Code != http.StatusOK {
			t.Fatalf("mcp_sync_tools: status = %d (%s)", recorder.Code, recorder.Body.String())
		}
	}

	sync(`[{"name":"old_tool","description":"Withdrawn","inputSchema":{"type":"object"}},
	       {"name":"kept_tool","description":"Still there","inputSchema":{"type":"object"}}]`)
	sync(`[{"name":"kept_tool","description":"Still there","inputSchema":{"type":"object"}},
	       {"name":"new_tool","description":"Added","inputSchema":{"type":"object"}}]`)

	rows, err := pool.Query(ctx, `
		SELECT tool.name
		FROM elitea_mcp.registered_tools AS tool
		JOIN elitea_mcp.registered_servers AS server ON server.id = tool.server_id
		WHERE server.project_id = 1 AND server.name = 'mcp_confluence'
		ORDER BY tool.name`)
	if err != nil {
		t.Fatalf("read stored tools: %v", err)
	}
	defer rows.Close()
	var names []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatalf("scan stored tool: %v", err)
		}
		names = append(names, name)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate stored tools: %v", err)
	}
	if len(names) != 2 || names[0] != "kept_tool" || names[1] != "new_tool" {
		t.Fatalf("stored tools = %v, want the withdrawn tool gone", names)
	}

	// One server row, not two: a re-registration updates rather than duplicates.
	var servers int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM elitea_mcp.registered_servers WHERE project_id = 1`).Scan(&servers); err != nil {
		t.Fatalf("count servers: %v", err)
	}
	if servers != 1 {
		t.Fatalf("server rows = %d, want 1", servers)
	}
}

// A discovery that fails must not replace a working tool set with nothing. The
// caller learns the discovery failed; the listing keeps serving what it had.
func TestFailedDiscoveryKeepsTheStoredToolSet(t *testing.T) {
	pool := newMCPPool(t)
	ctx := context.Background()

	storeServer(t, pool, 1, "mcp_confluence", tool("kept_tool", "Stored earlier"))

	refusing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(refusing.Close)

	syncRouter := chi.NewRouter()
	syncRouter.Post("/api/v2/elitea_core/mcp_sync_tools/prompt_lib/{projectID}",
		newLoopbackSyncHandler(pool).MCPSyncTools)

	recorder := do(t, syncRouter, http.MethodPost,
		"/api/v2/elitea_core/mcp_sync_tools/prompt_lib/"+homeProject,
		fmt.Sprintf(`{"url":%q,"toolkit_type":"mcp_confluence"}`, refusing.URL))
	if decoded := decode(t, recorder); decoded["success"] != false {
		t.Fatalf("a refused discovery reported success = %v (%s)", decoded["success"], recorder.Body.String())
	}

	var stored int
	if err := pool.QueryRow(ctx, `
		SELECT count(*)
		FROM elitea_mcp.registered_tools AS tool
		JOIN elitea_mcp.registered_servers AS server ON server.id = tool.server_id
		WHERE server.project_id = 1 AND server.name = 'mcp_confluence'`).Scan(&stored); err != nil {
		t.Fatalf("count stored tools: %v", err)
	}
	if stored != 1 {
		t.Fatalf("stored tools = %d, want the earlier tool set kept", stored)
	}
}

/* ── guard rails ───────────────────────────────────────────────────────── */

// A project segment that is not a positive integer must never reach a query.
func TestToolsListRejectsANonNumericProject(t *testing.T) {
	pool := newMCPPool(t)
	router := newRouter(mcp.NewHandler(pool, apimw.NewDBPersonalProjectResolver(pool), nil, nil), callerUserID)

	for _, project := range []string{"0", "-1", "abc"} {
		recorder := do(t, router, http.MethodGet, toolsListTarget+project, "")
		if recorder.Code != http.StatusBadRequest {
			t.Fatalf("project %q: status = %d, want 400", project, recorder.Code)
		}
	}
}

// Belt and braces on the context plumbing: the listing must react to the user
// the request carries, which the personal-project case above depends on.
func TestToolsListReadsTheCallerFromTheRequestContext(t *testing.T) {
	pool := newMCPPool(t)
	personalProject := seedPersonalProject(t, pool, callerUserID)
	storeServer(t, pool, personalProject, "mcp_private", tool("private_tool", "Personal only"))

	withCaller := listServers(t,
		newRouter(mcp.NewHandler(pool, apimw.NewDBPersonalProjectResolver(pool), nil, nil), callerUserID),
		toolsListTarget+homeProject)
	if len(withCaller) != 1 || withCaller[0]["name"] != "mcp_private" {
		t.Fatalf("with the owning caller: %v, want the personal server", serverNames(withCaller))
	}

	anonymous := chi.NewRouter()
	anonymous.Get("/api/v2/elitea_core/tools_list/{projectID}",
		mcp.NewHandler(pool, apimw.NewDBPersonalProjectResolver(pool), nil, nil).ToolsList)
	withoutCaller := listServers(t, anonymous, toolsListTarget+homeProject)
	if len(withoutCaller) != 0 {
		t.Fatalf("with no caller in the context: %v, want an empty listing", serverNames(withoutCaller))
	}

	// The seeded project id must differ from the current project, or the case
	// above would pass for the wrong reason.
	if strconv.FormatInt(personalProject, 10) == homeProject {
		t.Fatalf("personal project collided with the current project")
	}
}

// newLoopbackSyncHandler is the eliteacore handler with a plain HTTP client.
// The fake MCP servers here listen on 127.0.0.1, which the production
// default client refuses (the MCP egress guard); WithHTTPClient is the
// documented override for tests.
func newLoopbackSyncHandler(pool *pgxpool.Pool) *eliteacore.Handler {
	return eliteacore.NewHandler(pool, eliteacore.WithHTTPClient(&http.Client{}))
}
