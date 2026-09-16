package toolkits

// These PostgreSQL tests verify attachment repository reads and legacy type discovery.
// Runtime toolkit discovery has separate service and HTTP contracts.
// Fixtures use private databases and retain read, scan, and empty-result checks.
// Set ELITEA_TEST_DATABASE_URL to run these tests.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/db"
)

// toolListFixture is one migrated tenant schema (p_1) holding two toolkits of
// type "github" attached to entity version 42, and nothing attached to entity
// version 43.
type toolListFixture struct {
	pool   *pgxpool.Pool
	router chi.Router
}

const (
	populatedVersionID = "42"
	emptyVersionID     = "43"
	// p_9999 is never created by the migrations, so every statement against it
	// fails with an undefined-table error.
	missingSchemaProjectID = "9999"
)

func newToolListFixture(t *testing.T) *toolListFixture {
	t.Helper()
	pool := newToolkitsIntegrationPool(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	t.Cleanup(cancel)
	if err := db.RunMigrations(ctx, pool); err != nil {
		t.Fatalf("run baseline migrations: %v", err)
	}

	for _, tool := range []struct{ name, description string }{
		{"github-primary", "the first attached tool"},
		{"github-secondary", ""},
	} {
		var toolID int64
		if err := pool.QueryRow(ctx, `
			INSERT INTO p_1.elitea_tools (name, type, description, owner_id, author_id)
			VALUES ($1, 'github', NULLIF($2, ''), 1, 7) RETURNING id`,
			tool.name, tool.description).Scan(&toolID); err != nil {
			t.Fatalf("insert %s: %v", tool.name, err)
		}
		if _, err := pool.Exec(ctx, `
			INSERT INTO p_1.entity_tool_mapping (entity_version_id, entity_type, tool_id)
			VALUES ($1, 'application', $2)`, populatedVersionID, toolID); err != nil {
			t.Fatalf("attach %s: %v", tool.name, err)
		}
	}

	// Legacy type discovery uses the real repository.
	router := chi.NewRouter()
	handler := NewHandler(pool)
	router.Post("/toolkit_discover_tools/prompt_lib/{projectID}/{toolkitType}", handler.DiscoverTools)
	return &toolListFixture{pool: pool, router: router}
}

func (f *toolListFixture) attachedTools(t *testing.T, projectID, versionID string) ([]Tool, error) {
	t.Helper()
	return (&pgRepo{pool: f.pool}).AvailableTools(context.Background(), projectID, versionID)
}

func (f *toolListFixture) discoverTools(t *testing.T, projectID, toolkitType string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	f.router.ServeHTTP(rec, httptest.NewRequest(http.MethodPost,
		"/toolkit_discover_tools/prompt_lib/"+projectID+"/"+toolkitType, nil))
	return rec
}

// decodeToolList reads a success body. It fails the test if the body is a
// failure body, so a test that expects tools cannot pass on an error.
func decodeToolList(t *testing.T, rec *httptest.ResponseRecorder) []map[string]any {
	t.Helper()
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d; body: %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Tools []map[string]any `json:"tools"`
		Total int              `json:"total"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.Total != len(resp.Tools) {
		t.Errorf("total is %d but the list holds %d tools", resp.Total, len(resp.Tools))
	}
	return resp.Tools
}

// assertRealReadFault states the failure contract against the real repository:
// a failure status, the named reason, and no tool list at all.
func assertRealReadFault(t *testing.T, rec *httptest.ResponseRecorder, wantReason string) {
	t.Helper()
	if rec.Code == http.StatusOK {
		t.Fatalf("a failed database read answered 200; body: %s", rec.Body.String())
	}
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("expected 500, got %d; body: %s", rec.Code, rec.Body.String())
	}
	var resp map[string]any
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if _, hasTools := resp["tools"]; hasTools {
		t.Errorf("a failed database read carried a tools list: %#v", resp)
	}
	if reason, _ := resp["error"].(string); reason != wantReason {
		t.Errorf("error = %q, want the named reason %q", reason, wantReason)
	}
}

// Direction one: a read that works still returns its tools. This test also
// covers the second swallowed error — the per-row `continue` — because a scan
// that fails for every row produced the same empty list as an unattached
// toolkit, and nothing here would have told the two apart.
func TestAttachedToolsReturnsTheAttachedToolsFromTheDatabase(t *testing.T) {
	fixture := newToolListFixture(t)

	tools, err := fixture.attachedTools(t, "1", populatedVersionID)
	if err != nil {
		t.Fatal(err)
	}
	if len(tools) != 2 {
		t.Fatalf("expected the 2 attached tools, got %d: %#v", len(tools), tools)
	}
	names := map[string]bool{}
	for _, tool := range tools {
		name := tool.Name
		names[name] = true
		if tool.ID == "" {
			t.Errorf("tool %q came back with no id: %#v", name, tool)
		}
		if toolType := tool.Type; toolType != "github" {
			t.Errorf("tool %q has type %q, want github", name, toolType)
		}
	}
	for _, want := range []string{"github-primary", "github-secondary"} {
		if !names[want] {
			t.Errorf("%q is missing from the response: %#v", want, tools)
		}
	}
}

// Direction two, half one: a toolkit that really has no tools keeps the empty
// success answer.
func TestAttachedToolsReturnsAnEmptyListForAToolkitWithNoTools(t *testing.T) {
	fixture := newToolListFixture(t)
	tools, err := fixture.attachedTools(t, "1", emptyVersionID)
	if err != nil || tools == nil || len(tools) != 0 {
		t.Fatalf("empty attachment rows: %#v %v", tools, err)
	}
}

// A missing tenant schema produces a repository error.
func TestAttachedToolsReportsAMissingTenantSchemaAsAFailure(t *testing.T) {
	fixture := newToolListFixture(t)

	tools, err := fixture.attachedTools(t, missingSchemaProjectID, populatedVersionID)
	if err == nil || tools != nil {
		t.Fatalf("read fault: %#v %v", tools, err)
	}
}

// The same fault from the other direction: the schema exists and the table is
// gone. This is the shape a partial migration leaves behind.
func TestAttachedToolsReportsADroppedTableAsAFailure(t *testing.T) {
	fixture := newToolListFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if _, err := fixture.pool.Exec(ctx, `DROP TABLE p_1.entity_tool_mapping`); err != nil {
		t.Fatalf("drop the mapping table: %v", err)
	}

	tools, err := fixture.attachedTools(t, "1", populatedVersionID)
	if err == nil || tools != nil {
		t.Fatalf("read fault: %#v %v", tools, err)
	}
}

func TestDiscoverToolsReturnsTheTypesToolsFromTheDatabase(t *testing.T) {
	fixture := newToolListFixture(t)

	tools := decodeToolList(t, fixture.discoverTools(t, "1", "github"))
	if len(tools) != 2 {
		t.Fatalf("expected the 2 github toolkits, got %d: %#v", len(tools), tools)
	}
}

func TestDiscoverToolsReturnsAnEmptyListForATypeWithNoTools(t *testing.T) {
	fixture := newToolListFixture(t)

	if tools := decodeToolList(t, fixture.discoverTools(t, "1", "jira")); len(tools) != 0 {
		t.Fatalf("expected no tools, got %d: %#v", len(tools), tools)
	}
}

func TestDiscoverToolsReportsAMissingTenantSchemaAsAFailure(t *testing.T) {
	fixture := newToolListFixture(t)

	assertRealReadFault(t, fixture.discoverTools(t, missingSchemaProjectID, "github"),
		"discover tools read failed")
}

// A row that fails to scan is a failure, not a shorter list (#381 AC4). The
// column type changes under the query, which is what a bad migration does. The
// old `continue` dropped every such row and answered 200 with the rows that
// were left — an empty list when all rows fail.
func TestAttachedToolsReportsARowThatFailsToScanAsAFailure(t *testing.T) {
	fixture := newToolListFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if _, err := fixture.pool.Exec(ctx,
		`ALTER TABLE p_1.elitea_tools ALTER COLUMN name TYPE bytea USING name::bytea`); err != nil {
		t.Fatalf("change the name column type: %v", err)
	}

	tools, err := fixture.attachedTools(t, "1", populatedVersionID)
	if err == nil || tools != nil {
		t.Fatalf("read fault: %#v %v", tools, err)
	}
}

func jsonHasEmptyToolsArray(body string) bool {
	var resp map[string]json.RawMessage
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		return false
	}
	raw, ok := resp["tools"]
	return ok && string(raw) == "[]"
}
