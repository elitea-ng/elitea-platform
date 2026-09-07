package toolkits_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/toolkits"
)

// mockRepo implements toolkits.Repository for testing.
type mockRepo struct {
	types []string
	tools []toolkits.Tool
	valid bool
	tool  toolkits.Tool
	err   error
}

func (m *mockRepo) ListTypes(_ context.Context, _ string) ([]string, error) {
	if m.err != nil {
		return nil, m.err
	}
	return m.types, nil
}

func (m *mockRepo) AvailableTools(_ context.Context, _, _ string) ([]toolkits.Tool, error) {
	if m.err != nil {
		return nil, m.err
	}
	return m.tools, nil
}

func (m *mockRepo) DiscoverTools(_ context.Context, _, _ string) ([]toolkits.Tool, error) {
	if m.err != nil {
		return nil, m.err
	}
	return m.tools, nil
}

func (m *mockRepo) ValidateToolkit(_ context.Context, _, _ string) (bool, error) {
	if m.err != nil {
		return false, m.err
	}
	return m.valid, nil
}

func (m *mockRepo) ForkToolkit(_ context.Context, _ string, _ map[string]any) (toolkits.Tool, error) {
	if m.err != nil {
		return toolkits.Tool{}, m.err
	}
	return m.tool, nil
}

func (m *mockRepo) ListToolkits(_ context.Context, _ string, _, _ int) ([]map[string]any, int, error) {
	if m.err != nil {
		return nil, 0, m.err
	}
	return nil, 0, nil
}

func (m *mockRepo) CreateToolkit(_ context.Context, _ string, body map[string]any) (map[string]any, error) {
	if m.err != nil {
		return nil, m.err
	}
	return body, nil
}

func (m *mockRepo) GetToolkit(_ context.Context, _, _ string) (map[string]any, error) {
	if m.err != nil {
		return nil, m.err
	}
	return map[string]any{}, nil
}

func (m *mockRepo) UpdateToolkit(_ context.Context, _, _ string, body map[string]any) (map[string]any, error) {
	if m.err != nil {
		return nil, m.err
	}
	return body, nil
}

func (m *mockRepo) DeleteToolkit(_ context.Context, _, _ string) error {
	return m.err
}

// setupRouter wires up a chi router with the given mock repo attached at the
// URL patterns matching router.go.
func setupRouter(repo toolkits.Repository) *chi.Mux {
	r := chi.NewRouter()
	h := toolkits.NewHandlerWithRepo(repo)
	r.Get("/toolkit_types/prompt_lib/{projectID}", h.ListTypes)
	r.Get("/toolkit_available_tools/prompt_lib/{projectID}/{toolkitID}", h.AvailableTools)
	r.Post("/toolkit_discover_tools/prompt_lib/{projectID}/{toolkitType}", h.DiscoverTools)
	r.Post("/toolkit_validator/prompt_lib/{projectID}/{toolkitID}", h.ValidateToolkit)
	r.Post("/fork_toolkit/prompt_lib/{projectID}", h.ForkToolkit)
	r.Post("/test_tool/prompt_lib/{projectID}/{toolID}", h.TestTool)
	r.Post("/test_toolkit_tool/prompt_lib/{projectID}", h.TestToolkitTool)
	r.Get("/index_types/prompt_lib/{projectID}", h.IndexTypes)
	return r
}

// --- ListTypes ---

func TestListTypes_Success(t *testing.T) {
	// The two new database types join the 11 built-in types. Custom is deduplicated.
	repo := &mockRepo{types: []string{"openai", "langchain", "custom"}}
	r := setupRouter(repo)

	req := httptest.NewRequest(http.MethodGet, "/toolkit_types/prompt_lib/proj-1", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}

	var resp map[string]any
	_ = json.NewDecoder(rec.Body).Decode(&resp)

	// Handler emits key "rows", not "toolkit_types".
	types, ok := resp["rows"].([]any)
	if !ok {
		t.Fatalf("expected rows array, got %T", resp["rows"])
	}
	if len(types) != 13 {
		t.Errorf("expected 13 types, got %d", len(types))
	}
	total := resp["total"].(float64)
	if int(total) != 13 {
		t.Errorf("expected total 13, got %v", total)
	}
}

func TestListTypes_DBError(t *testing.T) {
	// This route degrades on purpose. The static knownToolkitTypes list is a
	// correct answer on its own and the create-toolkit form needs it, so a
	// failed tenant read still gives 200 and the 11 static types. #381 changed
	// only the record: the repository now returns the error and the handler
	// logs the degradation instead of dropping the error with `_`. The two
	// tool-LIST routes below are different — an empty list is a real answer
	// there, so a lost read must not borrow it.
	repo := &mockRepo{err: errors.New("db error")}
	r := setupRouter(repo)

	req := httptest.NewRequest(http.MethodGet, "/toolkit_types/prompt_lib/proj-1", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}

	var resp map[string]any
	_ = json.NewDecoder(rec.Body).Decode(&resp)

	// Handler emits key "rows", not "toolkit_types".
	types, ok := resp["rows"].([]any)
	if !ok {
		t.Fatalf("expected rows array, got %T", resp["rows"])
	}
	// A failed database read retains the built-in Remote MCP type.
	if len(types) != 11 {
		t.Errorf("expected 11 types on DB error (static list), got %d", len(types))
	}
	foundMCP := false
	for _, toolkitType := range types {
		foundMCP = foundMCP || toolkitType == "mcp"
	}
	if !foundMCP {
		t.Error("remote MCP is missing from the built-in type list")
	}
	total := resp["total"].(float64)
	if int(total) != 11 {
		t.Errorf("expected total 11 on DB error, got %v", total)
	}
}

// --- AvailableTools ---

func TestAvailableTools_Success(t *testing.T) {
	repo := &mockRepo{
		tools: []toolkits.Tool{
			{ID: "tool-1", Name: "Tool One", Type: "openai"},
			{ID: "tool-2", Name: "Tool Two", Type: "langchain"},
		},
	}
	r := setupRouter(repo)

	req := httptest.NewRequest(http.MethodGet, "/toolkit_available_tools/prompt_lib/proj-1/toolkit-abc", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}

	var resp map[string]any
	_ = json.NewDecoder(rec.Body).Decode(&resp)

	tools, ok := resp["tools"].([]any)
	if !ok {
		t.Fatalf("expected tools array, got %T", resp["tools"])
	}
	if len(tools) != 2 {
		t.Errorf("expected 2 tools, got %d", len(tools))
	}
	total := resp["total"].(float64)
	if int(total) != 2 {
		t.Errorf("expected total 2, got %v", total)
	}
}

func TestAvailableTools_Empty(t *testing.T) {
	repo := &mockRepo{tools: []toolkits.Tool{}}
	r := setupRouter(repo)

	req := httptest.NewRequest(http.MethodGet, "/toolkit_available_tools/prompt_lib/proj-1/toolkit-none", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}

	var resp map[string]any
	_ = json.NewDecoder(rec.Body).Decode(&resp)

	tools, ok := resp["tools"].([]any)
	if !ok {
		t.Fatalf("expected tools array, got %T", resp["tools"])
	}
	if len(tools) != 0 {
		t.Errorf("expected 0 tools, got %d", len(tools))
	}
}

// --- DiscoverTools ---

func TestDiscoverTools_Success(t *testing.T) {
	repo := &mockRepo{
		tools: []toolkits.Tool{
			{ID: "tool-3", Name: "Discovered Tool", Type: "custom"},
		},
	}
	r := setupRouter(repo)

	req := httptest.NewRequest(http.MethodPost, "/toolkit_discover_tools/prompt_lib/proj-1/custom", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}

	var resp map[string]any
	_ = json.NewDecoder(rec.Body).Decode(&resp)

	tools, ok := resp["tools"].([]any)
	if !ok {
		t.Fatalf("expected tools array, got %T", resp["tools"])
	}
	if len(tools) != 1 {
		t.Errorf("expected 1 tool, got %d", len(tools))
	}
	total := resp["total"].(float64)
	if int(total) != 1 {
		t.Errorf("expected total 1, got %v", total)
	}
}

func TestDiscoverTools_Empty(t *testing.T) {
	repo := &mockRepo{tools: []toolkits.Tool{}}
	r := setupRouter(repo)

	req := httptest.NewRequest(http.MethodPost, "/toolkit_discover_tools/prompt_lib/proj-1/nonexistent", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}

	var resp map[string]any
	_ = json.NewDecoder(rec.Body).Decode(&resp)

	tools, ok := resp["tools"].([]any)
	if !ok {
		t.Fatalf("expected tools array, got %T", resp["tools"])
	}
	if len(tools) != 0 {
		t.Errorf("expected 0 tools, got %d", len(tools))
	}
}

// --- #381: a read fault and an empty result must not share one response ---

// assertReadFaultResponse states what a lost read must look like: a failure
// status, a named reason, and NO tool list. The absent list is the load-bearing
// half. A failure status that still carried `"tools": []` would let a caller
// that only reads the body go on showing an empty tool picker.
func assertReadFaultResponse(t *testing.T, rec *httptest.ResponseRecorder, wantReason string) {
	t.Helper()

	if rec.Code == http.StatusOK {
		t.Fatalf("a failed read answered 200; body: %s", rec.Body.String())
	}
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("expected 500, got %d", rec.Code)
	}

	var resp map[string]any
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if _, hasTools := resp["tools"]; hasTools {
		t.Errorf("a failed read carried a tools list: %#v", resp)
	}
	if _, hasTotal := resp["total"]; hasTotal {
		t.Errorf("a failed read carried a total: %#v", resp)
	}
	reason, _ := resp["error"].(string)
	if reason != wantReason {
		t.Errorf("error = %q, want the named reason %q", reason, wantReason)
	}
}

func TestAvailableTools_ReadFaultIsNotAnEmptyList(t *testing.T) {
	repo := &mockRepo{err: errors.New("connection refused")}
	r := setupRouter(repo)

	req := httptest.NewRequest(http.MethodGet, "/toolkit_available_tools/prompt_lib/proj-1/42", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	assertReadFaultResponse(t, rec, "available tools read failed")
}

func TestDiscoverTools_ReadFaultIsNotAnEmptyList(t *testing.T) {
	repo := &mockRepo{err: errors.New("connection refused")}
	r := setupRouter(repo)

	req := httptest.NewRequest(http.MethodPost, "/toolkit_discover_tools/prompt_lib/proj-1/openapi", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	assertReadFaultResponse(t, rec, "discover tools read failed")
}

// The pair above and the two _Empty tests above it only discriminate together.
// This test states the pair directly, so that a later edit which collapses the
// two outcomes back into one body fails here and not only in a distant file.
func TestToolListEmptyAndFailedReadDoNotShareAResponse(t *testing.T) {
	empty := httptest.NewRecorder()
	setupRouter(&mockRepo{tools: []toolkits.Tool{}}).ServeHTTP(
		empty, httptest.NewRequest(http.MethodGet, "/toolkit_available_tools/prompt_lib/proj-1/42", nil))

	failed := httptest.NewRecorder()
	setupRouter(&mockRepo{err: errors.New("connection refused")}).ServeHTTP(
		failed, httptest.NewRequest(http.MethodGet, "/toolkit_available_tools/prompt_lib/proj-1/42", nil))

	if empty.Code != http.StatusOK {
		t.Errorf("a toolkit with no tools answered %d, want 200", empty.Code)
	}
	if empty.Code == failed.Code && empty.Body.String() == failed.Body.String() {
		t.Fatalf("an empty toolkit and a lost read give the same answer: %d %s", empty.Code, empty.Body.String())
	}
}

// --- ValidateToolkit ---

func TestValidateToolkit_Valid(t *testing.T) {
	repo := &mockRepo{valid: true}
	r := setupRouter(repo)

	req := httptest.NewRequest(http.MethodPost, "/toolkit_validator/prompt_lib/proj-1/toolkit-exists", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}

	var resp map[string]any
	_ = json.NewDecoder(rec.Body).Decode(&resp)

	valid, ok := resp["valid"].(bool)
	if !ok {
		t.Fatalf("expected valid bool, got %T", resp["valid"])
	}
	if !valid {
		t.Error("expected valid=true")
	}
}

func TestValidateToolkit_Invalid(t *testing.T) {
	// Handler returns 400 (not 200) when valid=false.
	repo := &mockRepo{valid: false}
	r := setupRouter(repo)

	req := httptest.NewRequest(http.MethodPost, "/toolkit_validator/prompt_lib/proj-1/toolkit-missing", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", rec.Code)
	}

	var resp map[string]any
	_ = json.NewDecoder(rec.Body).Decode(&resp)

	valid, ok := resp["valid"].(bool)
	if !ok {
		t.Fatalf("expected valid bool, got %T", resp["valid"])
	}
	if valid {
		t.Error("expected valid=false")
	}
}

func TestValidateToolkit_DBError(t *testing.T) {
	// Handler returns 400 with valid=false and settings_errors on repo error.
	repo := &mockRepo{err: errors.New("connection reset")}
	r := setupRouter(repo)

	req := httptest.NewRequest(http.MethodPost, "/toolkit_validator/prompt_lib/proj-1/toolkit-any", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", rec.Code)
	}

	var resp map[string]any
	_ = json.NewDecoder(rec.Body).Decode(&resp)

	valid, ok := resp["valid"].(bool)
	if !ok {
		t.Fatalf("expected valid bool, got %T", resp["valid"])
	}
	if valid {
		t.Error("expected valid=false on error")
	}
	// On error, handler also includes settings_errors array.
	if _, hasErrors := resp["settings_errors"]; !hasErrors {
		t.Error("expected settings_errors field in error response")
	}
}

// --- ForkToolkit ---

func TestForkToolkit_Success(t *testing.T) {
	forked := toolkits.Tool{ID: "forked-1", Name: "My Tool (copy)", Type: "openai"}
	repo := &mockRepo{tool: forked}
	r := setupRouter(repo)

	body, _ := json.Marshal(map[string]string{"source_id": "original-1"})
	req := httptest.NewRequest(http.MethodPost, "/fork_toolkit/prompt_lib/proj-1", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d; body: %s", rec.Code, rec.Body.String())
	}

	var tool toolkits.Tool
	_ = json.NewDecoder(rec.Body).Decode(&tool)
	if tool.ID != "forked-1" {
		t.Errorf("expected ID forked-1, got %q", tool.ID)
	}
	if tool.Name != "My Tool (copy)" {
		t.Errorf("expected name 'My Tool (copy)', got %q", tool.Name)
	}
}

func TestForkToolkit_Error(t *testing.T) {
	repo := &mockRepo{err: errors.New("source not found")}
	r := setupRouter(repo)

	body, _ := json.Marshal(map[string]string{"source_id": "bad-id"})
	req := httptest.NewRequest(http.MethodPost, "/fork_toolkit/prompt_lib/proj-1", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500, got %d", rec.Code)
	}

	var resp map[string]any
	_ = json.NewDecoder(rec.Body).Decode(&resp)

	ok, exists := resp["ok"].(bool)
	if !exists || ok {
		t.Error("expected ok=false in error response")
	}
	if _, hasErr := resp["error"]; !hasErr {
		t.Error("expected error field in error response")
	}
}

// --- TestTool ---

func TestTestTool_NoTester_Returns503(t *testing.T) {
	repo := &mockRepo{}
	r := setupRouter(repo)

	req := httptest.NewRequest(http.MethodPost, "/test_tool/prompt_lib/proj-1/tool-99", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d", rec.Code)
	}
}

// --- TestToolkitTool ---

func TestTestToolkitTool_NoTester_Returns503(t *testing.T) {
	repo := &mockRepo{}
	r := setupRouter(repo)

	req := httptest.NewRequest(http.MethodPost, "/test_toolkit_tool/prompt_lib/proj-1", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d", rec.Code)
	}
}

// --- IndexTypes ---

// TestIndexTypes_RefusesInsteadOfServingAnInventedCatalogue replaces
// TestIndexTypes_StaticResponse, which asserted the six hand-written entries
// verbatim — including their invented `supported_extensions` lists. That test
// could only ever confirm that the literal in the handler still matched the
// literal in the test; it had no way to notice that neither matched anything
// the deployment can actually index.
//
// This one fails on a 200: a deployment that has not enabled the real
// index-types route must say so rather than answer with a guess.
func TestIndexTypes_RefusesInsteadOfServingAnInventedCatalogue(t *testing.T) {
	repo := &mockRepo{}
	r := setupRouter(repo)

	req := httptest.NewRequest(http.MethodGet, "/index_types/prompt_lib/proj-1", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code == http.StatusOK {
		t.Fatalf("status = 200 — the six hardcoded loaders were served as though a "+
			"catalogue had been read; body=%s", rec.Body.String())
	}
	if rec.Code != http.StatusNotImplemented {
		t.Fatalf("status = %d, want 501; body=%s", rec.Code, rec.Body.String())
	}

	var resp map[string]any
	_ = json.NewDecoder(rec.Body).Decode(&resp)
	if code, _ := resp["code"].(string); code != "index_types_not_available" {
		t.Fatalf("code = %q, want a machine-readable index_types_not_available", code)
	}
	if _, present := resp["items"]; present {
		t.Fatal("the refusal must not carry an items list — a client that reads it " +
			"would be back where it started")
	}
}
