package mcp_test

// Acceptance for the MCP surface (issue 252) against a real PostgreSQL.
//
// Every case here asserts what the endpoint says about ROWS — which agents and
// toolkits appear, which do not, and what changes when the rows change. A
// status-code test would pass against a handler that returned a hardcoded list,
// or an empty one, forever; issue 128 is this repository's file of exactly that
// defect, and a tool listing is a good hiding place for it because "no tools"
// looks like a legitimate answer.
//
// So the discriminating assertions are:
//
//   - a tagged agent appears and an untagged one does not, in the same project;
//   - a flagged toolkit's tools appear and an unflagged one's do not, addressed
//     both project-wide and directly by id;
//   - the same rows in a DIFFERENT project's schema do not appear;
//   - the tool names are derived from the row values, so renaming the row
//     renames the tool;
//   - tools/call resolves against that same listing;
//   - the PAT states are the three the token rows actually imply.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	apimw "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/middleware"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/mcp"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth"
	platformmigrations "github.com/EliteaAI/elitea-platform/services/elitea-main/migrations"
)

const (
	homeProject  = "1"
	homeSchema   = "p_1"
	otherProject = "2"
	otherSchema  = "p_2"
	callerUserID = 7
)

/* ── harness ───────────────────────────────────────────────────────────── */

// newRouter mirrors internal/api/router.go's registration. The user middleware
// stands in for apimw.Auth: the routes under test read the authenticated
// principal from the context, and the middleware that puts it there has its own
// tests.
func newRouter(handler *mcp.Handler, userID int64) chi.Router {
	router := chi.NewRouter()
	router.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			user := auth.User{ID: fmt.Sprint(userID), UserID: fmt.Sprint(userID)}
			next.ServeHTTP(w, r.WithContext(auth.ContextWithUser(r.Context(), user)))
		})
	})
	router.Get("/app/{projectID}/mcp", handler.Endpoint)
	router.Post("/app/{projectID}/mcp", handler.Endpoint)
	router.Get("/app/{projectID}/mcp/*", handler.Endpoint)
	router.Post("/app/{projectID}/mcp/*", handler.Endpoint)
	router.Get("/api/v2/elitea_core/tools_list/{projectID}", handler.ToolsList)
	router.Get("/api/v2/elitea_core/tools_list/default/{projectID}", handler.ToolsList)
	router.Post("/api/v2/elitea_core/tools_call/default/{projectID}", handler.ToolsCall)
	router.Get("/api/v2/elitea_core/internal_mcp_pat_status/prompt_lib/{projectID}/{toolkitType}",
		handler.InternalMCPPATStatus)
	return router
}

func do(t *testing.T, router chi.Router, method, target, body string) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(method, target, bytes.NewReader([]byte(body)))
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	return recorder
}

func decode(t *testing.T, recorder *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var body map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode %q: %v", recorder.Body.String(), err)
	}
	return body
}

// listToolNames performs a real tools/list against the given endpoint and
// returns the tool names it served.
func listToolNames(t *testing.T, router chi.Router, target string) []string {
	t.Helper()
	recorder := do(t, router, http.MethodPost, target, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)
	if recorder.Code != http.StatusOK {
		t.Fatalf("%s: status = %d (%s)", target, recorder.Code, recorder.Body.String())
	}
	body := decode(t, recorder)
	if errorMember, present := body["error"]; present {
		t.Fatalf("%s: JSON-RPC error %v", target, errorMember)
	}
	result, ok := body["result"].(map[string]any)
	if !ok {
		t.Fatalf("%s: result missing in %v", target, body)
	}
	entries, ok := result["tools"].([]any)
	if !ok {
		t.Fatalf("%s: tools missing in %v", target, result)
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		tool, _ := entry.(map[string]any)
		name, _ := tool["name"].(string)
		names = append(names, name)
	}
	return names
}

func contains(names []string, want string) bool {
	for _, name := range names {
		if name == want {
			return true
		}
	}
	return false
}

type staticToolkitArgumentSchemas map[string]map[string]map[string]any

func (s staticToolkitArgumentSchemas) ToolkitArgumentSchemas(
	toolkitType string,
) (map[string]map[string]any, bool, error) {
	schemas, found := s[toolkitType]
	return schemas, found, nil
}

func listedTool(t *testing.T, router chi.Router, target, name string) map[string]any {
	t.Helper()
	recorder := do(t, router, http.MethodPost, target, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)
	if recorder.Code != http.StatusOK {
		t.Fatalf("%s: status = %d (%s)", target, recorder.Code, recorder.Body.String())
	}
	result, _ := decode(t, recorder)["result"].(map[string]any)
	entries, _ := result["tools"].([]any)
	for _, entry := range entries {
		tool, _ := entry.(map[string]any)
		if tool["name"] == name {
			return tool
		}
	}
	t.Fatalf("tool %q missing from %v", name, entries)
	return nil
}

/* ── seeds ─────────────────────────────────────────────────────────────── */

// seedAgent creates an application with one version, optionally tagged.
func seedAgent(t *testing.T, pool *pgxpool.Pool, schema, name, description string, tags ...string) int64 {
	t.Helper()
	ctx := context.Background()
	var applicationID int64
	if err := pool.QueryRow(ctx, fmt.Sprintf(`
		INSERT INTO %q.applications (name, description, owner_id, meta)
		VALUES ($1, $2, 1, '{}'::jsonb) RETURNING id`, schema), name, description).Scan(&applicationID); err != nil {
		t.Fatalf("seed application: %v", err)
	}
	var versionID int64
	if err := pool.QueryRow(ctx, fmt.Sprintf(`
		INSERT INTO %q.application_versions (application_id, name, status, author_id, agent_type)
		VALUES ($1, 'latest', 'draft', 7, 'react') RETURNING id`, schema), applicationID).Scan(&versionID); err != nil {
		t.Fatalf("seed application version: %v", err)
	}
	for _, tag := range tags {
		var tagID int64
		if err := pool.QueryRow(ctx, fmt.Sprintf(`
			INSERT INTO %q.tags (name) VALUES ($1)
			ON CONFLICT (name) DO UPDATE SET name = EXCLUDED.name RETURNING id`, schema), tag).Scan(&tagID); err != nil {
			t.Fatalf("seed tag: %v", err)
		}
		if _, err := pool.Exec(ctx, fmt.Sprintf(`
			INSERT INTO %q.application_version_tag_association (version_id, tag_id) VALUES ($1, $2)`, schema),
			versionID, tagID); err != nil {
			t.Fatalf("seed tag association: %v", err)
		}
	}
	return versionID
}

func seedToolkit(t *testing.T, pool *pgxpool.Pool, schema, name, toolkitType string, meta string, selectedTools ...string) int64 {
	t.Helper()
	selected, err := json.Marshal(selectedTools)
	if err != nil {
		t.Fatalf("marshal selected_tools: %v", err)
	}
	var toolkitID int64
	if err := pool.QueryRow(context.Background(), fmt.Sprintf(`
		INSERT INTO %q.elitea_tools (name, type, description, owner_id, author_id, meta, settings)
		VALUES ($1, $2, 'seeded toolkit', 1, 7, $3::jsonb, jsonb_build_object('selected_tools', $4::jsonb))
		RETURNING id`, schema), name, toolkitType, meta, string(selected)).Scan(&toolkitID); err != nil {
		t.Fatalf("seed toolkit: %v", err)
	}
	return toolkitID
}

// seedMCPToolkitWithURL creates a toolkit carrying a server URL, which is what
// the PAT-status endpoint inspects.
func seedMCPToolkitWithURL(t *testing.T, pool *pgxpool.Pool, schema, toolkitType, url string) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), fmt.Sprintf(`
		INSERT INTO %q.elitea_tools (name, type, description, owner_id, author_id, meta, settings)
		VALUES ($1, $1, 'seeded mcp toolkit', 1, 7, '{}'::jsonb, jsonb_build_object('url', $2::text))`, schema),
		toolkitType, url); err != nil {
		t.Fatalf("seed mcp toolkit: %v", err)
	}
}

func seedUser(t *testing.T, pool *pgxpool.Pool, userID int64) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), `
		INSERT INTO public.auth_core__user (id, email, name) VALUES ($1, $2, 'Seeded')
		ON CONFLICT (id) DO NOTHING`, userID, fmt.Sprintf("user%d@example.com", userID)); err != nil {
		t.Fatalf("seed user: %v", err)
	}
}

func seedToken(t *testing.T, pool *pgxpool.Pool, userID int64, expires *time.Time) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), `
		INSERT INTO public.auth_core__token (uuid, expires, user_id, name)
		VALUES (gen_random_uuid()::text, $1, $2, 'seeded')`, expires, userID); err != nil {
		t.Fatalf("seed token: %v", err)
	}
}

const availableByMCP = `{"mcp_options": {"available_by_mcp": true}}`

/* ── tools/list serves real rows ───────────────────────────────────────── */

// The core claim: the listing is this project's agents, and the `mcp` tag is
// what selects them. Two agents, one tag apart, must not both appear.
func TestToolsListServesOnlyAgentsTaggedMCP(t *testing.T) {
	pool := newMCPPool(t)
	router := newRouter(mcp.NewHandler(pool, apimw.NewDBPersonalProjectResolver(pool), nil, nil), callerUserID)

	seedAgent(t, pool, homeSchema, "Release Notes", "writes release notes", "mcp")
	seedAgent(t, pool, homeSchema, "Untagged Helper", "not exposed")

	names := listToolNames(t, router, "/app/"+homeProject+"/mcp")

	if !contains(names, "Release_Notes") {
		t.Fatalf("tagged agent missing from %v", names)
	}
	if contains(names, "Untagged_Helper") {
		t.Fatalf("untagged agent leaked into %v — the mcp tag is the opt-in", names)
	}
}

// Renaming the row must rename the tool. This is what separates "reads the
// database" from "returns a plausible constant".
func TestToolNamesFollowTheRowValues(t *testing.T) {
	pool := newMCPPool(t)
	router := newRouter(mcp.NewHandler(pool, apimw.NewDBPersonalProjectResolver(pool), nil, nil), callerUserID)
	seedAgent(t, pool, homeSchema, "First Name", "", "mcp")

	if names := listToolNames(t, router, "/app/"+homeProject+"/mcp"); !contains(names, "First_Name") {
		t.Fatalf("before rename: %v", names)
	}
	if _, err := pool.Exec(context.Background(),
		`UPDATE p_1.applications SET name = 'Second Name' WHERE name = 'First Name'`); err != nil {
		t.Fatalf("rename: %v", err)
	}
	names := listToolNames(t, router, "/app/"+homeProject+"/mcp")
	if contains(names, "First_Name") || !contains(names, "Second_Name") {
		t.Fatalf("after rename: %v", names)
	}
}

func TestToolsListServesOnlyToolkitsFlaggedAvailableByMCP(t *testing.T) {
	pool := newMCPPool(t)
	router := newRouter(mcp.NewHandler(pool, apimw.NewDBPersonalProjectResolver(pool), nil, nil), callerUserID)

	seedToolkit(t, pool, homeSchema, "Repo", "github", availableByMCP, "get_issue", "list_issues")
	seedToolkit(t, pool, homeSchema, "Private", "github", `{}`, "get_issue")

	names := listToolNames(t, router, "/app/"+homeProject+"/mcp")

	for _, want := range []string{"Repo_get_issue", "Repo_list_issues"} {
		if !contains(names, want) {
			t.Fatalf("%q missing from %v", want, names)
		}
	}
	if contains(names, "Private_get_issue") {
		t.Fatalf("unflagged toolkit leaked into %v", names)
	}
}

// The external MCP schema comes from the same digest-pinned SDK catalogue as
// the toolkit editor. Dynamic types remain explicit open objects because their
// operation schemas are discovered from a server or OpenAPI document at run
// time and therefore cannot appear in the built-in snapshot.
func TestToolkitToolsUsePinnedArgumentSchemasAndDynamicFallback(t *testing.T) {
	pool := newMCPPool(t)
	schemas := staticToolkitArgumentSchemas{
		"github": {
			"get_issue": {
				"type": "object",
				"properties": map[string]any{
					"issue_number": map[string]any{"type": "integer"},
				},
				"required": []string{"issue_number"},
			},
		},
		"openapi": {},
	}
	handler := mcp.NewHandler(
		pool,
		apimw.NewDBPersonalProjectResolver(pool),
		nil,
		nil,
		mcp.WithToolkitArgumentSchemas(schemas),
	)
	router := newRouter(handler, callerUserID)

	seedToolkit(t, pool, homeSchema, "Repo", "github", availableByMCP, "get_issue")
	seedToolkit(t, pool, homeSchema, "Dynamic", "openapi", availableByMCP, "search")

	pinned := listedTool(t, router, "/app/"+homeProject+"/mcp", "Repo_get_issue")
	pinnedSchema, _ := pinned["inputSchema"].(map[string]any)
	properties, _ := pinnedSchema["properties"].(map[string]any)
	issueNumber, _ := properties["issue_number"].(map[string]any)
	if issueNumber["type"] != "integer" {
		t.Fatalf("pinned input schema = %v, want integer issue_number", pinnedSchema)
	}
	required, _ := pinnedSchema["required"].([]any)
	if len(required) != 1 || required[0] != "issue_number" {
		t.Fatalf("pinned required = %v, want [issue_number]", required)
	}

	dynamic := listedTool(t, router, "/app/"+homeProject+"/mcp", "Dynamic_search")
	dynamicSchema, _ := dynamic["inputSchema"].(map[string]any)
	if dynamicSchema["additionalProperties"] != true {
		t.Fatalf("dynamic input schema = %v, want explicit open object", dynamicSchema)
	}
}

// Tenant isolation, asserted the only way that discriminates: identical rows in
// two schemas, and each endpoint serves exactly its own.
func TestListingIsScopedToTheProjectInTheURL(t *testing.T) {
	pool := newMCPPool(t)
	router := newRouter(mcp.NewHandler(pool, apimw.NewDBPersonalProjectResolver(pool), nil, nil), callerUserID)

	seedAgent(t, pool, homeSchema, "Home Agent", "", "mcp")
	seedAgent(t, pool, otherSchema, "Other Agent", "", "mcp")

	home := listToolNames(t, router, "/app/"+homeProject+"/mcp")
	other := listToolNames(t, router, "/app/"+otherProject+"/mcp")

	if !contains(home, "Home_Agent") || contains(home, "Other_Agent") {
		t.Fatalf("project %s served %v", homeProject, home)
	}
	if !contains(other, "Other_Agent") || contains(other, "Home_Agent") {
		t.Fatalf("project %s served %v", otherProject, other)
	}
}

/* ── category and resource scopes ──────────────────────────────────────── */

func TestCategoryScopesSplitTheTwoSources(t *testing.T) {
	pool := newMCPPool(t)
	router := newRouter(mcp.NewHandler(pool, apimw.NewDBPersonalProjectResolver(pool), nil, nil), callerUserID)

	seedAgent(t, pool, homeSchema, "Tagged Agent", "", "mcp")
	seedToolkit(t, pool, homeSchema, "Repo", "github", availableByMCP, "get_issue")

	applications := listToolNames(t, router, "/app/"+homeProject+"/mcp/applications")
	if !contains(applications, "Tagged_Agent") || contains(applications, "Repo_get_issue") {
		t.Fatalf("applications category served %v", applications)
	}

	toolkits := listToolNames(t, router, "/app/"+homeProject+"/mcp/toolkits")
	if !contains(toolkits, "Repo_get_issue") || contains(toolkits, "Tagged_Agent") {
		t.Fatalf("toolkits category served %v", toolkits)
	}

	everything := listToolNames(t, router, "/app/"+homeProject+"/mcp")
	if !contains(everything, "Tagged_Agent") || !contains(everything, "Repo_get_issue") {
		t.Fatalf("unscoped endpoint served %v, want both", everything)
	}
}

// Addressing a toolkit directly must not be a way past the
// available_by_mcp flag, and must not serve the project's other toolkits.
func TestResourceScopeServesOneToolkitAndStillHonoursTheFlag(t *testing.T) {
	pool := newMCPPool(t)
	router := newRouter(mcp.NewHandler(pool, apimw.NewDBPersonalProjectResolver(pool), nil, nil), callerUserID)

	exposed := seedToolkit(t, pool, homeSchema, "Repo", "github", availableByMCP, "get_issue")
	alsoExposed := seedToolkit(t, pool, homeSchema, "Wiki", "confluence", availableByMCP, "read_page")
	hidden := seedToolkit(t, pool, homeSchema, "Private", "github", `{}`, "get_issue")

	only := listToolNames(t, router, fmt.Sprintf("/app/%s/mcp/toolkit/%d", homeProject, exposed))
	if len(only) != 1 || only[0] != "Repo_get_issue" {
		t.Fatalf("toolkit scope served %v, want only Repo_get_issue", only)
	}
	if contains(only, "Wiki_read_page") {
		t.Fatalf("toolkit scope leaked a sibling toolkit: %v", only)
	}
	_ = alsoExposed

	refused := listToolNames(t, router, fmt.Sprintf("/app/%s/mcp/toolkit/%d", homeProject, hidden))
	if len(refused) != 0 {
		t.Fatalf("unflagged toolkit addressed directly served %v, want nothing", refused)
	}
}

// An application version id belongs to a project. Reaching one from another
// project's endpoint must find nothing — the tenant boundary is the schema, and
// the id alone must not be enough.
func TestResourceScopeCannotReachAnotherProjectsVersion(t *testing.T) {
	pool := newMCPPool(t)
	router := newRouter(mcp.NewHandler(pool, apimw.NewDBPersonalProjectResolver(pool), nil, nil), callerUserID)

	versionID := seedAgent(t, pool, otherSchema, "Foreign Agent", "")

	own := listToolNames(t, router, fmt.Sprintf("/app/%s/mcp/agent/%d", otherProject, versionID))
	if len(own) != 1 || own[0] != "Foreign_Agent" {
		t.Fatalf("own project served %v, want Foreign_Agent", own)
	}

	across := listToolNames(t, router, fmt.Sprintf("/app/%s/mcp/agent/%d", homeProject, versionID))
	if len(across) != 0 {
		t.Fatalf("cross-project version scope served %v, want nothing", across)
	}
}

/* ── tools/call ────────────────────────────────────────────────────────── */

// The call path resolves against the same rows the listing served: a real
// agent's tool is recognised and answered with the stated reason, and a name
// that is not in the listing is a caller error.
func TestToolsCallResolvesAgainstTheRealListing(t *testing.T) {
	pool := newMCPPool(t)
	router := newRouter(mcp.NewHandler(pool, apimw.NewDBPersonalProjectResolver(pool), nil, nil), callerUserID)
	seedAgent(t, pool, homeSchema, "Release Notes", "", "mcp")

	listed := decode(t, do(t, router, http.MethodPost, "/app/"+homeProject+"/mcp",
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"Release_Notes","arguments":{"task":"go"}}}`))
	result, ok := listed["result"].(map[string]any)
	if !ok {
		t.Fatalf("expected a CallToolResult, got %v", listed)
	}
	if result["isError"] != true {
		t.Fatalf("isError = %v, want true — execution is not wired", result["isError"])
	}
	content, _ := result["content"].([]any)
	if len(content) == 0 {
		t.Fatalf("content = %v, want the stated reason", result["content"])
	}
	block, _ := content[0].(map[string]any)
	if block["text"] != mcp.ToolExecutionUnavailableReason {
		t.Fatalf("content text = %v, want the stated reason", block["text"])
	}

	unlisted := decode(t, do(t, router, http.MethodPost, "/app/"+homeProject+"/mcp",
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"Untagged_Helper"}}`))
	if _, present := unlisted["error"]; !present {
		t.Fatalf("a tool that is not listed was accepted: %v", unlisted)
	}
}

/* ── the master switch ─────────────────────────────────────────────────── */

// The flag's own description promises it removes MCP functionality "including
// API endpoints". A version that only hid the menu would leave this whole
// surface open to anyone who kept a URL.
func TestMCPMasterSwitchClosesEveryEndpoint(t *testing.T) {
	pool := newMCPPool(t)
	router := newRouter(mcp.NewHandler(pool, apimw.NewDBPersonalProjectResolver(pool), nil, nil), callerUserID)
	seedAgent(t, pool, homeSchema, "Tagged Agent", "", "mcp")

	// Enabled by default: the listing works.
	if names := listToolNames(t, router, "/app/"+homeProject+"/mcp"); !contains(names, "Tagged_Agent") {
		t.Fatalf("precondition: listing served %v", names)
	}

	if _, err := pool.Exec(context.Background(), `
		INSERT INTO centry.platform_config (section, key, value)
		VALUES ('mcp_configuration', 'mcp_enabled', 'false'::jsonb)
		ON CONFLICT (section, key) DO UPDATE SET value = EXCLUDED.value`); err != nil {
		t.Fatalf("switch MCP off: %v", err)
	}

	for _, endpoint := range []struct{ method, target, body string }{
		{http.MethodPost, "/app/" + homeProject + "/mcp", `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`},
		{http.MethodPost, "/app/" + homeProject + "/mcp/applications", `{"jsonrpc":"2.0","id":1,"method":"initialize"}`},
		{http.MethodGet, "/app/" + homeProject + "/mcp", ""},
		{http.MethodGet, "/api/v2/elitea_core/tools_list/default/" + homeProject, ""},
		{http.MethodPost, "/api/v2/elitea_core/tools_call/default/" + homeProject, `{}`},
		{http.MethodGet, "/api/v2/elitea_core/internal_mcp_pat_status/prompt_lib/" + homeProject + "/mcp_elitea", ""},
	} {
		recorder := do(t, router, endpoint.method, endpoint.target, endpoint.body)
		if recorder.Code != http.StatusForbidden {
			t.Fatalf("%s %s: status = %d, want 403 (%s)",
				endpoint.method, endpoint.target, recorder.Code, recorder.Body.String())
		}
	}
}

/* ── the one remaining REST refusal ────────────────────────────────────── */

// tools_call still refuses, and the reason is pinned so that a later edit
// cannot quietly turn it into a stub that answers 200 with nothing behind it.
//
// tools_list is NOT in this table any more. It serves the durable store — see
// the tools_list cases in this package.
func TestToolsCallRefusesWithTheStatedReason(t *testing.T) {
	pool := newMCPPool(t)
	router := newRouter(mcp.NewHandler(pool, apimw.NewDBPersonalProjectResolver(pool), nil, nil), callerUserID)

	recorder := do(t, router, http.MethodPost, "/api/v2/elitea_core/tools_call/default/"+homeProject,
		`{"server":"local","tool_call_id":"1","tool_timeout_sec":30,"params":{}}`)
	if recorder.Code != http.StatusNotImplemented {
		t.Fatalf("tools_call: status = %d, want 501", recorder.Code)
	}
	if got := decode(t, recorder)["error"]; got != mcp.ToolRegistryUnavailableReason {
		t.Fatalf("tools_call: error = %v, want the stated reason", got)
	}
}

/* ── PAT status ────────────────────────────────────────────────────────── */

// BOTH forms of the internal endpoint URL count — the unresolved
// `{project_id}` marker and a URL already resolved to a concrete project id.
// The resolved form is the one this stack can actually produce: the marker is
// written only by pylon's prebuilt-config machinery, which was not ported, so a
// template-only match would make the whole `internal: true` branch unreachable
// here and every caller would be told VALID whatever their token state.
//
// A non-`mcp_` type is not internal whatever its URL says, and an endpoint URL
// that is not this platform's is not internal either.
func TestPATStatusRecognisesTheInternalEndpointInBothURLForms(t *testing.T) {
	pool := newMCPPool(t)
	seedUser(t, pool, callerUserID)
	router := newRouter(mcp.NewHandler(pool, apimw.NewDBPersonalProjectResolver(pool), nil, nil), callerUserID)

	seedMCPToolkitWithURL(t, pool, homeSchema, "mcp_elitea_internal",
		"http://pylon_main:8080/app/{project_id}/mcp/elitea_core/applications")
	seedMCPToolkitWithURL(t, pool, homeSchema, "mcp_elitea_resolved",
		"http://pylon_main:8080/app/1/mcp/elitea_core/applications")
	seedMCPToolkitWithURL(t, pool, homeSchema, "mcp_external", "https://mcp.example.com/sse")
	seedMCPToolkitWithURL(t, pool, homeSchema, "github", "http://pylon_main:8080/app/{project_id}/mcp/toolkits")

	for toolkitType, wantInternal := range map[string]bool{
		"mcp_elitea_internal": true,
		"mcp_elitea_resolved": true,
		"mcp_external":        false,
		"github":              false,
		"mcp_never_created":   false,
	} {
		recorder := do(t, router, http.MethodGet,
			"/api/v2/elitea_core/internal_mcp_pat_status/prompt_lib/"+homeProject+"/"+toolkitType, "")
		if recorder.Code != http.StatusOK {
			t.Fatalf("%s: status = %d (%s)", toolkitType, recorder.Code, recorder.Body.String())
		}
		body := decode(t, recorder)
		if body["internal"] != wantInternal {
			t.Fatalf("%s: internal = %v, want %v", toolkitType, body["internal"], wantInternal)
		}
	}
}

// The three states are the ones the token rows imply, and they must move as the
// rows move — a handler that answered a constant would pass a single-state test.
//
// Seeded with the RESOLVED URL on purpose: that is the form a toolkit created
// in this stack carries, so this exercises the state machine on the shape
// production can actually produce rather than only on the pylon-written
// template.
func TestPATStatusReportsMissingThenExpiredThenValid(t *testing.T) {
	pool := newMCPPool(t)
	seedUser(t, pool, callerUserID)
	router := newRouter(mcp.NewHandler(pool, apimw.NewDBPersonalProjectResolver(pool), nil, nil), callerUserID)
	seedMCPToolkitWithURL(t, pool, homeSchema, "mcp_elitea_internal",
		"http://elitea-main:8080/app/"+homeProject+"/mcp/elitea_core/applications")

	target := "/api/v2/elitea_core/internal_mcp_pat_status/prompt_lib/" + homeProject + "/mcp_elitea_internal"

	state := func() string {
		recorder := do(t, router, http.MethodGet, target, "")
		if recorder.Code != http.StatusOK {
			t.Fatalf("status = %d (%s)", recorder.Code, recorder.Body.String())
		}
		body := decode(t, recorder)
		if body["internal"] != true {
			t.Fatalf("internal = %v, want true", body["internal"])
		}
		value, _ := body["state"].(string)
		return value
	}

	if got := state(); got != "MISSING" {
		t.Fatalf("with no tokens: state = %q, want MISSING", got)
	}

	past := time.Now().UTC().Add(-24 * time.Hour)
	seedToken(t, pool, callerUserID, &past)
	if got := state(); got != "EXPIRED" {
		t.Fatalf("with one expired token: state = %q, want EXPIRED", got)
	}

	future := time.Now().UTC().Add(24 * time.Hour)
	seedToken(t, pool, callerUserID, &future)
	if got := state(); got != "VALID" {
		t.Fatalf("with one live token: state = %q, want VALID", got)
	}
}

// A token with no expiry never expires — pylon's rule, and the one every
// non-expiring PAT in a real deployment relies on.
func TestPATStatusTreatsANullExpiryAsValid(t *testing.T) {
	pool := newMCPPool(t)
	seedUser(t, pool, callerUserID)
	router := newRouter(mcp.NewHandler(pool, apimw.NewDBPersonalProjectResolver(pool), nil, nil), callerUserID)
	seedMCPToolkitWithURL(t, pool, homeSchema, "mcp_elitea_internal",
		"http://pylon_main:8080/app/{project_id}/mcp/elitea_core/applications")
	seedToken(t, pool, callerUserID, nil)

	body := decode(t, do(t, router, http.MethodGet,
		"/api/v2/elitea_core/internal_mcp_pat_status/prompt_lib/"+homeProject+"/mcp_elitea_internal", ""))
	if body["state"] != "VALID" {
		t.Fatalf("state = %v, want VALID", body["state"])
	}
}

// The report is about the CALLER's own tokens. Another user's live PAT must not
// make this user's toolkit look usable.
func TestPATStatusReadsOnlyTheCallersTokens(t *testing.T) {
	pool := newMCPPool(t)
	seedUser(t, pool, callerUserID)
	seedUser(t, pool, callerUserID+1)
	seedMCPToolkitWithURL(t, pool, homeSchema, "mcp_elitea_internal",
		"http://pylon_main:8080/app/{project_id}/mcp/elitea_core/applications")

	future := time.Now().UTC().Add(24 * time.Hour)
	seedToken(t, pool, callerUserID+1, &future)

	target := "/api/v2/elitea_core/internal_mcp_pat_status/prompt_lib/" + homeProject + "/mcp_elitea_internal"

	withoutToken := decode(t, do(t, newRouter(mcp.NewHandler(pool, apimw.NewDBPersonalProjectResolver(pool), nil, nil), callerUserID), http.MethodGet, target, ""))
	if withoutToken["state"] != "MISSING" {
		t.Fatalf("caller without a token: state = %v, want MISSING", withoutToken["state"])
	}
	withToken := decode(t, do(t, newRouter(mcp.NewHandler(pool, apimw.NewDBPersonalProjectResolver(pool), nil, nil), callerUserID+1), http.MethodGet, target, ""))
	if withToken["state"] != "VALID" {
		t.Fatalf("caller with a token: state = %v, want VALID", withToken["state"])
	}
}

/* ── database bootstrap ────────────────────────────────────────────────── */

func newMCPPool(t *testing.T) *pgxpool.Pool {
	t.Helper()

	const environment = "ELITEA_TEST_DATABASE_URL"
	databaseURL := os.Getenv(environment)
	if databaseURL == "" {
		t.Skipf("set %s to run the PostgreSQL service-integration test", environment)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	adminConfig, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		t.Fatalf("parse %s: %v", environment, err)
	}
	adminConfig.MaxConns = 2
	adminPool, err := pgxpool.NewWithConfig(ctx, adminConfig)
	if err != nil {
		t.Fatalf("open PostgreSQL admin pool: %v", err)
	}
	if err := adminPool.Ping(ctx); err != nil {
		adminPool.Close()
		t.Fatalf("ping PostgreSQL: %v", err)
	}

	databaseName := fmt.Sprintf("elitea_mcp_it_%d_%d", os.Getpid(), time.Now().UnixNano())
	quotedDatabase := pgx.Identifier{databaseName}.Sanitize()
	if _, err := adminPool.Exec(ctx, "CREATE DATABASE "+quotedDatabase); err != nil {
		adminPool.Close()
		t.Fatalf("create isolated PostgreSQL integration database: %v", err)
	}

	testConfig := adminConfig.Copy()
	testConfig.ConnConfig.Database = databaseName
	testConfig.MaxConns = 4
	pool, err := pgxpool.NewWithConfig(ctx, testConfig)
	if err != nil {
		if _, dropErr := adminPool.Exec(context.Background(), "DROP DATABASE "+quotedDatabase+" WITH (FORCE)"); dropErr != nil {
			t.Errorf("drop database after pool open failure: %v", dropErr)
		}
		adminPool.Close()
		t.Fatalf("open isolated PostgreSQL integration database: %v", err)
	}
	t.Cleanup(func() {
		pool.Close()
		// 120 s, not the old 20 s to 30 s. This DROP queues behind the
		// CREATE DATABASE calls of every package that `go test ./...` runs at
		// the same time, so the wait is server load and not a hang. Two full
		// runs failed here with "drop isolated ... database: timeout" (#409).
		dropCtx, dropCancel := context.WithTimeout(context.Background(), 120*time.Second)
		defer dropCancel()
		if _, err := adminPool.Exec(dropCtx, "DROP DATABASE "+quotedDatabase+" WITH (FORCE)"); err != nil {
			t.Errorf("drop isolated PostgreSQL integration database: %v", err)
		}
		adminPool.Close()
	})

	initial, err := os.ReadFile(filepath.Join("..", "..", "..", "infra", "db", "migrations", "001_initial.sql"))
	if err != nil {
		t.Fatalf("read 001_initial.sql: %v", err)
	}
	if _, err := pool.Exec(ctx, string(initial)); err != nil {
		t.Fatalf("apply 001_initial.sql: %v", err)
	}
	if _, err := pool.Exec(ctx, `SELECT create_tenant_schema($1)`, otherSchema); err != nil {
		t.Fatalf("create second project schema: %v", err)
	}

	// The durable MCP server store lives in the shared history, not in
	// 001_initial.sql. The migration file itself is applied — not a copy of its
	// DDL — so a change to the shipped schema that these tests do not expect
	// makes them fail here rather than passing against a stale duplicate.
	registry, err := platformmigrations.Files.ReadFile(mcpRegistryMigration)
	if err != nil {
		t.Fatalf("read %s: %v", mcpRegistryMigration, err)
	}
	if _, err := pool.Exec(ctx, string(registry)); err != nil {
		t.Fatalf("apply %s: %v", mcpRegistryMigration, err)
	}
	return pool
}

// mcpRegistryMigration creates elitea_mcp.registered_servers and
// elitea_mcp.registered_tools (issue 335).
const mcpRegistryMigration = "shared/0073_mcp_tool_registry.sql"
