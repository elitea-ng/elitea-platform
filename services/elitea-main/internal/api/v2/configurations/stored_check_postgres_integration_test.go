package configurations_test

// The two ROUTE halves of the project-plane stored-credential work, against a
// real PostgreSQL:
//
//	POST /check_stored_connection/{projectID}/{configID} — check a SAVED
//	    credential without the client resending its api_key, and persist
//	    nothing.
//	POST /revalidate/{projectID}/{configID} — re-derive status_ok for a stored
//	    row and persist it.
//
// # Why these cannot be unit tests
//
// stored_check_test.go and revalidate_test.go measure the decisions with fakes,
// and they are the faster feedback. They cannot measure the three facts that
// matter most here, because all three live in the database:
//
//  1. The credential is read from the ROW. The request carries no body at all
//     (http.NoBody below), so a handler that read the key from the request
//     could not pass — and that is the whole point of the route.
//  2. The secret is redeemed through the REAL vault. The seed writes the
//     credential through the real Create route, so the row holds the
//     {{secret.NAME}} reference production stores and the key is in the
//     project vault, wrapped exactly as the running server wraps it.
//  3. status_ok is READ BACK WITH SQL after each call. A handler that believes
//     it wrote the column, and a column that holds the value, are different
//     facts: the status write goes through a second statement outside the
//     read, and "the response said false" would pass against a write that
//     never landed.
//
// # The flip is asserted in BOTH directions
//
// A revalidation test that only asserts the false answer passes against a
// route that always answers false — which is the same defect class as a gate
// that refuses everyone. So the model row is revalidated FIRST with its
// credential in place (and must stay true), then the credential is deleted and
// it is revalidated again (and must go false).

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/testpg"
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

	handler "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/v2/configurations"
	v2secrets "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/v2/secrets"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/db/dbtest"
	dbrepos "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/db/repos"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/runtimecomposition"
)

// The public project the harness composes with, and the second project used to
// prove that a configID from another tenant is not addressable.
const (
	storedCheckProject = 1
	storedCheckOther   = 2
)

const storedCheckDeadline = 120 * time.Second

/* ── harness ───────────────────────────────────────────────────────────── */

// newStoredCheckPool builds an isolated database with the two tenant schemas
// and the centry tables the vault lives in.
//
// It applies 001_initial.sql rather than the configuration projection the
// global-scope harness applies: this file needs centry.secrets_key and
// centry.secrets_data, because a resolution with no vault fails for a reason
// that has nothing to do with the row under test — and an absent vault reading
// as a broken credential is a defect this repository has already met.
func newStoredCheckPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	const environment = "ELITEA_TEST_DATABASE_URL"
	databaseURL := testpg.DSN(os.Getenv(environment))
	if databaseURL == "" {
		t.Skipf("set %s to run the stored-check PostgreSQL integration test", environment)
	}
	// One key source for BOTH the vault creator and the reader composed below.
	// Two key sources compose without an error and leave a vault one path
	// cannot decrypt (#399), which would fail this file with a message about
	// the credential rather than about the key.
	t.Setenv(v2secrets.MasterKeyEnvVar, "")

	ctx, cancel := context.WithTimeout(context.Background(), storedCheckDeadline)
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

	databaseName := fmt.Sprintf("elitea_storedcheck_it_%d_%d", os.Getpid(), time.Now().UnixNano())
	quoted := pgx.Identifier{databaseName}.Sanitize()
	if _, err := adminPool.Exec(ctx, "CREATE DATABASE "+quoted); err != nil {
		adminPool.Close()
		t.Fatalf("create the isolated integration database: %v", err)
	}

	testConfig := adminConfig.Copy()
	testConfig.ConnConfig.Database = databaseName
	testConfig.MaxConns = 6
	pool, err := pgxpool.NewWithConfig(ctx, testConfig)
	if err != nil {
		if _, dropErr := adminPool.Exec(context.Background(),
			"DROP DATABASE "+quoted+" WITH (FORCE)"); dropErr != nil {
			t.Errorf("drop the database after a failed pool open: %v", dropErr)
		}
		adminPool.Close()
		t.Fatalf("open the isolated integration database: %v", err)
	}
	t.Cleanup(func() {
		pool.Close()
		dropCtx, dropCancel := context.WithTimeout(context.Background(), storedCheckDeadline)
		defer dropCancel()
		if _, err := adminPool.Exec(dropCtx, "DROP DATABASE "+quoted+" WITH (FORCE)"); err != nil {
			t.Errorf("drop the isolated integration database: %v", err)
		}
		adminPool.Close()
	})

	source := filepath.Join("..", "..", "..", "infra", "db", "migrations", "001_initial.sql")
	initial, err := os.ReadFile(source)
	if err != nil {
		t.Fatalf("read %s: %v", source, err)
	}
	if _, err := pool.Exec(ctx, string(initial)); err != nil {
		t.Fatalf("apply %s: %v", source, err)
	}
	if _, err := pool.Exec(ctx, fmt.Sprintf(`
INSERT INTO centry.project (id, name, owner_id, create_success)
VALUES (%d, 'public', 1, true), (%d, 'other-tenant', 1, true)
ON CONFLICT (id) DO NOTHING;
SELECT create_tenant_schema('p_%d');
SELECT create_tenant_schema('p_%d');`,
		storedCheckProject, storedCheckOther, storedCheckProject, storedCheckOther)); err != nil {
		t.Fatalf("create the tenant schemas: %v", err)
	}
	// The naive-clock probe the dbtest template installs, after the last
	// schema statement: a writer that stores the session-local clock in a
	// `timestamp` WITHOUT time zone column fails instead of passing unread.
	if err := dbtest.InstallNaiveClockProbe(ctx, pool); err != nil {
		t.Fatal(err)
	}
	return pool
}

// storedCheckRouter mounts the routes exactly as router.go composes them: one
// handler, with the real resolver and the real admission built from the same
// Configurations runtime, and a fake CHECKER — the provider round trip is the
// one thing a test must not make.
func storedCheckRouter(t *testing.T, pool *pgxpool.Pool, checker handler.ConnectionChecker) *chi.Mux {
	t.Helper()
	runtime, err := runtimecomposition.NewCurrentConfigurationsRuntime(pool, storedCheckProject, "", nil)
	if err != nil {
		t.Fatalf("compose the Configurations runtime: %v", err)
	}
	t.Cleanup(runtime.Destroy)

	admission, err := runtimecomposition.NewCurrentProviderAdmission(runtime, true)
	if err != nil {
		t.Fatalf("compose the provider admission: %v", err)
	}
	resolver, err := runtimecomposition.NewCurrentStoredConfigurationResolver(runtime)
	if err != nil {
		t.Fatalf("compose the stored configuration resolver: %v", err)
	}
	sealer, err := dbrepos.NewCurrentSecretVaultRepository(pool, nil,
		dbrepos.WithProjectVaultCreator(v2secrets.NewHandler(pool)))
	if err != nil {
		t.Fatalf("compose the secret sealer: %v", err)
	}

	configurations := handler.NewHandler(pool,
		handler.WithPermissionResolver(entitledResolver()),
		handler.WithProviderAdmission(admission),
		handler.WithStoredConfigurationResolver(resolver),
		handler.WithConnectionChecker(checker),
		handler.WithSecretSealer(sealer),
		handler.WithPublicProjectID(storedCheckProject),
	)
	router := chi.NewRouter()
	router.Use(withTestUser)
	router.Mount("/api/v2/configurations", configurations.Routes())
	// The ADMIN plane's two stored routes, from the SAME handler — which is
	// how router.go composes them, and the reason they can be measured here at
	// all: they carry no {projectID}, so the only project they can name is the
	// public one this handler was built with.
	//
	// The central gate is not applied. It is applied at the mount in router.go
	// and asserted in global_providers_test.go; what this file measures is what
	// happens after a caller is admitted.
	router.Mount("/api/v2/admin/gateway/providers", configurations.GlobalProviderRoutes())
	return router
}

// storedCheckDo issues one request. The stored check and the revalidation both
// take NO body: everything they need is in the row.
func storedCheckDo(t *testing.T, router chi.Router, target string) *httptest.ResponseRecorder {
	t.Helper()
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, target, http.NoBody))
	return recorder
}

// saveCredential creates a credential through the REAL write route, so the row
// and the vault end up exactly as a user's save leaves them: the api_key
// column holds a {{secret.NAME}} reference and the key is in the project
// vault.
func saveCredential(t *testing.T, pool *pgxpool.Pool, router chi.Router, title, apiKey string) int {
	t.Helper()
	body, err := json.Marshal(map[string]any{
		"elitea_title": title,
		"label":        title,
		"type":         "open_ai",
		"section":      "ai_credentials",
		"data":         map[string]any{"api_key": apiKey, "api_base": "https://api.openai.com/v1"},
	})
	if err != nil {
		t.Fatalf("encode the credential: %v", err)
	}
	request := httptest.NewRequest(http.MethodPost,
		fmt.Sprintf("/api/v2/configurations/configurations/%d", storedCheckProject),
		strings.NewReader(string(body)))
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusCreated && recorder.Code != http.StatusOK {
		t.Fatalf("saving the credential answered %d: %s", recorder.Code, recorder.Body.String())
	}
	var created map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode the created credential: %v", err)
	}
	id, ok := created["id"].(float64)
	if !ok {
		t.Fatalf("the created credential carries no id: %s", recorder.Body.String())
	}

	// The seed is only a seed if it stored the reference rather than the key.
	// Without this the "checked without resending the secret" assertion below
	// could be satisfied by a row that still held the plaintext.
	var stored string
	if err := pool.QueryRow(context.Background(), fmt.Sprintf(
		`SELECT data::text FROM p_%d.configuration WHERE id = $1`, storedCheckProject), int(id)).
		Scan(&stored); err != nil {
		t.Fatalf("read the stored credential: %v", err)
	}
	if strings.Contains(stored, apiKey) {
		t.Fatalf("the seed stored the api_key in clear text: %s", stored)
	}
	if !strings.Contains(stored, "{{secret.") {
		t.Fatalf("the seed stored no secret reference: %s", stored)
	}
	return int(id)
}

// seedModel inserts one model row that REFERENCES a credential by title, which
// is the shape whose status_ok stops being true when that credential goes
// away.
func seedModel(t *testing.T, pool *pgxpool.Pool, title string, data map[string]any, statusOK bool) int {
	t.Helper()
	encoded, err := json.Marshal(data)
	if err != nil {
		t.Fatalf("encode the model data: %v", err)
	}
	var id int
	if err := pool.QueryRow(context.Background(), fmt.Sprintf(`
		INSERT INTO p_%d.configuration
			(uuid, project_id, elitea_title, type, section, data, meta, shared,
			 status_ok, source, created_at, updated_at)
		VALUES (gen_random_uuid(), $1, $2, 'llm_model', 'llm', $3::jsonb, '{}'::jsonb, false,
			 $4, 'user', (now() AT TIME ZONE 'UTC'), (now() AT TIME ZONE 'UTC'))
		RETURNING id`, storedCheckProject),
		storedCheckProject, title, string(encoded), statusOK).Scan(&id); err != nil {
		t.Fatalf("seed the model row: %v", err)
	}
	return id
}

func storedStatusOK(t *testing.T, pool *pgxpool.Pool, id int) bool {
	t.Helper()
	var statusOK bool
	if err := pool.QueryRow(context.Background(), fmt.Sprintf(
		`SELECT status_ok FROM p_%d.configuration WHERE id = $1`, storedCheckProject), id).
		Scan(&statusOK); err != nil {
		t.Fatalf("read status_ok of row %d: %v", id, err)
	}
	return statusOK
}

/* ── the stored check ──────────────────────────────────────────────────── */

// THE DISCRIMINATING PROPERTY of the whole change: a saved credential is
// checked, and the request carries nothing.
func TestAStoredCredentialIsCheckedWithoutTheClientResendingTheSecret(t *testing.T) {
	pool := newStoredCheckPool(t)
	checker := &fakeConnectionChecker{
		result: handler.ConnectionCheckResult{Success: true, Message: "Connection successful"},
	}
	router := storedCheckRouter(t, pool, checker)
	id := saveCredential(t, pool, router, "OpenAI", "sk-only-the-vault-has-this")

	recorder := storedCheckDo(t, router, fmt.Sprintf(
		"/api/v2/configurations/check_stored_connection/%d/%d", storedCheckProject, id))

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d. Body: %s", recorder.Code, http.StatusOK, recorder.Body.String())
	}
	if len(checker.calls) != 1 {
		t.Fatalf("the checker ran %d times, want 1: a success reported without a provider "+
			"round trip is the defect this route must not reproduce", len(checker.calls))
	}
	if got := checker.calls[0].data["api_key"]; got != "sk-only-the-vault-has-this" {
		t.Fatalf("the checker received api_key %v.\n"+
			"  The request carried no body, so the ONLY source of this value is the project "+
			"vault, reached through the stored {{secret.NAME}} reference.", got)
	}
}

// The check PERSISTS NOTHING. A provider verdict must not become the admission
// column: status_ok records that the platform accepted the row, and a provider
// outage must not withdraw every credential in the project from the gateway.
func TestAStoredCheckWritesNothing(t *testing.T) {
	pool := newStoredCheckPool(t)
	checker := &fakeConnectionChecker{
		result: handler.ConnectionCheckResult{Success: true, Message: "Connection successful"},
	}
	router := storedCheckRouter(t, pool, checker)
	id := saveCredential(t, pool, router, "OpenAI", "sk-only-the-vault-has-this")

	before := storedRowSnapshot(t, pool, id)
	// A successful check first, then a rejecting one: neither direction may
	// move the column.
	if recorder := storedCheckDo(t, router, fmt.Sprintf(
		"/api/v2/configurations/check_stored_connection/%d/%d", storedCheckProject, id,
	)); recorder.Code != http.StatusOK {
		t.Fatalf("the successful check answered %d: %s", recorder.Code, recorder.Body.String())
	}
	checker.result = handler.ConnectionCheckResult{Success: false, Message: "The provider rejected the credential."}
	if recorder := storedCheckDo(t, router, fmt.Sprintf(
		"/api/v2/configurations/check_stored_connection/%d/%d", storedCheckProject, id,
	)); recorder.Code != http.StatusBadRequest {
		t.Fatalf("the rejected check answered %d, want %d", recorder.Code, http.StatusBadRequest)
	}

	if after := storedRowSnapshot(t, pool, id); after != before {
		t.Fatalf("the stored row changed across two checks.\n  before: %s\n  after:  %s", before, after)
	}
}

// A configID that belongs to ANOTHER project is not addressable, and the
// provider is never dialled for it.
//
// The schema in every statement is built from the {projectID} in the path, so
// this is a property of the query rather than of a comparison a later edit
// could drop — which is exactly why it is asserted against a real database.
func TestAStoredCheckOfAnotherProjectsConfigurationIsNotFound(t *testing.T) {
	pool := newStoredCheckPool(t)
	checker := &fakeConnectionChecker{
		result: handler.ConnectionCheckResult{Success: true, Message: "Connection successful"},
	}
	router := storedCheckRouter(t, pool, checker)
	id := saveCredential(t, pool, router, "OpenAI", "sk-only-the-vault-has-this")

	recorder := storedCheckDo(t, router, fmt.Sprintf(
		"/api/v2/configurations/check_stored_connection/%d/%d", storedCheckOther, id))

	if recorder.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d. A credential of project %d must not be checkable "+
			"through project %d's path.", recorder.Code, http.StatusNotFound,
			storedCheckProject, storedCheckOther)
	}
	if len(checker.calls) != 0 {
		t.Fatalf("the provider was dialled %d times for another project's row, want 0", len(checker.calls))
	}
}

// The BATCH form, against the real table.
//
// Its statement is the one piece of SQL in this change that no unit test can
// reach: it binds two arrays and casts them (`id = ANY($1::integer[]) OR
// uuid::text = ANY($2::text[])`), so a wrong cast fails only against
// PostgreSQL — and it would fail as "no such configuration" for every row,
// which reads as an empty project rather than as a broken query.
func TestTheStoredBatchCheckAnswersOneRowPerRequestedIDInOrder(t *testing.T) {
	pool := newStoredCheckPool(t)
	checker := &fakeConnectionChecker{
		result: handler.ConnectionCheckResult{Success: true, Message: "Connection successful"},
	}
	router := storedCheckRouter(t, pool, checker)
	first := saveCredential(t, pool, router, "OpenAI", "sk-only-the-vault-has-this")
	second := saveCredential(t, pool, router, "OpenAI Two", "sk-second")
	var secondUUID string
	if err := pool.QueryRow(context.Background(), fmt.Sprintf(
		`SELECT uuid::text FROM p_%d.configuration WHERE id = $1`, storedCheckProject), second).
		Scan(&secondUUID); err != nil {
		t.Fatalf("read the second credential's uuid: %v", err)
	}

	// The shipped client sends STRINGS
	// (apps/elitea-web/src/features/credentials/api/configurations.ts,
	// batchCheckStoredConfigurationConnections), and a row may be addressed by
	// id or by uuid, so all three forms are exercised together. The last id
	// names no row.
	body, err := json.Marshal(map[string]any{
		"configuration_ids": []any{strconv.Itoa(first), secondUUID, "999999"},
	})
	if err != nil {
		t.Fatalf("encode the batch request: %v", err)
	}
	request := httptest.NewRequest(http.MethodPost, fmt.Sprintf(
		"/api/v2/configurations/check_stored_connections/%d", storedCheckProject),
		strings.NewReader(string(body)))
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d. The credential screen marks EVERY credential invalid "+
			"when this request fails. Body: %s", recorder.Code, http.StatusOK, recorder.Body.String())
	}
	var rows []map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &rows); err != nil {
		t.Fatalf("decode the batch response: %v", err)
	}
	if len(rows) != 3 {
		t.Fatalf("the batch answered %d rows for 3 ids: %s", len(rows), recorder.Body.String())
	}
	if rows[0]["id"] != strconv.Itoa(first) || rows[1]["id"] != secondUUID || rows[2]["id"] != "999999" {
		t.Fatalf("the ids came back reordered or rewritten: %s", recorder.Body.String())
	}
	if rows[0]["success"] != true || rows[1]["success"] != true {
		t.Fatalf("a stored credential did not check: %s", recorder.Body.String())
	}
	if rows[2]["success"] != false {
		t.Fatalf("an id naming no row reported success: %s", recorder.Body.String())
	}
	// Two rows were found and checked; the third was not. A statement that
	// matched nothing would also produce three rows — all of them failures —
	// so the checker count is what separates the two.
	if len(checker.calls) != 2 {
		t.Fatalf("the provider was dialled %d times, want 2 (one per row that exists)", len(checker.calls))
	}
}

/* ── the revalidation ──────────────────────────────────────────────────── */

// The flip, in both directions, read back with SQL.
func TestRevalidateFlipsStatusOKWhenTheReferencedCredentialIsDeleted(t *testing.T) {
	pool := newStoredCheckPool(t)
	router := storedCheckRouter(t, pool, &fakeConnectionChecker{})
	saveCredential(t, pool, router, "OpenAI", "sk-only-the-vault-has-this")
	model := seedModel(t, pool, "gpt-4o", map[string]any{
		"name":           "gpt-4o",
		"ai_credentials": map[string]any{"elitea_title": "OpenAI"},
	}, false)

	// Direction one. The credential is there, so the row resolves and the
	// revalidation must say so. Without this half, a route that always
	// answered false would pass the half below.
	target := fmt.Sprintf("/api/v2/configurations/revalidate/%d/%d", storedCheckProject, model)
	if recorder := storedCheckDo(t, router, target); recorder.Code != http.StatusOK {
		t.Fatalf("the first revalidation answered %d: %s", recorder.Code, recorder.Body.String())
	}
	if !storedStatusOK(t, pool, model) {
		t.Fatal("the model does not resolve with its credential in place; the flip below would " +
			"prove nothing")
	}

	// Direction two. The credential goes away, and nothing re-runs admission
	// on its own — which is the gap this route fills.
	if _, err := pool.Exec(context.Background(), fmt.Sprintf(
		`DELETE FROM p_%d.configuration WHERE elitea_title = 'OpenAI'`, storedCheckProject)); err != nil {
		t.Fatalf("delete the referenced credential: %v", err)
	}

	recorder := storedCheckDo(t, router, target)
	if recorder.Code != http.StatusOK {
		t.Fatalf("the second revalidation answered %d: %s", recorder.Code, recorder.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode the revalidated row: %v", err)
	}
	if body["status_ok"] != false {
		t.Fatalf("the response reports status_ok = %v, want false", body["status_ok"])
	}
	// THE ASSERTION THAT MATTERS. The response is what the handler believes;
	// this is what the gateway will read.
	if storedStatusOK(t, pool, model) {
		t.Fatal("p_1.configuration.status_ok is still true.\n" +
			"  The response said false, so the status write did not land — and every reader " +
			"of a provider row selects on this column.")
	}
}

// An UNMANAGED row is left exactly as it is. A model with no ai_credentials is
// an imported definition: it declares no references and holds no secrets, so
// there is nothing to resolve and its status belongs to whoever wrote it.
func TestRevalidateLeavesAnUnmanagedRowUntouched(t *testing.T) {
	pool := newStoredCheckPool(t)
	router := storedCheckRouter(t, pool, &fakeConnectionChecker{})
	// status_ok TRUE, and nothing in this project resolves anything: a route
	// that revalidated it anyway would report false.
	imported := seedModel(t, pool, "imported-model", map[string]any{"name": "imported-model"}, true)
	before := storedRowSnapshot(t, pool, imported)

	recorder := storedCheckDo(t, router, fmt.Sprintf(
		"/api/v2/configurations/revalidate/%d/%d", storedCheckProject, imported))

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", recorder.Code, recorder.Body.String())
	}
	if !storedStatusOK(t, pool, imported) {
		t.Fatal("an unmanaged row's status_ok was written to false by a decision that does not own it")
	}
	if after := storedRowSnapshot(t, pool, imported); after != before {
		t.Fatalf("the revalidation changed the row.\n  before: %s\n  after:  %s", before, after)
	}
}

// The revalidation writes the STATUS and no field of the row — no data, no
// meta, no title, no updated_at. It is the one write on this route, and a
// route that also touched a field would silently rewrite rows a user did not
// edit.
func TestRevalidateWritesNoFieldOfTheRow(t *testing.T) {
	pool := newStoredCheckPool(t)
	router := storedCheckRouter(t, pool, &fakeConnectionChecker{})
	saveCredential(t, pool, router, "OpenAI", "sk-only-the-vault-has-this")
	model := seedModel(t, pool, "gpt-4o", map[string]any{
		"name":           "gpt-4o",
		"ai_credentials": map[string]any{"elitea_title": "OpenAI"},
	}, false)

	before := storedRowFields(t, pool, model)
	if recorder := storedCheckDo(t, router, fmt.Sprintf(
		"/api/v2/configurations/revalidate/%d/%d", storedCheckProject, model,
	)); recorder.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", recorder.Code, recorder.Body.String())
	}
	if after := storedRowFields(t, pool, model); after != before {
		t.Fatalf("the revalidation rewrote a field.\n  before: %s\n  after:  %s", before, after)
	}
	// ... and it DID write the status, so the case above is not passing
	// because nothing happened at all.
	if !storedStatusOK(t, pool, model) {
		t.Fatal("the revalidation wrote no status; this case would then measure nothing")
	}
}

// A row of another project is not revalidatable through this project's path,
// for the same reason it is not checkable.
func TestRevalidateOfAnotherProjectsConfigurationIsNotFound(t *testing.T) {
	pool := newStoredCheckPool(t)
	router := storedCheckRouter(t, pool, &fakeConnectionChecker{})
	imported := seedModel(t, pool, "imported-model", map[string]any{"name": "imported-model"}, true)

	recorder := storedCheckDo(t, router, fmt.Sprintf(
		"/api/v2/configurations/revalidate/%d/%d", storedCheckOther, imported))

	if recorder.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusNotFound)
	}
	if !storedStatusOK(t, pool, imported) {
		t.Fatal("a cross-project revalidation wrote the row's status anyway")
	}
}

/* ── row snapshots ─────────────────────────────────────────────────────── */

// storedRowSnapshot renders the WHOLE row, status included, so a case can
// assert that nothing at all moved.
func storedRowSnapshot(t *testing.T, pool *pgxpool.Pool, id int) string {
	t.Helper()
	var snapshot string
	if err := pool.QueryRow(context.Background(), fmt.Sprintf(`
		SELECT concat_ws('|', elitea_title, type, section, data::text, meta::text,
			shared::text, status_ok::text, coalesce(status_logs, ''), source,
			coalesce(updated_at::text, ''))
		FROM p_%d.configuration WHERE id = $1`, storedCheckProject), id).Scan(&snapshot); err != nil {
		t.Fatalf("snapshot row %d: %v", id, err)
	}
	return snapshot
}

// storedRowFields renders every column EXCEPT the status pair, so a case can
// assert that the status moved and nothing else did.
func storedRowFields(t *testing.T, pool *pgxpool.Pool, id int) string {
	t.Helper()
	var fields string
	if err := pool.QueryRow(context.Background(), fmt.Sprintf(`
		SELECT concat_ws('|', elitea_title, type, section, data::text, meta::text,
			shared::text, source, coalesce(author_id::text, ''),
			created_at::text, coalesce(updated_at::text, ''))
		FROM p_%d.configuration WHERE id = $1`, storedCheckProject), id).Scan(&fields); err != nil {
		t.Fatalf("read the fields of row %d: %v", id, err)
	}
	return fields
}

/* ── the admin plane ───────────────────────────────────────────────────── */

// The same two executors, reached from `/api/v2/admin/gateway/providers`,
// where the caller names no project at all.
//
// WHAT THESE ADD over the project-plane cases above is the one thing the admin
// routes own: the project. Everything else — the row read, the vault redeem,
// the status write — is the same code, and re-asserting it would only prove
// that delegation happens. What cannot be proved anywhere else is that the
// project the executor works on is the PUBLIC one. Nothing in the request says
// so: there is no {projectID} segment, so a handler that defaulted to the
// caller's project, to project 1, or to nothing at all would look identical
// from outside and would be discovered as "the platform credential screen's
// Test button never works".

// platformProviderPath is the admin route for one stored provider row.
func platformProviderPath(id int, action string) string {
	return fmt.Sprintf("/api/v2/admin/gateway/providers/%d/%s", id, action)
}

// TestAPlatformCredentialIsCheckedAgainstThePublicProjectsVault.
//
// The assertion that discriminates is the api_key the CHECKER received. The
// request carries no body, and the key exists in exactly one place: the public
// project's vault, reached through the {{secret.NAME}} reference in the public
// project's row. An admin route that resolved against any other project would
// answer "this credential could not be resolved" — a real-looking failure that
// an operator would read as a broken credential.
func TestAPlatformCredentialIsCheckedAgainstThePublicProjectsVault(t *testing.T) {
	pool := newStoredCheckPool(t)
	checker := &fakeConnectionChecker{
		result: handler.ConnectionCheckResult{Success: true, Message: "Connection successful"},
	}
	router := storedCheckRouter(t, pool, checker)
	id := saveCredential(t, pool, router, "Platform OpenAI", "sk-only-the-public-vault-has-this")
	before := storedRowSnapshot(t, pool, id)

	recorder := storedCheckDo(t, router, platformProviderPath(id, "check"))

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d. Body: %s", recorder.Code, http.StatusOK, recorder.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode the check answer: %v", err)
	}
	// The project plane's contract, byte for byte: the same browser control
	// renders both answers.
	if body["success"] != true {
		t.Fatalf("the answer is %v, want the project plane's {\"success\":true,\"message\":…}", body)
	}
	if len(checker.calls) != 1 {
		t.Fatalf("the checker ran %d times, want 1: a success reported without a provider "+
			"round trip is the defect this route must not reproduce", len(checker.calls))
	}
	if got := checker.calls[0].data["api_key"]; got != "sk-only-the-public-vault-has-this" {
		t.Fatalf("the checker received api_key %v.\n"+
			"  The request carried no body and no project id, so the ONLY source of this value "+
			"is the PUBLIC project's vault — which is the fact this route exists to establish.",
			got)
	}
	// And it persisted nothing, on the admin plane as on the project plane. A
	// provider verdict written into status_ok would let one outage withdraw
	// every platform credential from the gateway at once.
	if after := storedRowSnapshot(t, pool, id); after != before {
		t.Fatalf("the platform check changed the row.\n  before: %s\n  after:  %s", before, after)
	}
}

// TestAPlatformRevalidationWritesStatusOKOnThePublicProjectsRow — the flip,
// read back with SQL from p_1.
//
// Both directions are asserted, because a route that always answered false
// would pass the second half alone. The lever is the row's own secret
// reference: admission redeems it, so a reference to a secret that is not
// there is a credential the gateway must stop serving — and until this route
// existed nothing re-derived that column after the row was written.
func TestAPlatformRevalidationWritesStatusOKOnThePublicProjectsRow(t *testing.T) {
	pool := newStoredCheckPool(t)
	router := storedCheckRouter(t, pool, &fakeConnectionChecker{})
	id := saveCredential(t, pool, router, "Platform OpenAI", "sk-only-the-public-vault-has-this")

	// Direction one: the credential resolves, so the revalidation must say so.
	if recorder := storedCheckDo(t, router, platformProviderPath(id, "revalidate")); recorder.Code != http.StatusOK {
		t.Fatalf("the first revalidation answered %d: %s", recorder.Code, recorder.Body.String())
	}
	if !storedStatusOK(t, pool, id) {
		t.Fatal("the freshly saved platform credential does not resolve; the flip below would " +
			"prove nothing")
	}

	// Direction two: the reference stops redeeming, and nothing re-runs
	// admission on its own.
	if _, err := pool.Exec(context.Background(), fmt.Sprintf(
		`UPDATE p_%d.configuration
		    SET data = jsonb_set(data, '{api_key}', '"{{secret.0000000000000000000000000000dead}}"')
		  WHERE id = $1`, storedCheckProject), id); err != nil {
		t.Fatalf("break the stored secret reference: %v", err)
	}

	recorder := storedCheckDo(t, router, platformProviderPath(id, "revalidate"))
	if recorder.Code != http.StatusOK {
		t.Fatalf("the second revalidation answered %d: %s", recorder.Code, recorder.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode the revalidated row: %v", err)
	}
	// The project plane's response shape: the whole Configuration object, so
	// the admin screen can replace the row it holds without a second read.
	if body["status_ok"] != false {
		t.Fatalf("the response reports status_ok = %v, want false. Body: %s",
			body["status_ok"], recorder.Body.String())
	}
	// The project plane's Configuration DTO, field for field — `name` carries
	// elitea_title there, and the admin screen decodes the same shape it
	// decodes for the credentials screen.
	if body["name"] != "Platform OpenAI" {
		t.Fatalf("the answer is not the row: %s", recorder.Body.String())
	}
	// THE ASSERTION THAT MATTERS. The response is what the handler believes;
	// this is the column the gateway reads, in the public project's schema.
	if storedStatusOK(t, pool, id) {
		t.Fatal("p_1.configuration.status_ok is still true.\n" +
			"  The response said false, so the status write did not land on the public " +
			"project's row — and every gateway read of a provider row selects on this column.")
	}
}

// TestThePlatformStoredRoutesRefuseARowThisSurfaceDoesNotOwn.
//
// Three ways a {configID} can fail to name a platform provider, and all three
// must answer 404 WITHOUT dialling a provider and WITHOUT writing a status:
//
//   - a row of another SECTION — the delegated handlers address a row by id
//     alone, so without the fence this route dials the provider with, or
//     re-derives the status of, any row the public project holds;
//   - a row of another PROJECT — the statements are built from the public
//     project's schema, so a p_2 id names nothing here;
//   - an id that names no row at all.
func TestThePlatformStoredRoutesRefuseARowThisSurfaceDoesNotOwn(t *testing.T) {
	pool := newStoredCheckPool(t)
	checker := &fakeConnectionChecker{
		result: handler.ConnectionCheckResult{Success: true, Message: "Connection successful"},
	}
	router := storedCheckRouter(t, pool, checker)

	// A model row of the public project: right project, wrong surface.
	model := seedModel(t, pool, "gpt-4o", map[string]any{"name": "gpt-4o"}, true)

	// A credential of the OTHER tenant, at an id p_1 does not hold.
	const foreignID = 777777
	if _, err := pool.Exec(context.Background(), fmt.Sprintf(`
		INSERT INTO p_%d.configuration
			(id, uuid, project_id, elitea_title, type, section, data, meta, shared,
			 status_ok, source, created_at, updated_at)
		VALUES ($1, gen_random_uuid(), $2, 'Tenant OpenAI', 'open_ai', 'ai_credentials',
			'{}'::jsonb, '{}'::jsonb, true, true, 'user', (now() AT TIME ZONE 'UTC'), (now() AT TIME ZONE 'UTC'))`, storedCheckOther),
		foreignID, storedCheckOther); err != nil {
		t.Fatalf("seed the other tenant's credential: %v", err)
	}

	for name, id := range map[string]int{
		"another section": model,
		"another project": foreignID,
		"no row at all":   424242,
	} {
		for _, action := range []string{"check", "revalidate", "models"} {
			recorder := storedCheckDo(t, router, platformProviderPath(id, action))
			if recorder.Code != http.StatusNotFound {
				t.Errorf("%s: POST .../%d/%s = %d, want 404 (body %s)",
					name, id, action, recorder.Code, recorder.Body.String())
			}
		}
	}

	if len(checker.calls) != 0 {
		t.Errorf("the provider was dialled %d times for rows this surface does not own, want 0",
			len(checker.calls))
	}
	if !storedStatusOK(t, pool, model) {
		t.Error("a refused platform revalidation rewrote the model's status anyway")
	}
}

// listingChecker is a connection checker that ALSO lists models, which is what
// the composed gateway client is: elitea-main reads the listing capability off
// the checker it already composed, so a fake that implemented only one of the
// two would not stand where the real client stands.
type listingChecker struct {
	fakeConnectionChecker
	listed  []checkerCall
	listing handler.ProviderModelListing
	listErr error
}

func (l *listingChecker) ListProviderModels(
	_ context.Context, configType string, data map[string]any,
) (handler.ProviderModelListing, error) {
	l.listed = append(l.listed, checkerCall{configType: configType, data: data})
	return l.listing, l.listErr
}

// TestAPlatformProvidersModelsAreListedWithThePublicProjectsVaultKey.
//
// The successor to legacy's `import_llm_models`, at the route where the
// substitution actually happens. Legacy read LiteLLM's own model table; this
// asks the PROVIDER, with the credential the platform already holds.
//
// The assertion that discriminates is the api_key the LISTER received. The
// request carries no body and no project id, so the key exists in exactly one
// place: the public project's vault, reached through the {{secret.NAME}}
// reference in the public project's row. A route that resolved against any
// other project, or that forwarded the stored reference unresolved, would ask
// the provider to authenticate a template string and report a working
// credential as broken.
func TestAPlatformProvidersModelsAreListedWithThePublicProjectsVaultKey(t *testing.T) {
	pool := newStoredCheckPool(t)
	lister := &listingChecker{listing: handler.ProviderModelListing{
		Success: true, Models: []string{"gpt-4o", "gpt-4o-mini"},
	}}
	router := storedCheckRouter(t, pool, lister)
	id := saveCredential(t, pool, router, "Platform OpenAI", "sk-only-the-public-vault-has-this")
	before := storedRowSnapshot(t, pool, id)

	recorder := storedCheckDo(t, router, platformProviderPath(id, "models"))

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200. Body: %s", recorder.Code, recorder.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode the listing: %v", err)
	}
	models, ok := body["models"].([]any)
	if !ok || len(models) != 2 || models[0] != "gpt-4o" {
		t.Fatalf("models = %v, want the provider's own ids in the provider's own order", body["models"])
	}
	if body["type"] != "open_ai" {
		t.Fatalf("type = %v, want the stored row's own type", body["type"])
	}

	if len(lister.listed) != 1 {
		t.Fatalf("the lister ran %d times, want 1: a catalogue reported without asking the "+
			"provider is the defect this route must not reproduce", len(lister.listed))
	}
	if got := lister.listed[0].data["api_key"]; got != "sk-only-the-public-vault-has-this" {
		t.Fatalf("the lister received api_key %v.\n"+
			"  The request carried no body and no project id, so the ONLY source of this value "+
			"is the PUBLIC project's vault — which is the fact this route exists to establish.",
			got)
	}
	if len(lister.recordedCalls()) != 0 {
		t.Fatalf("the listing ran a connection CHECK as well (%d times); the two are separate "+
			"round trips and one must not stand in for the other", len(lister.recordedCalls()))
	}

	// Reading a provider's catalogue is not adopting it, and not admission.
	// The legacy task's own failure mode was writing rows nobody chose.
	if after := storedRowSnapshot(t, pool, id); after != before {
		t.Fatalf("listing the provider's models changed the row.\n  before: %s\n  after:  %s", before, after)
	}
}

// TestAPlatformModelListingReportsARefusalRatherThanAnEmptyCatalogue.
//
// An empty list is the answer an operator acts on — "this provider offers no
// models" sends them to check the wrong thing entirely. A provider verdict has
// to arrive as a refusal, with the gateway's own safe sentence.
func TestAPlatformModelListingReportsARefusalRatherThanAnEmptyCatalogue(t *testing.T) {
	pool := newStoredCheckPool(t)
	lister := &listingChecker{listing: handler.ProviderModelListing{
		Success: false, Message: "The provider rejected the credential.",
	}}
	router := storedCheckRouter(t, pool, lister)
	id := saveCredential(t, pool, router, "Platform OpenAI", "sk-only-the-public-vault-has-this")

	recorder := storedCheckDo(t, router, platformProviderPath(id, "models"))

	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400. Body: %s", recorder.Code, recorder.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode the refusal: %v", err)
	}
	if body["error"] != "The provider rejected the credential." {
		t.Fatalf("error = %v, want the gateway's own verdict", body["error"])
	}
	if _, present := body["models"]; present {
		t.Fatalf("a refusal carried a models field: %s", recorder.Body.String())
	}
}
