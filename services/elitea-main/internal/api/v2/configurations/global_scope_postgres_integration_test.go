package configurations_test

// The platform surfaces' ROW SCOPING, against a real PostgreSQL.
//
// ## Why this file exists
//
// `requireGlobalRowSection` is the fix for a hole that let each platform
// surface write the whole public project: the delegated Update and Delete
// address a row by id alone — `DELETE FROM p_N.configuration WHERE id = $1`,
// with no section predicate — so `DELETE /providers/{id}` given a MODEL's id
// deleted that model, `DELETE /platform_models/{id}` deleted a credential, and
// a PUT carrying only `data` overwrote a project_context row.
//
// Its DECISION is unit-tested. Its QUERY was not, and this package had no
// PostgreSQL harness to test it with — so a malformed statement would have made
// every Update and Delete answer 404 forever, which is fail-closed, silent, and
// indistinguishable from "the row is not yours". That is the shape this
// codebase keeps meeting: absence read as correctness.
//
// So every refusal here asserts the ROW SURVIVED, not merely that the status
// changed. A 404 with the row deleted anyway would pass a status-code test and
// is the exact outcome the guard exists to prevent.
//
// ## The table shape comes from the projection, not from this file
//
// `internal/db/schema/configuration_baseline.sql` is the canonical projection of
// the tenant `configuration` table. It is read and applied into the test's
// `p_N` schema rather than restated here, so this harness tracks the column set
// instead of becoming a third copy that drifts from it.

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/testpg"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/configurations"
)

// globalScopeProject is the public project this harness publishes into.
const globalScopeProject = 1

/* ── harness ───────────────────────────────────────────────────────────── */

// newGlobalScopePool builds an isolated database carrying one tenant schema.
//
// Isolated per test run, like the admin package's own harness: `go test ./...`
// runs packages concurrently, and a shared database would make one package's
// DROP another package's flake.
func newGlobalScopePool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	const environment = "ELITEA_TEST_DATABASE_URL"
	databaseURL := testpg.DSN(os.Getenv(environment))
	if databaseURL == "" {
		t.Skipf("set %s to run the PostgreSQL service-integration test", environment)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
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

	databaseName := fmt.Sprintf("elitea_globalscope_it_%d_%d", os.Getpid(), time.Now().UnixNano())
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
		if _, dropErr := adminPool.Exec(context.Background(),
			"DROP DATABASE "+quotedDatabase+" WITH (FORCE)"); dropErr != nil {
			t.Errorf("drop database after pool open failure: %v", dropErr)
		}
		adminPool.Close()
		t.Fatalf("open isolated PostgreSQL integration database: %v", err)
	}
	t.Cleanup(func() {
		pool.Close()
		// 120 s for the reason the admin harness gives: this DROP queues behind
		// the CREATE DATABASE of every package `go test ./...` runs at once, so
		// the wait is server load rather than a hang.
		dropCtx, dropCancel := context.WithTimeout(context.Background(), 120*time.Second)
		defer dropCancel()
		if _, err := adminPool.Exec(dropCtx, "DROP DATABASE "+quotedDatabase+" WITH (FORCE)"); err != nil {
			t.Errorf("drop isolated PostgreSQL integration database: %v", err)
		}
		adminPool.Close()
	})

	applyTenantSchema(t, ctx, pool)
	return pool
}

// applyTenantSchema creates `p_1` and the tenant configuration table inside it,
// from the canonical projection.
func applyTenantSchema(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	baseline, err := os.ReadFile(filepath.Join(
		"..", "..", "..", "db", "schema", "configuration_baseline.sql"))
	if err != nil {
		t.Fatalf("read configuration_baseline.sql: %v", err)
	}

	schema := fmt.Sprintf("p_%d", globalScopeProject)
	if _, err := pool.Exec(ctx, "CREATE SCHEMA "+pgx.Identifier{schema}.Sanitize()); err != nil {
		t.Fatalf("create tenant schema: %v", err)
	}
	// The projection declares an unqualified `configuration`, so the table
	// lands in the tenant schema by setting the search path for this statement
	// rather than by rewriting the file's text.
	if _, err := pool.Exec(ctx,
		"SET LOCAL search_path TO "+pgx.Identifier{schema}.Sanitize()+"; "+string(baseline)); err != nil {
		t.Fatalf("apply configuration_baseline.sql into %s: %v", schema, err)
	}
}

// seedRow inserts one configuration row and returns its id and uuid.
//
// It stamps `updated_at`, which is NOT what a created row looks like. See
// seedNeverUpdatedRow: every row this helper makes has been "edited", so the
// listing tests below could never have met a NULL `updated_at` — which is how
// F1 reached production with this harness already in place.
func seedRow(t *testing.T, pool *pgxpool.Pool, section, configType, title string) (int, string) {
	t.Helper()
	var id int
	var rowUUID string
	err := pool.QueryRow(context.Background(), fmt.Sprintf(`
		INSERT INTO %q.configuration
			(uuid, project_id, elitea_title, type, section, data, meta, shared,
			 status_ok, source, created_at, updated_at)
		VALUES (gen_random_uuid(), $1, $2, $3, $4, '{}'::jsonb, '{}'::jsonb, true,
			 true, 'user', (now() AT TIME ZONE 'UTC'), (now() AT TIME ZONE 'UTC'))
		RETURNING id, uuid::text`, fmt.Sprintf("p_%d", globalScopeProject)),
		globalScopeProject, title, configType, section).Scan(&id, &rowUUID)
	if err != nil {
		t.Fatalf("seed %s/%s row: %v", section, configType, err)
	}
	return id, rowUUID
}

// seedNeverUpdatedRow inserts a row the way CREATE does: `created_at` only.
//
// This is not an exotic state. `updated_at` is nullable in the projection and
// the insert statement in handler.go names neither timestamp, so EVERY row this
// platform has ever created starts here and stays here until someone edits it.
func seedNeverUpdatedRow(t *testing.T, pool *pgxpool.Pool, section, configType, title string) int {
	t.Helper()
	schema := fmt.Sprintf("p_%d", globalScopeProject)
	var id int
	err := pool.QueryRow(context.Background(), fmt.Sprintf(`
		INSERT INTO %q.configuration
			(uuid, project_id, elitea_title, type, section, data, meta, shared,
			 status_ok, source, created_at)
		VALUES (gen_random_uuid(), $1, $2, $3, $4, '{}'::jsonb, '{}'::jsonb, true,
			 true, 'user', (now() AT TIME ZONE 'UTC'))
		RETURNING id`, schema),
		globalScopeProject, title, configType, section).Scan(&id)
	if err != nil {
		t.Fatalf("seed never-updated %s/%s row: %v", section, configType, err)
	}
	var stamped bool
	if err := pool.QueryRow(context.Background(), fmt.Sprintf(
		`SELECT updated_at IS NOT NULL FROM %q.configuration WHERE id = $1`, schema),
		id).Scan(&stamped); err != nil {
		t.Fatalf("read back the seeded row: %v", err)
	}
	// Asserted, not assumed. A projection that grew a DEFAULT or a trigger on
	// `updated_at` would make this seed stamp the column, and the regression
	// below would then pass against the bug it exists to catch.
	if stamped {
		t.Fatalf("the seeded row carries an updated_at; this test cannot see the defect it pins")
	}
	return id
}

// globalScopeRouter mounts both platform surfaces at the paths production uses.
//
// No permission middleware: the gate is applied at the mount in router.go and
// is covered there. What is under test is what happens AFTER a caller is
// admitted, which is exactly where the hole was.
func globalScopeRouter(pool *pgxpool.Pool) chi.Router {
	handler := configurations.NewHandler(pool,
		configurations.WithPublicProjectID(globalScopeProject))
	r := chi.NewRouter()
	r.Route("/gateway", func(r chi.Router) {
		r.Mount("/providers", handler.GlobalProviderRoutes())
		r.Mount("/platform_models", handler.GlobalModelRoutes())
	})
	return r
}

// rowExists reports whether a row with this id is still in the table.
func rowExists(t *testing.T, pool *pgxpool.Pool, id int) bool {
	t.Helper()
	var count int
	if err := pool.QueryRow(context.Background(), fmt.Sprintf(
		`SELECT count(*) FROM %q.configuration WHERE id = $1`,
		fmt.Sprintf("p_%d", globalScopeProject)), id).Scan(&count); err != nil {
		t.Fatalf("count row %d: %v", id, err)
	}
	return count > 0
}

func globalScopeDo(t *testing.T, router chi.Router, method, target string) *httptest.ResponseRecorder {
	t.Helper()
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, httptest.NewRequest(method, target, nil))
	return recorder
}

/* ── the scoping ───────────────────────────────────────────────────────── */

// TestDeleteRefusesARowFromAnotherSectionAndLeavesItAlone is the whole point.
//
// The assertion that matters is the SECOND one: the row survives. A handler
// that answered 404 and deleted the row anyway would pass a status-code test,
// and that is precisely the outcome the guard exists to prevent.
func TestDeleteRefusesARowFromAnotherSectionAndLeavesItAlone(t *testing.T) {
	pool := newGlobalScopePool(t)
	router := globalScopeRouter(pool)

	modelID, _ := seedRow(t, pool, "llm", "llm_model", "gpt-4o")
	credentialID, _ := seedRow(t, pool, "ai_credentials", "open_ai", "platform-openai")

	// The provider surface must not delete a MODEL.
	recorder := globalScopeDo(t, router, http.MethodDelete,
		"/gateway/providers/"+strconv.Itoa(modelID))
	if recorder.Code != http.StatusNotFound {
		t.Errorf("DELETE /providers/{model id} = %d, want 404", recorder.Code)
	}
	if !rowExists(t, pool, modelID) {
		t.Fatal("the provider surface DELETED a platform model")
	}

	// …and the model surface must not delete a CREDENTIAL.
	recorder = globalScopeDo(t, router, http.MethodDelete,
		"/gateway/platform_models/"+strconv.Itoa(credentialID))
	if recorder.Code != http.StatusNotFound {
		t.Errorf("DELETE /platform_models/{credential id} = %d, want 404", recorder.Code)
	}
	if !rowExists(t, pool, credentialID) {
		t.Fatal("the model surface DELETED a platform credential")
	}
}

// TestDeleteStillWorksOnTheSurfacesOwnRow — the other direction, and the one a
// too-strict guard would break silently. A scoping check that refused
// everything would pass the test above and make both surfaces read-only.
func TestDeleteStillWorksOnTheSurfacesOwnRow(t *testing.T) {
	pool := newGlobalScopePool(t)
	router := globalScopeRouter(pool)

	credentialID, _ := seedRow(t, pool, "ai_credentials", "open_ai", "platform-openai")
	modelID, _ := seedRow(t, pool, "llm", "llm_model", "gpt-4o")

	if recorder := globalScopeDo(t, router, http.MethodDelete,
		"/gateway/providers/"+strconv.Itoa(credentialID)); recorder.Code != http.StatusNoContent {
		t.Fatalf("DELETE /providers/{own id} = %d, want 204 (body %s)",
			recorder.Code, recorder.Body.String())
	}
	if rowExists(t, pool, credentialID) {
		t.Error("the credential survived its own surface's delete")
	}

	if recorder := globalScopeDo(t, router, http.MethodDelete,
		"/gateway/platform_models/"+strconv.Itoa(modelID)); recorder.Code != http.StatusNoContent {
		t.Fatalf("DELETE /platform_models/{own id} = %d, want 204 (body %s)",
			recorder.Code, recorder.Body.String())
	}
	if rowExists(t, pool, modelID) {
		t.Error("the model survived its own surface's delete")
	}
}

// TestEveryModelSectionIsItsSurfacesOwn — the model surface admits all five
// dispatchable sections, not just `llm`. A guard that named one would leave
// embeddings, audio and image models undeletable, and the failure would look
// like "the row is not yours".
func TestEveryModelSectionIsItsSurfacesOwn(t *testing.T) {
	pool := newGlobalScopePool(t)
	router := globalScopeRouter(pool)

	for section, configType := range map[string]string{
		"llm":              "llm_model",
		"embedding":        "embedding_model",
		"image_generation": "image_generation_model",
		"asr":              "asr_model",
		"tts":              "tts_model",
	} {
		id, _ := seedRow(t, pool, section, configType, "model-"+section)
		recorder := globalScopeDo(t, router, http.MethodDelete,
			"/gateway/platform_models/"+strconv.Itoa(id))
		if recorder.Code != http.StatusNoContent {
			t.Errorf("DELETE a %s model = %d, want 204 (body %s)",
				section, recorder.Code, recorder.Body.String())
		}
	}
}

// TestTheScopeCheckResolvesAUUIDAsWellAsAnID.
//
// `configurationIDColumn` picks `id` for a numeric segment and `uuid::text`
// otherwise, so the statement has two shapes and only one of them is exercised
// by an integer id. A uuid that silently missed would make every uuid-addressed
// edit answer 404.
func TestTheScopeCheckResolvesAUUIDAsWellAsAnID(t *testing.T) {
	pool := newGlobalScopePool(t)
	router := globalScopeRouter(pool)

	credentialID, credentialUUID := seedRow(t, pool, "ai_credentials", "open_ai", "platform-openai")
	modelID, modelUUID := seedRow(t, pool, "llm", "llm_model", "gpt-4o")

	// Its own row, by uuid: admitted and deleted.
	if recorder := globalScopeDo(t, router, http.MethodDelete,
		"/gateway/providers/"+credentialUUID); recorder.Code != http.StatusNoContent {
		t.Fatalf("DELETE /providers/{own uuid} = %d, want 204 (body %s)",
			recorder.Code, recorder.Body.String())
	}
	if rowExists(t, pool, credentialID) {
		t.Error("the credential survived a delete addressed by uuid")
	}

	// Another surface's row, by uuid: refused, and left alone.
	if recorder := globalScopeDo(t, router, http.MethodDelete,
		"/gateway/providers/"+modelUUID); recorder.Code != http.StatusNotFound {
		t.Errorf("DELETE /providers/{model uuid} = %d, want 404", recorder.Code)
	}
	if !rowExists(t, pool, modelID) {
		t.Fatal("the provider surface deleted a model addressed by uuid")
	}
}

// TestAMissingRowIsRefusedRatherThanReported — a `configID` that names nothing
// answers 404 from the scope check, which is what the delegated Delete would
// have answered anyway. The check must not turn a missing row into a 500.
func TestAMissingRowIsRefusedRatherThanReported(t *testing.T) {
	pool := newGlobalScopePool(t)
	router := globalScopeRouter(pool)

	for _, target := range []string{
		"/gateway/providers/424242",
		"/gateway/platform_models/424242",
		"/gateway/providers/2f1a0f7e-0000-4000-8000-000000000000",
	} {
		if recorder := globalScopeDo(t, router, http.MethodDelete, target); recorder.Code != http.StatusNotFound {
			t.Errorf("DELETE %s = %d, want 404 (body %s)", target, recorder.Code, recorder.Body.String())
		}
	}
}

// TestARowWithNoSectionIsRefusedCleanly — the COALESCE. A NULL section belongs
// to no surface either way; without it the scan errors and the refusal is
// logged as a failure, which sends an operator looking for a fault that is not
// there.
func TestARowWithNoSectionIsRefusedCleanly(t *testing.T) {
	pool := newGlobalScopePool(t)
	router := globalScopeRouter(pool)

	// `section` is NOT NULL in the projection, so the row is relaxed for this
	// case alone — a legacy import is where such a row would come from, and the
	// point is that the reader survives one.
	schema := fmt.Sprintf("p_%d", globalScopeProject)
	if _, err := pool.Exec(context.Background(),
		fmt.Sprintf(`ALTER TABLE %q.configuration ALTER COLUMN section DROP NOT NULL`, schema)); err != nil {
		t.Fatalf("relax the section column: %v", err)
	}
	id, _ := seedRow(t, pool, "ai_credentials", "open_ai", "orphan")
	if _, err := pool.Exec(context.Background(),
		fmt.Sprintf(`UPDATE %q.configuration SET section = NULL WHERE id = $1`, schema), id); err != nil {
		t.Fatalf("null the section: %v", err)
	}

	if recorder := globalScopeDo(t, router, http.MethodDelete,
		"/gateway/providers/"+strconv.Itoa(id)); recorder.Code != http.StatusNotFound {
		t.Errorf("DELETE a NULL-section row = %d, want 404", recorder.Code)
	}
	if !rowExists(t, pool, id) {
		t.Error("a NULL-section row was deleted")
	}
}

/* ── the listing ───────────────────────────────────────────────────────── */

// TestTheProviderListingShowsOnlyCredentials — the read side of the same
// boundary, which was already scoped by its own `WHERE section = …`. Asserted
// against a real table so the two statements are known to agree about what
// belongs to which surface.
func TestTheProviderListingShowsOnlyCredentials(t *testing.T) {
	pool := newGlobalScopePool(t)
	router := globalScopeRouter(pool)

	seedRow(t, pool, "ai_credentials", "open_ai", "platform-openai")
	seedRow(t, pool, "llm", "llm_model", "gpt-4o")
	seedRow(t, pool, "project_settings", "project_context", "context")

	recorder := globalScopeDo(t, router, http.MethodGet, "/gateway/providers")
	if recorder.Code != http.StatusOK {
		t.Fatalf("GET /providers = %d (body %s)", recorder.Code, recorder.Body.String())
	}
	body := recorder.Body.String()
	if !strings.Contains(body, "platform-openai") {
		t.Errorf("the credential is missing from the listing: %s", body)
	}
	for _, foreign := range []string{"gpt-4o", "context"} {
		if strings.Contains(body, foreign) {
			t.Errorf("the provider listing carried %q, which is not a credential: %s", foreign, body)
		}
	}
}

// TestTheModelListingShowsOnlyModels — the same, the other way round.
func TestTheModelListingShowsOnlyModels(t *testing.T) {
	pool := newGlobalScopePool(t)
	router := globalScopeRouter(pool)

	seedRow(t, pool, "ai_credentials", "open_ai", "platform-openai")
	seedRow(t, pool, "llm", "llm_model", "gpt-4o")
	seedRow(t, pool, "project_settings", "project_context", "context")

	recorder := globalScopeDo(t, router, http.MethodGet, "/gateway/platform_models")
	if recorder.Code != http.StatusOK {
		t.Fatalf("GET /platform_models = %d (body %s)", recorder.Code, recorder.Body.String())
	}
	body := recorder.Body.String()
	if !strings.Contains(body, "gpt-4o") {
		t.Errorf("the model is missing from the listing: %s", body)
	}
	if strings.Contains(body, "context") {
		t.Errorf("the model listing carried a project_context row: %s", body)
	}
	// The credential IS named — as a credential a model may link to, in
	// `credential_names`, which is what the form's select offers. It must not
	// appear as an ITEM, and the two are told apart by the key it sits under.
	if !strings.Contains(body, `"credential_names":["platform-openai"]`) {
		t.Errorf("the platform credential is not offered as a link target: %s", body)
	}
}

/* ── the F1 regression ─────────────────────────────────────────────────── */

// TestANewlyCreatedProviderDoesNotBreakTheProviderListing is F1, against a real
// PostgreSQL and a real pgx.
//
// MEASURED through the admin panel: POST /admin/gateway/providers answered 201,
// and every GET after it answered 500 "list failed" with
//
//	can't scan into dest[9] (col: updated_at): cannot scan NULL into *time.Time
//
// A listing raises its scan error per ROW, so the operator did not lose the new
// credential — they lost the whole screen, including every credential that was
// already there, for everyone, until the row was touched in SQL. Publishing a
// platform provider bricked the surface for publishing platform providers.
//
// The status code alone is not the assertion. A handler that skipped unreadable
// rows would answer 200 with the new credential missing — the invisible-data
// shape this codebase keeps meeting — so the row's title and its total are
// checked, and so is the row that was already in the table.
func TestANewlyCreatedProviderDoesNotBreakTheProviderListing(t *testing.T) {
	pool := newGlobalScopePool(t)
	router := globalScopeRouter(pool)

	seedRow(t, pool, "ai_credentials", "open_ai", "existing-openai")
	seedNeverUpdatedRow(t, pool, "ai_credentials", "open_ai", "brand-new-openai")

	recorder := globalScopeDo(t, router, http.MethodGet, "/gateway/providers")
	if recorder.Code != http.StatusOK {
		t.Fatalf("GET /providers = %d after ONE provider was created (body %s)",
			recorder.Code, recorder.Body.String())
	}
	var body struct {
		Total int `json:"total"`
		Items []struct {
			Name      string `json:"elitea_title"`
			CreatedAt string `json:"created_at"`
			UpdatedAt string `json:"updated_at"`
		} `json:"items"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode the listing: %v (body %s)", err, recorder.Body.String())
	}
	if body.Total != 2 || len(body.Items) != 2 {
		t.Fatalf("the listing carries %d of 2 credentials: %s", len(body.Items), recorder.Body.String())
	}
	for _, item := range body.Items {
		if item.CreatedAt == "" {
			t.Errorf("%s reports no creation time", item.Name)
		}
		if item.Name == "brand-new-openai" && item.UpdatedAt != "" {
			t.Errorf("a never-updated row reports updated_at %q rather than nothing", item.UpdatedAt)
		}
		if item.Name == "existing-openai" && item.UpdatedAt == "" {
			t.Errorf("an edited row lost its updated_at")
		}
	}
}

// TestANewlyCreatedModelDoesNotBreakTheModelListing — the model surface's half
// of the same defect. Both are asserted because `scanGlobalModel` carried the
// identical two lines, and a fix applied to one of a pair is how the other half
// survives a repair.
func TestANewlyCreatedModelDoesNotBreakTheModelListing(t *testing.T) {
	pool := newGlobalScopePool(t)
	router := globalScopeRouter(pool)

	seedRow(t, pool, "llm", "llm_model", "existing-gpt-4o")
	seedNeverUpdatedRow(t, pool, "llm", "llm_model", "brand-new-gpt-4o")

	recorder := globalScopeDo(t, router, http.MethodGet, "/gateway/platform_models")
	if recorder.Code != http.StatusOK {
		t.Fatalf("GET /platform_models = %d after ONE model was created (body %s)",
			recorder.Code, recorder.Body.String())
	}
	var body struct {
		Total int `json:"total"`
		Items []struct {
			Name      string `json:"elitea_title"`
			CreatedAt string `json:"created_at"`
			UpdatedAt string `json:"updated_at"`
		} `json:"items"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode the listing: %v (body %s)", err, recorder.Body.String())
	}
	if body.Total != 2 || len(body.Items) != 2 {
		t.Fatalf("the listing carries %d of 2 models: %s", len(body.Items), recorder.Body.String())
	}
	for _, item := range body.Items {
		if item.CreatedAt == "" {
			t.Errorf("%s reports no creation time", item.Name)
		}
		if item.Name == "brand-new-gpt-4o" && item.UpdatedAt != "" {
			t.Errorf("a never-updated row reports updated_at %q rather than nothing", item.UpdatedAt)
		}
	}
}
