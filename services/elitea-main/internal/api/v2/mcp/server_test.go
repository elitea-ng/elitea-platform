package mcp

// Protocol-level tests for the MCP server (issue 252 P2/P3), with the tool
// catalog faked so the assertions are about the WIRE, not about SQL. The
// database half — that the catalog reads real rows, and that the listing
// changes when the rows change — is asserted separately in
// mcp_postgres_integration_test.go, because a fake source here could never
// catch a query that returns nothing.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/db/tenantschema"
)

/* ── harness ───────────────────────────────────────────────────────────── */

// newTestHandler builds a handler whose catalog is the supplied source.
//
// The nil pool is deliberate: platformconfig.Load reads a nil pool as "nothing
// configured", so the MCP master switch resolves to its permissive default and
// these tests exercise the protocol rather than the flag. The flag's own
// behaviour is asserted in the integration suite, against a real
// centry.platform_config row.
func newTestHandler(t *testing.T, source toolSource) *Handler {
	t.Helper()
	handler := NewHandler(nil, nil, nil, nil)
	handler.source = source
	return handler
}

// funcSource adapts a closure to toolSource, recording the scope it was asked
// for. That record is how the scope tests prove the URL reached the catalog as
// the right query rather than merely producing a 200.
type funcSource struct {
	fn    func(schema string, s scope) ([]Tool, error)
	last  scope
	calls int
}

func (f *funcSource) tools(_ context.Context, schema string, s scope) ([]Tool, error) {
	f.last = s
	f.calls++
	return f.fn(schema, s)
}

// router mirrors the production registration in
// internal/api/router.go:mountMCPServerRoutes, minus the auth and
// project-access middleware, which are the router's concern and are covered by
// their own tests. The two path shapes matter here: chi will not match the bare
// endpoint against the wildcard pattern, and a test that only exercised the
// wildcard would not notice if the bare one were dropped.
func newTestRouter(handler *Handler) chi.Router {
	router := chi.NewRouter()
	router.Get("/app/{projectID}/mcp", handler.Endpoint)
	router.Post("/app/{projectID}/mcp", handler.Endpoint)
	router.Get("/app/{projectID}/mcp/*", handler.Endpoint)
	router.Post("/app/{projectID}/mcp/*", handler.Endpoint)
	return router
}

func post(t *testing.T, router chi.Router, target, body string) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(http.MethodPost, target, strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	return recorder
}

func decodeRPC(t *testing.T, recorder *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var decoded map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &decoded); err != nil {
		t.Fatalf("decode %q: %v", recorder.Body.String(), err)
	}
	return decoded
}

func resultOf(t *testing.T, recorder *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	body := decodeRPC(t, recorder)
	if errorMember, present := body["error"]; present {
		t.Fatalf("expected a result, got error %v", errorMember)
	}
	result, ok := body["result"].(map[string]any)
	if !ok {
		t.Fatalf("result missing or not an object in %v", body)
	}
	return result
}

func errorOf(t *testing.T, recorder *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	body := decodeRPC(t, recorder)
	errorMember, ok := body["error"].(map[string]any)
	if !ok {
		t.Fatalf("expected an error object, got %v", body)
	}
	return errorMember
}

func staticSource(tools ...Tool) *funcSource {
	return &funcSource{fn: func(string, scope) ([]Tool, error) { return tools, nil }}
}

/* ── handshake ─────────────────────────────────────────────────────────── */

func TestInitializeEchoesASupportedProtocolVersion(t *testing.T) {
	router := newTestRouter(newTestHandler(t, staticSource()))

	for _, version := range supportedProtocolVersions {
		recorder := post(t, router, "/app/7/mcp",
			`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"`+version+`"}}`)
		if recorder.Code != http.StatusOK {
			t.Fatalf("initialize status = %d, want 200 (%s)", recorder.Code, recorder.Body.String())
		}
		if got := resultOf(t, recorder)["protocolVersion"]; got != version {
			t.Fatalf("protocolVersion = %v, want the requested %q", got, version)
		}
	}
}

// A client asking for a revision this server does not speak must be told the
// newest one it does, not handed its own string back — the client's whole
// decision to stay connected rests on that field being true.
func TestInitializeNamesTheNewestVersionForAnUnknownRequest(t *testing.T) {
	router := newTestRouter(newTestHandler(t, staticSource()))
	recorder := post(t, router, "/app/7/mcp",
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"1999-01-01"}}`)
	if got := resultOf(t, recorder)["protocolVersion"]; got != supportedProtocolVersions[0] {
		t.Fatalf("protocolVersion = %v, want %q", got, supportedProtocolVersions[0])
	}
}

// Capabilities are a promise the client acts on. Declaring `resources` or
// `prompts` would make a client send resources/list and prompts/list, both of
// which this server answers "method not found"; declaring
// `tools.listChanged: true` would make it wait for a notification that can
// never arrive on a stateless endpoint with no open stream. pylon declares all
// of them.
func TestInitializeDeclaresOnlyImplementedCapabilities(t *testing.T) {
	router := newTestRouter(newTestHandler(t, staticSource()))
	recorder := post(t, router, "/app/7/mcp", `{"jsonrpc":"2.0","id":1,"method":"initialize"}`)

	capabilities, ok := resultOf(t, recorder)["capabilities"].(map[string]any)
	if !ok {
		t.Fatalf("capabilities missing")
	}
	if len(capabilities) != 1 {
		t.Fatalf("capabilities = %v, want tools only", capabilities)
	}
	tools, ok := capabilities["tools"].(map[string]any)
	if !ok {
		t.Fatalf("tools capability missing from %v", capabilities)
	}
	if tools["listChanged"] != false {
		t.Fatalf("tools.listChanged = %v, want false — no notification is ever sent", tools["listChanged"])
	}
}

func TestInitializeNamesTheScope(t *testing.T) {
	router := newTestRouter(newTestHandler(t, staticSource()))
	for target, want := range map[string]string{
		"/app/7/mcp":              "ELITEA MCP SERVER",
		"/app/7/mcp/applications": "ELITEA-APPLICATIONS",
		"/app/7/mcp/toolkit/42":   "ELITEA-TOOLKIT-42",
		"/app/7/mcp/agent/42":     "ELITEA-APPLICATION-42",
	} {
		recorder := post(t, router, target, `{"jsonrpc":"2.0","id":1,"method":"initialize"}`)
		serverInfo, ok := resultOf(t, recorder)["serverInfo"].(map[string]any)
		if !ok {
			t.Fatalf("%s: serverInfo missing", target)
		}
		if serverInfo["name"] != want {
			t.Fatalf("%s: serverInfo.name = %v, want %q", target, serverInfo["name"], want)
		}
	}
}

/* ── transport rules ───────────────────────────────────────────────────── */

// A notification has no id, so there is no response to correlate. 202 with an
// empty body is what the specification requires and is the one place this port
// deliberately departs from pylon, which answers 200 `{}`.
func TestNotificationIsAcceptedWithNoBody(t *testing.T) {
	router := newTestRouter(newTestHandler(t, staticSource()))
	recorder := post(t, router, "/app/7/mcp", `{"jsonrpc":"2.0","method":"notifications/initialized"}`)

	if recorder.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", recorder.Code)
	}
	if body := strings.TrimSpace(recorder.Body.String()); body != "" {
		t.Fatalf("body = %q, want empty", body)
	}
}

func TestGetRefusesWithTheSSESentence(t *testing.T) {
	router := newTestRouter(newTestHandler(t, staticSource()))
	request := httptest.NewRequest(http.MethodGet, "/app/7/mcp", nil)
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", recorder.Code)
	}
	if got := decodeRPC(t, recorder)["error"]; got != sseUnavailableMessage {
		t.Fatalf("error = %v, want %q", got, sseUnavailableMessage)
	}
	if allow := recorder.Header().Get("Allow"); allow != http.MethodPost {
		t.Fatalf("Allow = %q, want POST", allow)
	}
}

func TestMalformedAndBatchBodiesAreDistinguished(t *testing.T) {
	router := newTestRouter(newTestHandler(t, staticSource()))

	parseError := errorOf(t, post(t, router, "/app/7/mcp", `{"jsonrpc":`))
	if parseError["code"] != float64(codeParseError) {
		t.Fatalf("malformed JSON code = %v, want %d", parseError["code"], codeParseError)
	}

	batchError := errorOf(t, post(t, router, "/app/7/mcp", `[{"jsonrpc":"2.0","id":1,"method":"ping"}]`))
	if batchError["code"] != float64(codeInvalidRequest) {
		t.Fatalf("batch code = %v, want %d", batchError["code"], codeInvalidRequest)
	}
	if message, _ := batchError["message"].(string); !strings.Contains(message, "batch") {
		t.Fatalf("batch message = %q, want it to name batching", message)
	}
}

// The response id must be the client's bytes, unchanged: a client keyed on the
// string "abc" cannot match a response carrying the number 0, and correlating
// the wrong response to the wrong request is silent corruption rather than an
// error.
func TestResponseEchoesTheRequestIDVerbatim(t *testing.T) {
	router := newTestRouter(newTestHandler(t, staticSource()))
	for _, id := range []string{`"abc"`, `17`, `0`} {
		recorder := post(t, router, "/app/7/mcp", `{"jsonrpc":"2.0","id":`+id+`,"method":"ping"}`)
		var raw struct {
			ID json.RawMessage `json:"id"`
		}
		if err := json.Unmarshal(recorder.Body.Bytes(), &raw); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if string(raw.ID) != id {
			t.Fatalf("id = %s, want %s", raw.ID, id)
		}
	}
}

func TestUnsupportedMethodIsMethodNotFound(t *testing.T) {
	router := newTestRouter(newTestHandler(t, staticSource()))
	for _, method := range []string{"resources/list", "prompts/list", "logging/setLevel", "completion/complete"} {
		body := errorOf(t, post(t, router, "/app/7/mcp", `{"jsonrpc":"2.0","id":1,"method":"`+method+`"}`))
		if body["code"] != float64(codeMethodNotFound) {
			t.Fatalf("%s: code = %v, want %d", method, body["code"], codeMethodNotFound)
		}
	}
}

func TestInvalidProjectSegmentIsRefusedBeforeAnyQuery(t *testing.T) {
	source := staticSource()
	router := newTestRouter(newTestHandler(t, source))
	recorder := post(t, router, "/app/notanumber/mcp", `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)

	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", recorder.Code)
	}
	if source.calls != 0 {
		t.Fatalf("catalog was queried %d times for an invalid project id", source.calls)
	}
}

/* ── scopes ────────────────────────────────────────────────────────────── */

// Each URL shape must reach the catalog as a different query. Asserting only
// the status code would pass even if every scope collapsed to "list
// everything", which is the failure that would silently widen a
// resource-scoped endpoint into a project-wide one.
func TestEachURLShapeReachesTheCatalogAsItsOwnScope(t *testing.T) {
	cases := []struct {
		target string
		want   scope
	}{
		{"/app/7/mcp", scope{kind: scopeAll}},
		{"/app/7/mcp/applications", scope{kind: scopeCategory, category: "applications"}},
		{"/app/7/mcp/elitea_core/applications", scope{kind: scopeCategory, category: "elitea_core/applications"}},
		{"/app/7/mcp/toolkits", scope{kind: scopeCategory, category: "toolkits"}},
		{"/app/7/mcp/toolkit/42", scope{kind: scopeResource, resourceType: "toolkit", resourceID: 42}},
		{"/app/7/mcp/agent/42", scope{kind: scopeResource, resourceType: "application", resourceID: 42}},
		{"/app/7/mcp/pipeline/9", scope{kind: scopeResource, resourceType: "application", resourceID: 9}},
		{"/app/7/mcp/application/9", scope{kind: scopeResource, resourceType: "application", resourceID: 9}},
	}
	for _, testCase := range cases {
		source := staticSource()
		router := newTestRouter(newTestHandler(t, source))
		recorder := post(t, router, testCase.target, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)
		if recorder.Code != http.StatusOK {
			t.Fatalf("%s: status = %d (%s)", testCase.target, recorder.Code, recorder.Body.String())
		}
		if source.last != testCase.want {
			t.Fatalf("%s: catalog scope = %+v, want %+v", testCase.target, source.last, testCase.want)
		}
	}
}

// The project schema the catalog is handed must come from the URL. A hardcoded
// or defaulted schema is the shape of a cross-tenant read.
func TestCatalogReceivesTheURLProjectSchema(t *testing.T) {
	var seen string
	source := &funcSource{fn: func(schema string, _ scope) ([]Tool, error) {
		seen = schema
		return nil, nil
	}}
	router := newTestRouter(newTestHandler(t, source))
	post(t, router, "/app/312/mcp", `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)
	// The schema is handed over QUOTED as a PostgreSQL identifier. The
	// expected value is built the same way the handler builds it, so the test
	// cannot drift from the quoting.
	want, err := tenantschema.Quote("312")
	if err != nil {
		t.Fatalf("Quote(312) failed: %v", err)
	}
	if seen != want {
		t.Fatalf("catalog schema = %q, want %q", seen, want)
	}
}

// pylon's OpenAPI-section categories publish REST operations as tools, which
// this server does not serve. Answering them with the project's agents instead
// would hand a legacy client a plausible listing containing none of the tools
// it asked for.
func TestPylonAPICategoriesAreRefusedRatherThanReinterpreted(t *testing.T) {
	source := staticSource(Tool{Name: "some_agent"})
	router := newTestRouter(newTestHandler(t, source))

	for _, tag := range []string{"elitea_core/chat", "secrets", "configurations", "api"} {
		recorder := post(t, router, "/app/7/mcp/"+tag, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)
		if recorder.Code != http.StatusBadRequest {
			t.Fatalf("%s: status = %d, want 400 (%s)", tag, recorder.Code, recorder.Body.String())
		}
		if source.calls != 0 {
			t.Fatalf("%s: catalog was queried for a refused category", tag)
		}
	}
}

func TestUnknownEntityTypeNamesTheValidTypes(t *testing.T) {
	router := newTestRouter(newTestHandler(t, staticSource()))
	recorder := post(t, router, "/app/7/mcp/widget/42", `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", recorder.Code)
	}
	message, _ := decodeRPC(t, recorder)["error"].(string)
	for _, entity := range []string{"agent", "application", "pipeline", "toolkit"} {
		if !strings.Contains(message, entity) {
			t.Fatalf("error %q does not name the valid type %q", message, entity)
		}
	}
}

/* ── tools/list ────────────────────────────────────────────────────────── */

func TestToolsListServesTheCatalogSortedAndOmitsNextCursor(t *testing.T) {
	source := staticSource(
		Tool{Name: "zeta", Description: "z", InputSchema: agentTaskSchema()},
		Tool{Name: "alpha", Description: "a", InputSchema: agentTaskSchema()},
	)
	router := newTestRouter(newTestHandler(t, source))
	result := resultOf(t, post(t, router, "/app/7/mcp", `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))

	if _, present := result["nextCursor"]; present {
		t.Fatalf("nextCursor present; a null cursor makes clients paginate into a second request")
	}
	tools, ok := result["tools"].([]any)
	if !ok || len(tools) != 2 {
		t.Fatalf("tools = %v, want two entries", result["tools"])
	}
	first, _ := tools[0].(map[string]any)
	if first["name"] != "alpha" {
		t.Fatalf("first tool = %v, want alpha (listing must be ordered)", first["name"])
	}
	if _, present := first["inputSchema"]; !present {
		t.Fatalf("inputSchema missing; clients reject a tool without one")
	}
}

// An empty project is an empty ARRAY, never a null: `"tools": null` is not a
// valid ListToolsResult and several clients treat it as a protocol error.
func TestEmptyCatalogSerialisesAsAnEmptyArray(t *testing.T) {
	router := newTestRouter(newTestHandler(t, staticSource()))
	recorder := post(t, router, "/app/7/mcp", `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)
	if !strings.Contains(recorder.Body.String(), `"tools":[]`) {
		t.Fatalf("body = %s, want an empty tools array", recorder.Body.String())
	}
}

/* ── tools/call ────────────────────────────────────────────────────────── */

// The honest-error contract: a call for a tool that IS listed reports that
// execution is unavailable and says why, as an isError result rather than an
// empty success. An empty success is what an agent host reads as "the tool ran
// and produced nothing", which is the failure issue 252 asks this not to be.
func TestCallOfAListedToolReportsExecutionUnavailable(t *testing.T) {
	router := newTestRouter(newTestHandler(t, staticSource(Tool{Name: "my_agent"})))
	result := resultOf(t, post(t, router, "/app/7/mcp",
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"my_agent","arguments":{"task":"hi"}}}`))

	if result["isError"] != true {
		t.Fatalf("isError = %v, want true", result["isError"])
	}
	content, ok := result["content"].([]any)
	if !ok || len(content) == 0 {
		t.Fatalf("content = %v, want one text block", result["content"])
	}
	block, _ := content[0].(map[string]any)
	if block["type"] != "text" || block["text"] != ToolExecutionUnavailableReason {
		t.Fatalf("content block = %v, want the stated reason", block)
	}
}

// A name that is not in the listing is a caller error, not a tool failure —
// and it must be resolved against the SAME scope tools/list served, or a
// resource-scoped call could name a tool the caller was never shown.
func TestCallOfAnUnlistedToolIsInvalidParams(t *testing.T) {
	source := &funcSource{fn: func(_ string, s scope) ([]Tool, error) {
		if s.kind == scopeResource {
			return []Tool{{Name: "scoped_tool"}}, nil
		}
		return []Tool{{Name: "scoped_tool"}, {Name: "other_tool"}}, nil
	}}
	router := newTestRouter(newTestHandler(t, source))

	body := errorOf(t, post(t, router, "/app/7/mcp/toolkit/42",
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"other_tool"}}`))
	if body["code"] != float64(codeInvalidParams) {
		t.Fatalf("code = %v, want %d", body["code"], codeInvalidParams)
	}
	if message, _ := body["message"].(string); !strings.Contains(message, "other_tool") {
		t.Fatalf("message = %q, want it to name the tool", message)
	}
}

func TestCallWithoutANameIsInvalidParams(t *testing.T) {
	router := newTestRouter(newTestHandler(t, staticSource(Tool{Name: "my_agent"})))
	for _, params := range []string{`{}`, `{"name":""}`, `{"name":"   "}`} {
		body := errorOf(t, post(t, router, "/app/7/mcp",
			`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":`+params+`}`))
		if body["code"] != float64(codeInvalidParams) {
			t.Fatalf("params %s: code = %v, want %d", params, body["code"], codeInvalidParams)
		}
	}
}
