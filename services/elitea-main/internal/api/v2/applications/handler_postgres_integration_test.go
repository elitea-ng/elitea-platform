package applications_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	handler "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/applications"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/db"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/db/repos"
)

// These tests drive the HTTP handler over the real repository against the
// PostgreSQL service the Test job in .github/workflows/ci-go.yml provisions
// (ELITEA_TEST_DATABASE_URL), so they cover the wiring the unit tests' mock
// repository cannot: which user id reaches applications.owner_id, and whether
// the version SQL matches the actual DDL.

const postgresIntegrationDatabaseURL = "ELITEA_TEST_DATABASE_URL"

func newHandlerTestPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	databaseURL := os.Getenv(postgresIntegrationDatabaseURL)
	if databaseURL == "" && os.Getenv("ELITEA_TEST_USE_SERVICE_DATABASE_URL") == "1" {
		databaseURL = os.Getenv("DATABASE_URL")
	}
	if databaseURL == "" {
		t.Skipf("set %s to run the PostgreSQL service-integration test", postgresIntegrationDatabaseURL)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	adminConfig, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		t.Fatalf("parse %s: %v", postgresIntegrationDatabaseURL, err)
	}
	adminConfig.MaxConns = 4
	adminPool, err := pgxpool.NewWithConfig(ctx, adminConfig)
	if err != nil {
		t.Fatalf("open PostgreSQL admin pool: %v", err)
	}
	databaseName := fmt.Sprintf("elitea_apps_it_%d_%d", os.Getpid(), time.Now().UnixNano())
	quoted := pgx.Identifier{databaseName}.Sanitize()
	if _, err := adminPool.Exec(ctx, "CREATE DATABASE "+quoted); err != nil {
		adminPool.Close()
		t.Fatalf("create isolated database: %v", err)
	}
	testConfig := adminConfig.Copy()
	testConfig.ConnConfig.Database = databaseName
	testConfig.MaxConns = 8
	pool, err := pgxpool.NewWithConfig(ctx, testConfig)
	if err != nil {
		_, _ = adminPool.Exec(context.Background(), "DROP DATABASE "+quoted+" WITH (FORCE)")
		adminPool.Close()
		t.Fatalf("open isolated database: %v", err)
	}
	t.Cleanup(func() {
		pool.Close()
		// 120 s, not the old 20 s to 30 s. This DROP queues behind the
		// CREATE DATABASE calls of every package that `go test ./...` runs at
		// the same time, so the wait is server load and not a hang. Two full
		// runs failed here with "drop isolated ... database: timeout" (#409).
		dropCtx, dropCancel := context.WithTimeout(context.Background(), 120*time.Second)
		defer dropCancel()
		if _, err := adminPool.Exec(dropCtx, "DROP DATABASE "+quoted+" WITH (FORCE)"); err != nil {
			t.Errorf("drop isolated database: %v", err)
		}
		adminPool.Close()
	})
	if err := db.RunMigrations(ctx, pool); err != nil {
		t.Fatalf("run baseline migrations: %v", err)
	}
	return pool
}

// newHandlerTestServer mounts the same route table internal/api/router.go
// registers behind cfg.AppsRepo, with `user` as the authenticated principal.
func newHandlerTestServer(t *testing.T, pool *pgxpool.Pool, user auth.User) *chi.Mux {
	t.Helper()
	h := handler.NewHandler(repos.NewApplicationsRepo(pool), pool)
	r := chi.NewRouter()
	r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			if user.ID == "" {
				next.ServeHTTP(w, req)
				return
			}
			next.ServeHTTP(w, req.WithContext(auth.ContextWithUser(req.Context(), user)))
		})
	})
	r.Get("/applications/prompt_lib/{projectID}", h.List)
	r.Post("/applications/prompt_lib/{projectID}", h.Create)
	r.Get("/application/prompt_lib/{projectID}/{applicationID}", h.Get)
	r.Put("/application/prompt_lib/{projectID}/{applicationID}", h.Update)
	r.Delete("/application/prompt_lib/{projectID}/{applicationID}", h.Delete)
	r.Get("/versions/prompt_lib/{projectID}/{applicationID}", h.ListVersions)
	r.Post("/versions/prompt_lib/{projectID}/{applicationID}", h.CreateVersion)
	r.Get("/version/prompt_lib/{projectID}/{applicationID}/{versionID}", h.GetVersion)
	r.Put("/version/prompt_lib/{projectID}/{applicationID}/{versionID}", h.UpdateVersion)
	r.Delete("/version/prompt_lib/{projectID}/{applicationID}/{versionID}", h.DeleteVersion)
	// PATCH on this path is the expanded READ the SDK calls, not an update
	// (#336). It is registered here because this server re-declares the router's
	// table rather than importing it; a route missing here is invisible to every
	// test in this file.
	r.Patch("/version/prompt_lib/{projectID}/{applicationID}/{versionID}", h.GetVersionExpanded)
	r.Get("/default_version/prompt_lib/{projectID}/{applicationID}", h.GetDefaultVersion)
	r.Patch("/default_version/prompt_lib/{projectID}/{applicationID}/{versionID}", h.SetDefaultVersion)
	return r
}

func seedHandlerUser(t *testing.T, pool *pgxpool.Pool, id int64, email string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if _, err := pool.Exec(ctx,
		`INSERT INTO public.auth_core__user (id, email, name) VALUES ($1, $2, $3)
		 ON CONFLICT (id) DO UPDATE SET email = EXCLUDED.email`, id, email, "User "+email); err != nil {
		t.Fatalf("seed user: %v", err)
	}
}

func do(t *testing.T, router *chi.Mux, method, path string, body any) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	var reader *bytes.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("encode body: %v", err)
		}
		reader = bytes.NewReader(encoded)
	} else {
		reader = bytes.NewReader(nil)
	}
	request := httptest.NewRequest(method, path, reader)
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)

	decoded := map[string]any{}
	if recorder.Body.Len() > 0 {
		_ = json.Unmarshal(recorder.Body.Bytes(), &decoded)
	}
	return recorder, decoded
}

// j14CreateBody is the shape apps/elitea-web sends from the create-agent form
// (entities/application-form/model/mutations.ts toVersionWriteRequest).
func j14CreateBody(name string) map[string]any {
	return map[string]any{
		"name":        name,
		"description": "created by the journey",
		"type":        "agent",
		"versions": []any{map[string]any{
			"name":                  "base",
			"agent_type":            "openai",
			"instructions":          "Follow the brief.",
			"conversation_starters": []any{"hello"},
			"variables":             []any{map[string]any{"name": "k", "value": "v"}},
			"meta":                  map[string]any{"step_limit": float64(25)},
		}},
	}
}

func TestHandlerPostgres_CreatePersistsTheAuthenticatedPrincipalAsOwner(t *testing.T) {
	pool := newHandlerTestPool(t)
	seedHandlerUser(t, pool, 42, "fortytwo@elitea.ai")
	router := newHandlerTestServer(t, pool, auth.User{ID: "42", UserID: "42", Email: "fortytwo@elitea.ai"})

	recorder, created := do(t, router, http.MethodPost, "/applications/prompt_lib/1", j14CreateBody("owned"))
	if recorder.Code != http.StatusCreated {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	applicationID, _ := created["id"].(string)
	if applicationID == "" {
		t.Fatalf("response carries no application id: %s", recorder.Body.String())
	}
	if created["owner_id"] != "42" {
		t.Errorf("response owner_id = %v, want \"42\"", created["owner_id"])
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	var ownerID, authorID int64
	if err := pool.QueryRow(ctx, `
		SELECT a.owner_id, v.author_id
		FROM p_1.applications a JOIN p_1.application_versions v ON v.application_id = a.id
		WHERE a.id = $1`, applicationID).Scan(&ownerID, &authorID); err != nil {
		t.Fatalf("read back: %v", err)
	}
	if ownerID != 42 {
		t.Errorf("applications.owner_id = %d, want 42 — not the hardcoded user 1", ownerID)
	}
	if authorID != 42 {
		t.Errorf("application_versions.author_id = %d, want 42", authorID)
	}
}

func TestHandlerPostgres_CreateWithoutAPrincipalIsRefused(t *testing.T) {
	pool := newHandlerTestPool(t)
	router := newHandlerTestServer(t, pool, auth.User{})

	recorder, _ := do(t, router, http.MethodPost, "/applications/prompt_lib/1", j14CreateBody("anonymous"))
	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401; body = %s", recorder.Code, recorder.Body.String())
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	var count int
	if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM p_1.applications`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Errorf("%d applications created without a principal, want 0", count)
	}
}

// TestHandlerPostgres_CreateThenReadBackIsWhatJourney14Asserts follows the
// exact request sequence the Playwright journey drives: POST the create form,
// then land on /agents/$tab/$agentId, which GETs the application and expects
// its versions and version_details back.
func TestHandlerPostgres_CreateThenReadBackIsWhatJourney14Asserts(t *testing.T) {
	pool := newHandlerTestPool(t)
	seedHandlerUser(t, pool, 1, "one@elitea.ai")
	router := newHandlerTestServer(t, pool, auth.User{ID: "1", UserID: "1", Email: "one@elitea.ai"})

	recorder, created := do(t, router, http.MethodPost, "/applications/prompt_lib/1", j14CreateBody("autotest-e2e-agent"))
	if recorder.Code != http.StatusCreated {
		t.Fatalf("create status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	applicationID := created["id"].(string)

	versionDetails, ok := created["version_details"].(map[string]any)
	if !ok {
		t.Fatalf("create response has no version_details: %s", recorder.Body.String())
	}
	if versionDetails["instructions"] != "Follow the brief." {
		t.Errorf("version_details.instructions = %v", versionDetails["instructions"])
	}
	if variables, _ := versionDetails["variables"].([]any); len(variables) != 1 {
		t.Errorf("version_details.variables = %v, want the one variable that was sent", versionDetails["variables"])
	}

	// The agent editor immediately re-reads the application by the id it was
	// handed. A create that returned the wrong identifier, or that skipped
	// the version row, does not survive this.
	recorder, fetched := do(t, router, http.MethodGet, "/application/prompt_lib/1/"+applicationID, nil)
	if recorder.Code != http.StatusOK {
		t.Fatalf("get status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	if fetched["name"] != "autotest-e2e-agent" {
		t.Errorf("fetched name = %v", fetched["name"])
	}
	versions, _ := fetched["versions"].([]any)
	if len(versions) != 1 {
		t.Fatalf("fetched versions = %v, want exactly the initial version", fetched["versions"])
	}
	if _, ok := fetched["version_details"].(map[string]any); !ok {
		t.Errorf("fetched response has no version_details: %s", recorder.Body.String())
	}

	// And the agent list the journey returns to must contain it.
	recorder, listed := do(t, router, http.MethodGet, "/applications/prompt_lib/1?limit=20&offset=0", nil)
	if recorder.Code != http.StatusOK {
		t.Fatalf("list status = %d", recorder.Code)
	}
	rows, _ := listed["rows"].([]any)
	if len(rows) != 1 {
		t.Fatalf("list rows = %v, want the created agent", listed["rows"])
	}
	if row, _ := rows[0].(map[string]any); row["name"] != "autotest-e2e-agent" {
		t.Errorf("listed row = %v", rows[0])
	}
}

// TestHandlerPostgres_UpdateVersionPersists covers the regression where the
// handler opened its SET list with `updated_at = now()` — a column
// application_versions does not have — so every version save returned a 500.
func TestHandlerPostgres_UpdateVersionPersists(t *testing.T) {
	pool := newHandlerTestPool(t)
	seedHandlerUser(t, pool, 1, "one@elitea.ai")
	router := newHandlerTestServer(t, pool, auth.User{ID: "1", UserID: "1", Email: "one@elitea.ai"})

	_, created := do(t, router, http.MethodPost, "/applications/prompt_lib/1", j14CreateBody("editable"))
	applicationID := created["id"].(string)
	versionID := created["version_details"].(map[string]any)["id"].(string)

	recorder, updated := do(t, router, http.MethodPut,
		fmt.Sprintf("/version/prompt_lib/1/%s/%s", applicationID, versionID),
		map[string]any{
			"instructions":    "Revised brief.",
			"welcome_message": "hey",
			"llm_settings":    map[string]any{"model_name": "gpt-4o"},
		})
	if recorder.Code != http.StatusCreated {
		t.Fatalf("update status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	if updated["instructions"] != "Revised brief." || updated["welcome_message"] != "hey" {
		t.Errorf("update response = %s", recorder.Body.String())
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	var instructions, welcome, llm string
	if err := pool.QueryRow(ctx,
		`SELECT instructions, welcome_message, llm_settings::text FROM p_1.application_versions WHERE id = $1`,
		versionID).Scan(&instructions, &welcome, &llm); err != nil {
		t.Fatalf("read back: %v", err)
	}
	if instructions != "Revised brief." || welcome != "hey" || !strings.Contains(llm, "gpt-4o") {
		t.Errorf("row = instructions=%q welcome=%q llm=%s", instructions, welcome, llm)
	}
}

// TestHandlerPostgres_UpdateVersionPersistsVariables covers #307: the agent
// editor sends `variables` on every save and UpdateVersion had no branch for
// the key, so the PUT answered 201 and the edit was gone. Asserted by
// reading the row back (and by re-GETting the version), not by the 201 —
// a 201 is exactly what passed while the field was being dropped.
func TestHandlerPostgres_UpdateVersionPersistsVariables(t *testing.T) {
	pool := newHandlerTestPool(t)
	seedHandlerUser(t, pool, 1, "one@elitea.ai")
	router := newHandlerTestServer(t, pool, auth.User{ID: "1", UserID: "1", Email: "one@elitea.ai"})

	_, created := do(t, router, http.MethodPost, "/applications/prompt_lib/1", j14CreateBody("variable-editor"))
	applicationID := created["id"].(string)
	versionDetails := created["version_details"].(map[string]any)
	versionID := versionDetails["id"].(string)

	// The body the web client actually sends: `meta` spread from the stored
	// blob (so it still carries the OLD variables) plus a separate, edited
	// `variables` list. See pages/agents/lib/editApplicationMappers.ts.
	storedMeta, _ := versionDetails["meta"].(map[string]any)
	if storedMeta == nil {
		storedMeta = map[string]any{}
	}
	recorder, updated := do(t, router, http.MethodPut,
		fmt.Sprintf("/version/prompt_lib/1/%s/%s", applicationID, versionID),
		map[string]any{
			"name": "base",
			"meta": storedMeta,
			"variables": []any{
				map[string]any{"name": "region", "value": "emea"},
				map[string]any{"name": "tier", "value": "gold"},
			},
		})
	if recorder.Code != http.StatusCreated {
		t.Fatalf("update status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	echoed, _ := updated["variables"].([]any)
	if len(echoed) != 2 {
		t.Errorf("update response variables = %v, want the 2 edited variables", updated["variables"])
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	var meta string
	if err := pool.QueryRow(ctx,
		`SELECT meta::text FROM p_1.application_versions WHERE id = $1`, versionID).Scan(&meta); err != nil {
		t.Fatalf("read back: %v", err)
	}
	if !strings.Contains(meta, "region") || !strings.Contains(meta, "emea") {
		t.Errorf("stored meta = %s, want the edited variables", meta)
	}
	if strings.Contains(meta, `"k"`) {
		t.Errorf("stored meta = %s, still carries the pre-edit variable", meta)
	}
	if !strings.Contains(meta, "step_limit") {
		t.Errorf("stored meta = %s, folding variables dropped the rest of meta", meta)
	}

	_, reread := do(t, router, http.MethodGet,
		fmt.Sprintf("/version/prompt_lib/1/%s/%s", applicationID, versionID), nil)
	rereadVariables, _ := reread["variables"].([]any)
	if len(rereadVariables) != 2 {
		t.Errorf("GET after save returned variables = %v, want the 2 saved variables", reread["variables"])
	}
}

// TestHandlerPostgres_CreatePersistsVariables is the other half of #307: the
// create paths folded `variables` into `meta` and stopped there, so an agent
// created WITH variables answered 201 with them echoed back and then returned
// `"variables": []` on every GET until someone saved the version. Nothing in
// the create response discriminates — the echo is built from `meta`, which
// really is written — so the assertion that matters is the GET, plus the
// `application_variables` rows the GET reads from.
func TestHandlerPostgres_CreatePersistsVariables(t *testing.T) {
	pool := newHandlerTestPool(t)
	seedHandlerUser(t, pool, 1, "one@elitea.ai")
	router := newHandlerTestServer(t, pool, auth.User{ID: "1", UserID: "1", Email: "one@elitea.ai"})

	body := j14CreateBody("created-with-variables")
	body["versions"].([]any)[0].(map[string]any)["variables"] = []any{
		map[string]any{"name": "region", "value": "emea"},
		map[string]any{"name": "tier", "value": "gold"},
		// The blank row the editor emits the moment a user clicks "add": it
		// must not be stored, and must not fail the create either.
		map[string]any{"name": "", "value": ""},
	}
	recorder, created := do(t, router, http.MethodPost, "/applications/prompt_lib/1", body)
	if recorder.Code != http.StatusCreated {
		t.Fatalf("create status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	applicationID := created["id"].(string)
	versionID := created["version_details"].(map[string]any)["id"].(string)

	assertStoredVariables(t, pool, versionID, map[string]string{"region": "emea", "tier": "gold"})

	// The reload the agent editor performs the instant it opens the new agent.
	getRecorder, fetched := do(t, router, http.MethodGet,
		fmt.Sprintf("/version/prompt_lib/1/%s/%s", applicationID, versionID), nil)
	if getRecorder.Code != http.StatusOK {
		t.Fatalf("get status = %d, body = %s", getRecorder.Code, getRecorder.Body.String())
	}
	assertVariablesInResponse(t, fetched, map[string]string{"region": "emea", "tier": "gold"})
}

// TestHandlerPostgres_CreateVersionPersistsVariables covers the second create
// entry point — "save as a new version" in the editor — which builds its
// version through the same versionFromBody and had the same gap.
func TestHandlerPostgres_CreateVersionPersistsVariables(t *testing.T) {
	pool := newHandlerTestPool(t)
	seedHandlerUser(t, pool, 1, "one@elitea.ai")
	router := newHandlerTestServer(t, pool, auth.User{ID: "1", UserID: "1", Email: "one@elitea.ai"})

	_, created := do(t, router, http.MethodPost, "/applications/prompt_lib/1", j14CreateBody("version-source"))
	applicationID := created["id"].(string)

	recorder, second := do(t, router, http.MethodPost, "/versions/prompt_lib/1/"+applicationID,
		map[string]any{
			"name":       "v2",
			"agent_type": "openai",
			"variables": []any{
				map[string]any{"name": "channel", "value": "slack"},
			},
		})
	if recorder.Code != http.StatusCreated {
		t.Fatalf("create version status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	secondID := second["id"].(string)

	assertStoredVariables(t, pool, secondID, map[string]string{"channel": "slack"})

	getRecorder, fetched := do(t, router, http.MethodGet,
		fmt.Sprintf("/version/prompt_lib/1/%s/%s", applicationID, secondID), nil)
	if getRecorder.Code != http.StatusOK {
		t.Fatalf("get status = %d, body = %s", getRecorder.Code, getRecorder.Body.String())
	}
	assertVariablesInResponse(t, fetched, map[string]string{"channel": "slack"})

	// The new version owns its own variables: the base version it was created
	// alongside keeps the one it was created with, unchanged.
	baseID := created["version_details"].(map[string]any)["id"].(string)
	assertStoredVariables(t, pool, baseID, map[string]string{"k": "v"})
}

func assertStoredVariables(t *testing.T, pool *pgxpool.Pool, versionID string, want map[string]string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	rows, err := pool.Query(ctx,
		`SELECT name, COALESCE(value, '') FROM p_1.application_variables WHERE application_version_id = $1`, versionID)
	if err != nil {
		t.Fatalf("read application_variables: %v", err)
	}
	defer rows.Close()
	got := map[string]string{}
	for rows.Next() {
		var name, value string
		if err := rows.Scan(&name, &value); err != nil {
			t.Fatalf("scan application_variables: %v", err)
		}
		got[name] = value
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate application_variables: %v", err)
	}
	if len(got) != len(want) {
		t.Errorf("application_variables rows for version %s = %v, want %v", versionID, got, want)
	}
	for name, value := range want {
		if got[name] != value {
			t.Errorf("application_variables[%q] = %q, want %q (all rows: %v)", name, got[name], value, got)
		}
	}
}

func assertVariablesInResponse(t *testing.T, response map[string]any, want map[string]string) {
	t.Helper()
	raw, _ := response["variables"].([]any)
	got := map[string]string{}
	for _, entry := range raw {
		item, _ := entry.(map[string]any)
		if item == nil {
			continue
		}
		name, _ := item["name"].(string)
		value, _ := item["value"].(string)
		got[name] = value
	}
	if len(got) != len(want) {
		t.Errorf("GET variables = %v, want %v", response["variables"], want)
	}
	for name, value := range want {
		if got[name] != value {
			t.Errorf("GET variables[%q] = %q, want %q (all: %v)", name, got[name], value, response["variables"])
		}
	}
}

// TestHandlerPostgres_UpdateReportsTheRowOwner pins that editing an
// application does not transfer or misreport its ownership: the prototype
// echoed whoever was making the request back as owner_id.
func TestHandlerPostgres_UpdateReportsTheRowOwner(t *testing.T) {
	pool := newHandlerTestPool(t)
	seedHandlerUser(t, pool, 11, "eleven@elitea.ai")
	seedHandlerUser(t, pool, 12, "twelve@elitea.ai")

	owner := newHandlerTestServer(t, pool, auth.User{ID: "11", UserID: "11", Email: "eleven@elitea.ai"})
	_, created := do(t, owner, http.MethodPost, "/applications/prompt_lib/1", j14CreateBody("owned-by-eleven"))
	applicationID, _ := created["id"].(string)
	if applicationID == "" {
		t.Fatalf("create failed: %v", created)
	}

	editor := newHandlerTestServer(t, pool, auth.User{ID: "12", UserID: "12", Email: "twelve@elitea.ai"})
	recorder, updated := do(t, editor, http.MethodPut, "/application/prompt_lib/1/"+applicationID,
		map[string]any{"name": "renamed-by-twelve"})
	if recorder.Code != http.StatusCreated {
		t.Fatalf("update status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	if updated["owner_id"] != "11" {
		t.Errorf("update response owner_id = %v, want the row's owner \"11\"", updated["owner_id"])
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	var ownerID int64
	if err := pool.QueryRow(ctx, `SELECT owner_id FROM p_1.applications WHERE id = $1`, applicationID).Scan(&ownerID); err != nil {
		t.Fatal(err)
	}
	if ownerID != 11 {
		t.Errorf("applications.owner_id = %d after an edit by user 12, want 11", ownerID)
	}
}

func TestHandlerPostgres_SetAndGetDefaultVersion(t *testing.T) {
	pool := newHandlerTestPool(t)
	seedHandlerUser(t, pool, 1, "one@elitea.ai")
	router := newHandlerTestServer(t, pool, auth.User{ID: "1", UserID: "1", Email: "one@elitea.ai"})

	_, created := do(t, router, http.MethodPost, "/applications/prompt_lib/1", j14CreateBody("defaults"))
	applicationID := created["id"].(string)

	recorder, second := do(t, router, http.MethodPost, "/versions/prompt_lib/1/"+applicationID,
		map[string]any{"name": "v2", "agent_type": "openai"})
	if recorder.Code != http.StatusCreated {
		t.Fatalf("create version status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	secondID := second["id"].(string)

	// Before any default is set, the resolution falls back to the version
	// named "base" — the same rule the UI's selectDefaultVersion applies.
	_, fallback := do(t, router, http.MethodGet, "/default_version/prompt_lib/1/"+applicationID, nil)
	if fallback["name"] != "base" {
		t.Errorf("fallback default version = %v, want the version named base", fallback["name"])
	}

	recorder, _ = do(t, router, http.MethodPatch,
		fmt.Sprintf("/default_version/prompt_lib/1/%s/%s", applicationID, secondID), nil)
	if recorder.Code != http.StatusOK {
		t.Fatalf("set default status = %d, body = %s", recorder.Code, recorder.Body.String())
	}

	_, resolved := do(t, router, http.MethodGet, "/default_version/prompt_lib/1/"+applicationID, nil)
	if resolved["id"] != secondID {
		t.Errorf("default version = %v, want %q", resolved["id"], secondID)
	}
	if resolved["is_default"] != true {
		t.Errorf("default version is_default = %v, want true", resolved["is_default"])
	}
}

func TestHandlerPostgres_NonNumericProjectIDIsRejectedWithoutReachingSQL(t *testing.T) {
	pool := newHandlerTestPool(t)
	seedHandlerUser(t, pool, 1, "one@elitea.ai")
	router := newHandlerTestServer(t, pool, auth.User{ID: "1", UserID: "1", Email: "one@elitea.ai"})

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if _, err := pool.Exec(ctx, `CREATE TABLE p_1.handler_canary (id int)`); err != nil {
		t.Fatalf("create canary: %v", err)
	}

	// Percent-encoded in the request target; chi decodes it back to the raw
	// text before the handler ever sees it, which is exactly the path a real
	// hostile request takes.
	// chi routes on the raw path, so a handler receives the project id still
	// percent-encoded. Both shapes are covered: the long encoded injection
	// attempt, and short non-numeric ids that no length bound would catch.
	projectIDs := []string{
		url.PathEscape(`1"."applications"; DROP TABLE p_1.handler_canary; SELECT * FROM "p_1"."applications`),
		"abc",
		"1a",
		"-1",
		"1.0",
	}

	for _, projectID := range projectIDs {
		for _, request := range []struct {
			method string
			path   string
			body   any
		}{
			{http.MethodGet, "/applications/prompt_lib/" + projectID, nil},
			{http.MethodPost, "/applications/prompt_lib/" + projectID, j14CreateBody("x")},
			{http.MethodGet, "/application/prompt_lib/" + projectID + "/1", nil},
			{http.MethodPut, "/application/prompt_lib/" + projectID + "/1", map[string]any{"name": "x"}},
			{http.MethodDelete, "/application/prompt_lib/" + projectID + "/1", nil},
			{http.MethodGet, "/versions/prompt_lib/" + projectID + "/1", nil},
			{http.MethodPost, "/versions/prompt_lib/" + projectID + "/1", map[string]any{"name": "v"}},
			{http.MethodGet, "/version/prompt_lib/" + projectID + "/1/1", nil},
			{http.MethodPut, "/version/prompt_lib/" + projectID + "/1/1", map[string]any{"name": "v"}},
			{http.MethodDelete, "/version/prompt_lib/" + projectID + "/1/1", nil},
			{http.MethodGet, "/default_version/prompt_lib/" + projectID + "/1", nil},
			{http.MethodPatch, "/default_version/prompt_lib/" + projectID + "/1/1", nil},
		} {
			name := fmt.Sprintf("%s %s %s", projectID[:min(len(projectID), 12)],
				request.method, strings.SplitN(request.path, "/", 3)[1])
			t.Run(name, func(t *testing.T) {
				recorder, decoded := do(t, router, request.method, request.path, request.body)
				if recorder.Code < 400 || recorder.Code >= 500 {
					t.Errorf("status = %d, want a 4xx rejection; body = %s", recorder.Code, recorder.Body.String())
				}
				if message, _ := decoded["error"].(string); strings.Contains(message, "SQLSTATE") {
					t.Errorf("error body leaks a SQL error: %s", message)
				}
			})
		}
	}

	var canaryAlive bool
	if err := pool.QueryRow(ctx, `SELECT EXISTS (
		SELECT 1 FROM information_schema.tables WHERE table_schema='p_1' AND table_name='handler_canary')`,
	).Scan(&canaryAlive); err != nil {
		t.Fatal(err)
	}
	if !canaryAlive {
		t.Fatal("injected SQL executed: the canary table was dropped")
	}
}

// TestHandlerPostgres_UpdateVersionPersistsPipelineSettings covers #135: the
// pipeline editor saved the flow graph, the PUT answered 200/201, and the
// nodes/edges were never written — pipeline_settings had no read in the
// handler and no SET clause in the repository.
func TestHandlerPostgres_UpdateVersionPersistsPipelineSettings(t *testing.T) {
	pool := newHandlerTestPool(t)
	seedHandlerUser(t, pool, 1, "one@elitea.ai")
	router := newHandlerTestServer(t, pool, auth.User{ID: "1", UserID: "1", Email: "one@elitea.ai"})

	_, created := do(t, router, http.MethodPost, "/applications/prompt_lib/1", j14CreateBody("graph-owner"))
	applicationID := created["id"].(string)
	versionID := created["version_details"].(map[string]any)["id"].(string)

	graphYAML := "nodes:\n  - id: Agent 1\n    type: llm\nentry_point: Agent 1\n"
	recorder, _ := do(t, router, http.MethodPut,
		fmt.Sprintf("/version/prompt_lib/1/%s/%s", applicationID, versionID),
		map[string]any{
			"name":         "base",
			"agent_type":   "pipeline",
			"instructions": graphYAML,
			"pipeline_settings": map[string]any{
				"nodes":          []any{map[string]any{"id": "Agent 1", "position": map[string]any{"x": 42, "y": 99}}},
				"edges":          []any{},
				"orientation":    "vertical",
				"layout_version": "1.0",
			},
		})
	if recorder.Code != http.StatusCreated {
		t.Fatalf("update status = %d, body = %s", recorder.Code, recorder.Body.String())
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	var instructions, pipelineSettings string
	if err := pool.QueryRow(ctx,
		`SELECT instructions, pipeline_settings::text FROM p_1.application_versions WHERE id = $1`,
		versionID).Scan(&instructions, &pipelineSettings); err != nil {
		t.Fatalf("read back: %v", err)
	}
	if instructions != graphYAML {
		t.Errorf("instructions = %q, want the edited pipeline YAML", instructions)
	}
	var stored map[string]any
	if err := json.Unmarshal([]byte(pipelineSettings), &stored); err != nil {
		t.Fatalf("stored pipeline_settings is not JSON (%v): %s", err, pipelineSettings)
	}
	if stored["layout_version"] != "1.0" || stored["orientation"] != "vertical" {
		t.Errorf("pipeline_settings = %s", pipelineSettings)
	}
	storedNodes, _ := stored["nodes"].([]any)
	if len(storedNodes) != 1 {
		t.Fatalf("pipeline_settings.nodes = %v", stored["nodes"])
	}
	if node, _ := storedNodes[0].(map[string]any); node["id"] != "Agent 1" {
		t.Errorf("pipeline_settings.nodes[0] = %v", storedNodes[0])
	}

	// The GET the editor reloads through must hand the same graph back.
	getRecorder, fetched := do(t, router, http.MethodGet,
		fmt.Sprintf("/version/prompt_lib/1/%s/%s", applicationID, versionID), nil)
	if getRecorder.Code != http.StatusOK {
		t.Fatalf("get status = %d, body = %s", getRecorder.Code, getRecorder.Body.String())
	}
	settings, ok := fetched["pipeline_settings"].(map[string]any)
	if !ok || settings["layout_version"] != "1.0" {
		t.Errorf("GET pipeline_settings = %v", fetched["pipeline_settings"])
	}
	if fetched["instructions"] != graphYAML {
		t.Errorf("GET instructions = %v", fetched["instructions"])
	}
}

func TestHandlerPostgres_CreatePathsPersistPipelineSettingsAndTags(t *testing.T) {
	pool := newHandlerTestPool(t)
	seedHandlerUser(t, pool, 15, "fifteen@elitea.ai")
	router := newHandlerTestServer(t, pool, auth.User{ID: "15", UserID: "15", Email: "fifteen@elitea.ai"})

	createBody := j14CreateBody("created-pipeline")
	initial := createBody["versions"].([]any)[0].(map[string]any)
	initial["agent_type"] = "pipeline"
	initial["pipeline_settings"] = map[string]any{"trigger": "manual", "nodes": []any{}}
	initial["tags"] = []any{map[string]any{"name": "created-tag", "data": map[string]any{"color": "blue"}}}
	recorder, created := do(t, router, http.MethodPost, "/applications/prompt_lib/1", createBody)
	if recorder.Code != http.StatusCreated {
		t.Fatalf("create application status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	applicationID := created["id"].(string)
	initialVersion := created["version_details"].(map[string]any)
	initialVersionID := initialVersion["id"].(string)
	if names := responseTagNames(t, initialVersion); !sameNames(names, []string{"created-tag"}) {
		t.Fatalf("initial response tags = %v", names)
	}

	versionBody := map[string]any{
		"name":              "second",
		"agent_type":        "pipeline",
		"instructions":      "nodes: []",
		"pipeline_settings": map[string]any{"trigger": "scheduled", "nodes": []any{}},
		"tags":              []any{map[string]any{"name": "second-tag"}},
	}
	recorder, second := do(t, router, http.MethodPost,
		fmt.Sprintf("/versions/prompt_lib/1/%s", applicationID), versionBody)
	if recorder.Code != http.StatusCreated {
		t.Fatalf("create version status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	secondVersionID := second["id"].(string)

	for versionID, wantTrigger := range map[string]string{
		initialVersionID: "manual",
		secondVersionID:  "scheduled",
	} {
		var settingsText string
		if err := pool.QueryRow(context.Background(), `
			SELECT pipeline_settings::text FROM p_1.application_versions WHERE id = $1`, versionID).Scan(&settingsText); err != nil {
			t.Fatalf("read pipeline settings for %s: %v", versionID, err)
		}
		var settings map[string]any
		if err := json.Unmarshal([]byte(settingsText), &settings); err != nil {
			t.Fatalf("decode pipeline settings for %s: %v", versionID, err)
		}
		if settings["trigger"] != wantTrigger {
			t.Fatalf("version %s trigger = %v, want %q", versionID, settings["trigger"], wantTrigger)
		}
	}
	if names := storedVersionTagNames(t, pool, initialVersionID); !sameNames(names, []string{"created-tag"}) {
		t.Fatalf("initial stored tags = %v", names)
	}
	if names := storedVersionTagNames(t, pool, secondVersionID); !sameNames(names, []string{"second-tag"}) {
		t.Fatalf("second stored tags = %v", names)
	}
}

// storedVersionTagNames reads the tag names the version's association rows
// point at — the database assertion issue #345 asks for, not the response
// body. A 201 proved nothing here: the handler answered one on every save
// while writing no association row at all.
func storedVersionTagNames(t *testing.T, pool *pgxpool.Pool, versionID string) []string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	rows, err := pool.Query(ctx, `
		SELECT t.name
		FROM p_1.application_version_tag_association a
		JOIN p_1.tags t ON t.id = a.tag_id
		WHERE a.version_id = $1
		ORDER BY t.name`, versionID)
	if err != nil {
		t.Fatalf("read the association rows: %v", err)
	}
	defer rows.Close()
	names := []string{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatalf("scan an association row: %v", err)
		}
		names = append(names, name)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read the association rows: %v", err)
	}
	return names
}

// responseTagNames reads the `tags` list out of a version-detail body.
func responseTagNames(t *testing.T, body map[string]any) []string {
	t.Helper()
	raw, ok := body["tags"].([]any)
	if !ok {
		t.Fatalf("response carries no tags list: %v", body["tags"])
	}
	names := []string{}
	for _, entry := range raw {
		tag, _ := entry.(map[string]any)
		if tag == nil {
			t.Fatalf("a tag entry is not an object: %v", entry)
		}
		name, _ := tag["name"].(string)
		names = append(names, name)
	}
	return names
}

func sameNames(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range want {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

// TestHandlerPostgres_UpdateVersionPersistsTags is issue #345's regression
// test. Before the fix UpdateVersion read no `tags` key at all, and both read
// paths answered a hardcoded `[]`, so a tag edit was accepted with a 201 and
// then lost twice over: nothing was written, and the echo said "no tags"
// about the list the user had just sent.
func TestHandlerPostgres_UpdateVersionPersistsTags(t *testing.T) {
	pool := newHandlerTestPool(t)
	seedHandlerUser(t, pool, 7, "seven@elitea.ai")
	router := newHandlerTestServer(t, pool, auth.User{ID: "7", UserID: "7", Email: "seven@elitea.ai"})

	recorder, created := do(t, router, http.MethodPost, "/applications/prompt_lib/1", j14CreateBody("tagged"))
	if recorder.Code != http.StatusCreated {
		t.Fatalf("create status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	applicationID, _ := created["id"].(string)
	versionDetails, _ := created["version_details"].(map[string]any)
	versionID, _ := versionDetails["id"].(string)
	if applicationID == "" || versionID == "" {
		t.Fatalf("create response carries no ids: %s", recorder.Body.String())
	}
	versionPath := fmt.Sprintf("/version/prompt_lib/1/%s/%s", applicationID, versionID)

	// 1. A save that carries tags writes the association rows.
	recorder, saved := do(t, router, http.MethodPut, versionPath, map[string]any{
		"name":         "base",
		"instructions": "Follow the brief.",
		"tags": []any{
			map[string]any{"name": "beta"},
			map[string]any{"name": "alpha", "data": map[string]any{"color": "one"}},
		},
	})
	if recorder.Code != http.StatusCreated {
		t.Fatalf("save status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	if names := storedVersionTagNames(t, pool, versionID); !sameNames(names, []string{"alpha", "beta"}) {
		t.Fatalf("association rows = %v, want [alpha beta] — the tag edit was discarded", names)
	}
	// The save echo must report what was stored, not the empty list it used
	// to hardcode.
	if names := responseTagNames(t, saved); !sameNames(names, []string{"alpha", "beta"}) {
		t.Errorf("save echo tags = %v, want [alpha beta]", names)
	}

	// 2. The reload the editor issues hands the same tags back.
	recorder, fetched := do(t, router, http.MethodGet, versionPath, nil)
	if recorder.Code != http.StatusOK {
		t.Fatalf("get status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	if names := responseTagNames(t, fetched); !sameNames(names, []string{"alpha", "beta"}) {
		t.Errorf("GET tags = %v, want [alpha beta]", names)
	}
	storedTags, _ := fetched["tags"].([]any)
	first, _ := storedTags[0].(map[string]any)
	if _, hasID := first["id"]; !hasID {
		t.Errorf("GET tags[0] carries no id: %v", first)
	}
	if data, _ := first["data"].(map[string]any); data["color"] != "one" {
		t.Errorf("GET tags[0].data = %v, want the data that was sent", first["data"])
	}

	// 3. A save with no `tags` key leaves the stored set alone, exactly as
	// it leaves `variables` alone.
	recorder, _ = do(t, router, http.MethodPut, versionPath, map[string]any{"instructions": "Second pass."})
	if recorder.Code != http.StatusCreated {
		t.Fatalf("save status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	if names := storedVersionTagNames(t, pool, versionID); !sameNames(names, []string{"alpha", "beta"}) {
		t.Errorf("association rows = %v after a save with no tags key, want [alpha beta]", names)
	}

	// 4. Removing a tag deletes its association row, and leaves the shared
	// `tags` row for other versions.
	recorder, _ = do(t, router, http.MethodPut, versionPath, map[string]any{
		"tags": []any{map[string]any{"name": "alpha"}},
	})
	if recorder.Code != http.StatusCreated {
		t.Fatalf("save status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	if names := storedVersionTagNames(t, pool, versionID); !sameNames(names, []string{"alpha"}) {
		t.Errorf("association rows = %v, want [alpha] — the removed tag kept its row", names)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	var betaCount int
	if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM p_1.tags WHERE name = 'beta'`).Scan(&betaCount); err != nil {
		t.Fatalf("read the tags table: %v", err)
	}
	if betaCount != 1 {
		t.Errorf("p_1.tags rows named beta = %d, want 1 — the tag itself is shared, only the association goes", betaCount)
	}

	// 5. An empty list removes every association row: "I deleted my last
	// tag" is a real user action and must reach the database.
	recorder, emptied := do(t, router, http.MethodPut, versionPath, map[string]any{"tags": []any{}})
	if recorder.Code != http.StatusCreated {
		t.Fatalf("save status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	if names := storedVersionTagNames(t, pool, versionID); len(names) != 0 {
		t.Errorf("association rows = %v after an empty tags list, want none", names)
	}
	if names := responseTagNames(t, emptied); len(names) != 0 {
		t.Errorf("save echo tags = %v after an empty list, want none", names)
	}
}

// TestHandlerPostgres_ExpandedVersionDetailCarriesTheStoredTags covers the
// PATCH read the SDK uses (#336). It hardcoded the same empty list.
func TestHandlerPostgres_ExpandedVersionDetailCarriesTheStoredTags(t *testing.T) {
	pool := newHandlerTestPool(t)
	seedHandlerUser(t, pool, 8, "eight@elitea.ai")
	router := newHandlerTestServer(t, pool, auth.User{ID: "8", UserID: "8", Email: "eight@elitea.ai"})

	recorder, created := do(t, router, http.MethodPost, "/applications/prompt_lib/1", j14CreateBody("expanded-tags"))
	if recorder.Code != http.StatusCreated {
		t.Fatalf("create status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	applicationID, _ := created["id"].(string)
	versionDetails, _ := created["version_details"].(map[string]any)
	versionID, _ := versionDetails["id"].(string)
	versionPath := fmt.Sprintf("/version/prompt_lib/1/%s/%s", applicationID, versionID)

	recorder, _ = do(t, router, http.MethodPut, versionPath, map[string]any{
		"tags": []any{map[string]any{"name": "support"}},
	})
	if recorder.Code != http.StatusCreated {
		t.Fatalf("save status = %d, body = %s", recorder.Code, recorder.Body.String())
	}

	// The expanded read is the SDK's, and it checks X-SECRET against the
	// project vault. This project has no vault, so pylon's own default
	// value applies (see TestVersionPatchFallsBackToThePylonDefaultSecret).
	request := httptest.NewRequest(http.MethodPatch, versionPath, bytes.NewReader(nil))
	request.Header.Set("X-SECRET", "secret")
	patchRecorder := httptest.NewRecorder()
	router.ServeHTTP(patchRecorder, request)
	if patchRecorder.Code != http.StatusOK {
		t.Fatalf("expanded status = %d, body = %s", patchRecorder.Code, patchRecorder.Body.String())
	}
	expanded := map[string]any{}
	if err := json.Unmarshal(patchRecorder.Body.Bytes(), &expanded); err != nil {
		t.Fatalf("expanded body is not JSON (%v): %s", err, patchRecorder.Body.String())
	}
	if names := responseTagNames(t, expanded); !sameNames(names, []string{"support"}) {
		t.Errorf("expanded tags = %v, want [support]", names)
	}
}
