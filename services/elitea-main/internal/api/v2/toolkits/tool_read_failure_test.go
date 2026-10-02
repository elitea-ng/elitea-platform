package toolkits

// A failed tool read must report a failure (#340).
//
// `AvailableTools` and `DiscoverTools` answered `200 {"tools":[],"total":0}`
// for a failed read. An empty list is a legitimate answer — a toolkit can hold
// no tools — so the caller could not tell an outage from an empty toolkit.
//
// The swallow had TWO layers. The handler discarded the error, and `pgRepo`
// never produced one: it returned `[]Tool{}, nil` when the query failed, and it
// never checked `rows.Err()`. The handler's `if err != nil` was therefore dead
// against the production repository, so a handler-only fix would change
// nothing. The tests below cover both layers on purpose.
//
// The repository tests need a PostgreSQL service (ELITEA_TEST_DATABASE_URL);
// `newToolkitsIntegrationPool` creates a throwaway database per test.

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/db"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/db/migrate"
	platformmigrations "github.com/EliteaAI/elitea-platform/services/elitea-main/migrations"
)

// missingSchemaProjectIDLegacy names a project whose tenant schema `p_4242` was
// never created. Every read against it fails inside PostgreSQL with
// undefined_table. That is the exact production failure this pair of tests is
// about: the database refuses the read, and the caller must learn that it did.
//
// THIS FILE DUPLICATES COVERAGE THAT available_tools_read_fault_test.go AND
// handler_test.go ALREADY GIVE. It arrived with #410. #439 then rewrote the
// same routes and brought its own tests, which declare their own
// `missingSchemaProjectID` ("9999"). Both files cannot declare that name in
// one package, so this one carries the `Legacy` suffix.
//
// Every condition below is covered again, and more strictly, by #439:
//   - a missing tenant schema, by TestAvailableToolsReportsAMissingTenantSchemaAsAFailure
//     and TestDiscoverToolsReportsAMissingTenantSchemaAsAFailure
//   - a failed read that must not look like an empty list, by
//     TestAvailableToolsDoesNotReadFailedAttachmentRepository and its Discover twin, whose
//     assertReadFaultResponse requires the error to EQUAL a named reason. That
//     is stronger than the "not the raw cause" assertion below, because it
//     excludes the leak and pins the reason.
//   - an empty toolkit that must stay 200, by TestAvailableToolsRejectsEmptyAttachmentFallback and
//     TestToolListEmptyAndFailedReadDoNotShareAResponse
//
// #439 also covers a dropped table, a row that fails to scan, and a ListTypes
// read fault, none of which this file reaches. The file was kept rather than
// deleted so that the removal is a separate, deliberate change with the
// coverage argument written down. Delete it when someone confirms the mapping
// above; do not delete it merely to resolve a name collision.
const missingSchemaProjectIDLegacy = "4242"

// newToolReadFixture builds p_1 with one toolkit attached to one version, so a
// GOOD read returns exactly one row. Without that half the failure assertions
// below would also pass against a repository that returns an error for
// everything.
func newToolReadFixture(t *testing.T) (*pgRepo, string) {
	t.Helper()
	pool := newToolkitsIntegrationPool(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	t.Cleanup(cancel)

	if err := db.RunMigrations(ctx, pool); err != nil {
		t.Fatalf("run baseline migrations: %v", err)
	}
	runner := migrate.New(pool, platformmigrations.Files)
	if err := runner.ApplyShared(ctx); err != nil {
		t.Fatalf("apply shared migrations: %v", err)
	}
	if err := runner.ApplyTenant(ctx, 1); err != nil {
		t.Fatalf("apply tenant migrations to p_1: %v", err)
	}

	var applicationID, versionID, toolkitID int64
	if err := pool.QueryRow(ctx, `
		INSERT INTO p_1.applications (name, description, owner_id)
		VALUES ('tool-read-fixture', '', 1) RETURNING id`).Scan(&applicationID); err != nil {
		t.Fatalf("insert fixture application: %v", err)
	}
	if err := pool.QueryRow(ctx, `
		INSERT INTO p_1.application_versions (application_id, name, status, author_id)
		VALUES ($1, 'base', 'draft', 7) RETURNING id`, applicationID).Scan(&versionID); err != nil {
		t.Fatalf("insert fixture version: %v", err)
	}
	if err := pool.QueryRow(ctx, `
		INSERT INTO p_1.elitea_tools (name, type, description, settings, meta, owner_id, author_id)
		VALUES ('tool-read-fixture', 'github', 'fixture toolkit', '{}'::jsonb, '{}'::jsonb, 1, 7)
		RETURNING id`).Scan(&toolkitID); err != nil {
		t.Fatalf("insert fixture toolkit: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO p_1.entity_tool_mapping (entity_version_id, entity_id, entity_type, tool_id)
		VALUES ($1, $2, 'agent', $3)`, versionID, applicationID, toolkitID); err != nil {
		t.Fatalf("insert fixture mapping: %v", err)
	}

	return &pgRepo{pool: pool}, strconv.FormatInt(versionID, 10)
}

// ── Repository layer: the half that makes the handler's check reachable ──────

func TestPgRepoAvailableToolsReturnsAnErrorWhenTheReadFails(t *testing.T) {
	repo, versionID := newToolReadFixture(t)
	ctx := context.Background()

	// A good read first: the query itself works and finds the attached toolkit.
	tools, err := repo.AvailableTools(ctx, "1", versionID)
	if err != nil {
		t.Fatalf("read the available tools of the fixture version: %v", err)
	}
	if len(tools) != 1 {
		t.Fatalf("expected the fixture toolkit, got %d tools: %+v", len(tools), tools)
	}
	if tools[0].Name != "tool-read-fixture" {
		t.Fatalf("expected the fixture toolkit name, got %q", tools[0].Name)
	}

	// The same read against a schema that does not exist must FAIL.
	failed, err := repo.AvailableTools(ctx, missingSchemaProjectIDLegacy, versionID)
	if err == nil {
		t.Fatalf("a read of the missing schema p_%s reported success and returned %d tools; "+
			"a failed read must return an error, not an empty list", missingSchemaProjectIDLegacy, len(failed))
	}
	if failed != nil {
		t.Errorf("a failed read must return no tools, got %+v", failed)
	}
}

func TestPgRepoDiscoverToolsReturnsAnErrorWhenTheReadFails(t *testing.T) {
	repo, _ := newToolReadFixture(t)
	ctx := context.Background()

	tools, err := repo.DiscoverTools(ctx, "1", "github")
	if err != nil {
		t.Fatalf("discover the tools of type github: %v", err)
	}
	if len(tools) != 1 {
		t.Fatalf("expected the fixture toolkit, got %d tools: %+v", len(tools), tools)
	}

	failed, err := repo.DiscoverTools(ctx, missingSchemaProjectIDLegacy, "github")
	if err == nil {
		t.Fatalf("a discover against the missing schema p_%s reported success and returned %d tools; "+
			"a failed read must return an error, not an empty list", missingSchemaProjectIDLegacy, len(failed))
	}
	if failed != nil {
		t.Errorf("a failed discover must return no tools, got %+v", failed)
	}
}

// ── Handler layer ────────────────────────────────────────────────────────────

// toolReadRepo answers the two reads under test and nothing else. The embedded
// interface keeps it a Repository without restating the methods these routes
// never call; they stay nil and panic if a route ever reaches them.
type toolReadRepo struct {
	Repository
	tools          []Tool
	availableCalls int
	err            error
}

func (f *toolReadRepo) AvailableTools(context.Context, string, string) ([]Tool, error) {
	f.availableCalls++
	return f.tools, f.err
}

func (f *toolReadRepo) DiscoverTools(context.Context, string, string) ([]Tool, error) {
	return f.tools, f.err
}

func newToolReadRouter(repo Repository) chi.Router {
	h := NewHandlerWithRepo(repo)
	r := chi.NewRouter()
	r.Get("/toolkit_available_tools/prompt_lib/{projectID}/{toolkitID}", h.AvailableTools)
	r.Post("/toolkit_discover_tools/prompt_lib/{projectID}/{toolkitType}", h.DiscoverTools)
	return r
}

var errToolRead = errors.New("relation \"p_1.elitea_tools\" does not exist")

func TestAvailableToolsUnavailableDoesNotReadFailedRepository(t *testing.T) {
	repo := &toolReadRepo{err: errToolRead}
	rec := httptest.NewRecorder()
	newToolReadRouter(repo).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/toolkit_available_tools/prompt_lib/1/7", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body["error"] != "toolkit discovery unavailable" || len(body) != 1 || repo.availableCalls != 0 {
		t.Fatalf("response=%v attachment reads=%d", body, repo.availableCalls)
	}
}

func TestDiscoverToolsAnswersAFailureWhenTheReadFails(t *testing.T) {
	router := newToolReadRouter(&toolReadRepo{err: errToolRead})

	request := httptest.NewRequest(http.MethodPost, "/toolkit_discover_tools/prompt_lib/1/github", nil)
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500 for a failed discover, got %d: %s", recorder.Code, recorder.Body.String())
	}

	var body map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode the response: %v", err)
	}
	if _, present := body["tools"]; present {
		t.Errorf("a failed discover must not answer with a tools list, got %s", recorder.Body.String())
	}
	if message, _ := body["error"].(string); message == "" {
		t.Errorf("a failed discover must name the failure, got %s", recorder.Body.String())
	}
}

// An empty attachment repository cannot substitute for runtime discovery.
func TestAvailableToolsUnavailableDoesNotReportEmptyAttachments(t *testing.T) {
	repo := &toolReadRepo{tools: []Tool{}}
	rec := httptest.NewRecorder()
	newToolReadRouter(repo).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/toolkit_available_tools/prompt_lib/1/7", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body["error"] != "toolkit discovery unavailable" || len(body) != 1 || repo.availableCalls != 0 {
		t.Fatalf("response=%v attachment reads=%d", body, repo.availableCalls)
	}
}
