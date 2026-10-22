package eliteacore_test

// Issue #504 — the toolkit import wrote elitea_tools WITHOUT owner_id, and the
// migration corpus declares that column NOT NULL with no default. So every
// toolkit import failed with SQLSTATE 23502 on any deployment whose schema this
// repository built.
//
// Two things make this class of fault invisible, and this file removes both.
//
//  1. A legacy-era schema has no owner_id column on elitea_tools at all
//     (internal/db/schema/toolkit_baseline.sql, the sqlc projection of the
//     deployed table: id, created_at, updated_at, type, name, description,
//     settings, author_id, shared_owner_id, shared_id, meta). The identical
//     statement therefore succeeds there. The column is an invention of
//     internal/infra/db/migrations/001_initial.sql:435-447, which is the schema
//     a fresh install gets — and the direction the platform moves in.
//  2. Every other PostgreSQL test near this handler builds the schema it needs
//     by hand — platform_flags_postgres_integration_test.go CREATEs a stub
//     p_<id>.applications of four columns. A hand-made table cannot carry a
//     constraint nobody transcribed, so it can only agree with the statement.
//
// So the pool below applies the REAL corpus and nothing else: the bootstrap
// schema through db.RunMigrations, then every migrations/shared and
// migrations/tenant file through the same migrate.Runner the elitea-migrate
// command uses. No seed script, no hand-written CREATE TABLE.
//
// The fixture chooses three DIFFERENT numbers on purpose — project 5, caller 7,
// and 424242 for the user that is named in the payload and does not exist here.
// A statement that hardcoded 1, or that copied author_id into owner_id, or that
// adopted the id out of the export, satisfies the NOT NULL constraint and still
// fails these assertions.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/testpg"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/eliteacore"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/db"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/db/migrate"
	migrations "github.com/EliteaAI/elitea-platform/services/elitea-main/migrations"
)

const (
	importProjectID = 5 // the destination project — the value owner_id must hold
	importCallerID  = 7 // the authenticated principal — the value author_id must hold
	importAbsentUID = 424242
)

/* ── the import ────────────────────────────────────────────────────────── */

// TestToolkitImportStoresTheProjectAsOwnerAndTheCallerAsAuthor is the #504
// acceptance: import one toolkit into a project on a corpus-built database and
// read the ROW back, not the status code.
func TestToolkitImportStoresTheProjectAsOwnerAndTheCallerAsAuthor(t *testing.T) {
	pool := newImportCorpusPool(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	provisionTenantProject(t, pool, importProjectID)

	body := importToolkits(t, pool, []any{map[string]any{
		"entity":      "toolkits",
		"name":        "corpus-toolkit",
		"type":        "github",
		"description": "imported by the #504 regression test",
		"import_uuid": "tk-1",
		"settings":    map[string]any{"url": "https://example.invalid/repo"},
	}})

	toolkitID := onlyImportedToolkitID(t, body)

	var ownerID, authorID int
	var name, toolkitType string
	if err := pool.QueryRow(ctx, fmt.Sprintf(
		`SELECT owner_id, author_id, name, type FROM %s.elitea_tools WHERE id = $1`,
		tenantSchema(importProjectID)), toolkitID,
	).Scan(&ownerID, &authorID, &name, &toolkitType); err != nil {
		t.Fatalf("read the imported toolkit row back: %v", err)
	}

	if ownerID != importProjectID {
		t.Errorf("owner_id = %d, want %d — the project the toolkit was imported INTO", ownerID, importProjectID)
	}
	if authorID != importCallerID {
		t.Errorf("author_id = %d, want %d — the principal that performed the import", authorID, importCallerID)
	}
	if ownerID == authorID {
		t.Errorf("owner_id and author_id are both %d; the fixture chose distinct numbers precisely so a copy of one into the other would be visible", ownerID)
	}
	if name != "corpus-toolkit" || toolkitType != "github" {
		t.Errorf("the row is not the toolkit that was imported: name=%q type=%q", name, toolkitType)
	}
}

// TestToolkitImportIgnoresAUserThisDeploymentDoesNotHold answers the third
// candidate owner — "the original owner named in the export".
//
// The export writes no user for a toolkit at all (ExportImportGet emits id,
// name, type, import_uuid and settings), so the payload below names one that
// only another installation could have produced. It must be IGNORED: neither
// adopted, nor rejected into user 1, which is what the surrounding function's
// fmt.Sscanf fallback does on a parse failure.
//
// Nothing in either schema would catch an adopted id — author_id has no foreign
// key — so the check has to be here.
func TestToolkitImportIgnoresAUserThisDeploymentDoesNotHold(t *testing.T) {
	pool := newImportCorpusPool(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	provisionTenantProject(t, pool, importProjectID)

	// Measure the premise instead of assuming it: the corpus seeds exactly one
	// user (id 1, dev@elitea.ai) and the caller added by the harness.
	var exists bool
	if err := pool.QueryRow(ctx,
		`SELECT EXISTS(SELECT 1 FROM auth_core__user WHERE id = $1)`, importAbsentUID,
	).Scan(&exists); err != nil {
		t.Fatalf("check that user %d is absent: %v", importAbsentUID, err)
	}
	if exists {
		t.Fatalf("user %d exists on this deployment, so the test proves nothing", importAbsentUID)
	}

	body := importToolkits(t, pool, []any{map[string]any{
		"entity":      "toolkits",
		"name":        "foreign-owner-toolkit",
		"type":        "github",
		"import_uuid": "tk-foreign",
		// Every field an export from another installation could plausibly use
		// to name its original owner.
		"owner_id":  fmt.Sprintf("%d", importAbsentUID),
		"author_id": fmt.Sprintf("%d", importAbsentUID),
		"user_id":   fmt.Sprintf("%d", importAbsentUID),
		"settings":  map[string]any{},
	}})

	toolkitID := onlyImportedToolkitID(t, body)

	var ownerID, authorID int
	if err := pool.QueryRow(ctx, fmt.Sprintf(
		`SELECT owner_id, author_id FROM %s.elitea_tools WHERE id = $1`,
		tenantSchema(importProjectID)), toolkitID,
	).Scan(&ownerID, &authorID); err != nil {
		t.Fatalf("read the imported toolkit row back: %v", err)
	}

	if ownerID == importAbsentUID || authorID == importAbsentUID {
		t.Errorf("the import adopted a user this deployment does not hold: owner_id=%d author_id=%d", ownerID, authorID)
	}
	if authorID == 1 {
		t.Errorf("author_id = 1: the foreign id was turned into the fallback user rather than ignored")
	}
	if ownerID != importProjectID {
		t.Errorf("owner_id = %d, want %d — the destination project", ownerID, importProjectID)
	}
	if authorID != importCallerID {
		t.Errorf("author_id = %d, want %d — the principal that performed the import", authorID, importCallerID)
	}
}

// TestImportPathWritesEveryTableItNames is the general form of #504: a column
// that one schema declares NOT NULL and another does not breaks the statement
// that omits it, and only an execution against the corpus can tell.
//
// The payload exercises all four INSERTs the import path runs — applications,
// application_versions, elitea_tools and entity_tool_mapping — and every one of
// them is asserted as a ROW. Three of the four discard their error today
// (issue #505), so a status code proves nothing at all here.
func TestImportPathWritesEveryTableItNames(t *testing.T) {
	pool := newImportCorpusPool(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	provisionTenantProject(t, pool, importProjectID)
	schema := tenantSchema(importProjectID)

	body := importToolkits(t, pool, []any{
		map[string]any{
			"name":        "corpus-agent",
			"description": "imported by the #504 regression test",
			"import_uuid": "app-1",
			"versions": []any{map[string]any{
				"name":                  "latest",
				"agent_type":            "openai",
				"instructions":          "be useful",
				"welcome_message":       "hello",
				"llm_settings":          map[string]any{"model_name": "gpt-4o"},
				"conversation_starters": []any{"hi"},
				"meta":                  map[string]any{},
				"import_version_uuid":   "ver-1",
				// A map, because that is the only shape the import reads today
				// (handler.go: `toolRef["selected_tools"].(map[string]any)`)
				// while the column default and the export are an ARRAY. That
				// mismatch is issue #505 and is not measured here; the payload
				// uses the shape that reaches the database, so that this test
				// is about the NOT NULL columns and nothing else.
				"tools": []any{map[string]any{
					"import_uuid":    "tk-1",
					"selected_tools": map[string]any{"list_branches": true},
				}},
			}},
		},
		map[string]any{
			"entity":      "toolkits",
			"name":        "corpus-toolkit",
			"type":        "github",
			"import_uuid": "tk-1",
			"settings":    map[string]any{},
		},
	})

	if errs, ok := body["errors"].(map[string]any); ok {
		for _, key := range []string{"agents", "toolkits"} {
			if list, ok := errs[key].([]any); ok && len(list) > 0 {
				t.Fatalf("the import reported %s errors: %v", key, list)
			}
		}
	}

	// One row per table, and the mapping row carries the agent it belongs to —
	// entity_id is NOT NULL only after tenant migration 0125, so a statement
	// that omitted it would leave this count at zero.
	for _, probe := range []struct {
		what  string
		query string
	}{
		{"applications", `SELECT count(*) FROM %s.applications WHERE name = 'corpus-agent'`},
		{"application_versions", `SELECT count(*) FROM %s.application_versions WHERE name = 'latest'`},
		{"elitea_tools", `SELECT count(*) FROM %s.elitea_tools WHERE name = 'corpus-toolkit'`},
		{"entity_tool_mapping", `SELECT count(*) FROM %s.entity_tool_mapping WHERE entity_type = 'application'`},
	} {
		var rows int
		if err := pool.QueryRow(ctx, fmt.Sprintf(probe.query, schema)).Scan(&rows); err != nil {
			t.Fatalf("count %s: %v", probe.what, err)
		}
		if rows != 1 {
			t.Errorf("%s holds %d rows, want 1 — its INSERT did not reach the corpus schema", probe.what, rows)
		}
	}

	// And the toolkit the agent links to is owned by the destination project,
	// not by the caller, even on the mixed agent-and-toolkit payload.
	var ownerID, authorID int
	if err := pool.QueryRow(ctx, fmt.Sprintf(
		`SELECT owner_id, author_id FROM %s.elitea_tools WHERE name = 'corpus-toolkit'`, schema),
	).Scan(&ownerID, &authorID); err != nil {
		t.Fatalf("read the linked toolkit row: %v", err)
	}
	if ownerID != importProjectID || authorID != importCallerID {
		t.Errorf("linked toolkit owner_id=%d author_id=%d, want %d and %d", ownerID, authorID, importProjectID, importCallerID)
	}
}

/* ── harness ───────────────────────────────────────────────────────────── */

func tenantSchema(projectID int) string {
	return fmt.Sprintf("p_%d", projectID)
}

// importRouter mounts the import route the way internal/api/router.go does,
// minus the permission middleware, which is not what these tests measure.
func importRouter(handler *eliteacore.Handler) chi.Router {
	router := chi.NewRouter()
	router.Post("/elitea_core/import_wizard/prompt_lib/{projectID}", handler.ExportImportPost)
	return router
}

// importToolkits posts one import_wizard body as the caller and returns the
// decoded response. A non-2xx status fails here with the handler's own message,
// which is where the 23502 appears when owner_id is missing from the INSERT.
func importToolkits(t *testing.T, pool *pgxpool.Pool, entities []any) map[string]any {
	t.Helper()
	encoded, err := json.Marshal(entities)
	if err != nil {
		t.Fatalf("marshal import body: %v", err)
	}
	request := httptest.NewRequest(http.MethodPost,
		fmt.Sprintf("/elitea_core/import_wizard/prompt_lib/%d", importProjectID),
		bytes.NewReader(encoded))
	request.Header.Set("Content-Type", "application/json")
	request = request.WithContext(auth.ContextWithUser(request.Context(),
		auth.User{ID: fmt.Sprintf("%d", importCallerID)}))

	recorder := httptest.NewRecorder()
	importRouter(eliteacore.NewHandler(pool)).ServeHTTP(recorder, request)
	if recorder.Code < 200 || recorder.Code > 299 {
		t.Fatalf("import status = %d, want 2xx — body %s", recorder.Code, recorder.Body.String())
	}

	var body map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode %q: %v", recorder.Body.String(), err)
	}
	return body
}

// onlyImportedToolkitID pulls the single toolkit id out of an import response,
// and fails with the handler's error list when there is none — that list is
// what carries the SQLSTATE when the INSERT is refused.
func onlyImportedToolkitID(t *testing.T, body map[string]any) string {
	t.Helper()
	result, _ := body["result"].(map[string]any)
	toolkits, _ := result["toolkits"].([]any)
	if len(toolkits) != 1 {
		t.Fatalf("the import created %d toolkits, want 1 — response %v", len(toolkits), body)
	}
	entry, _ := toolkits[0].(map[string]any)
	id, _ := entry["id"].(string)
	if id == "" {
		t.Fatalf("the imported toolkit has no id: %v", entry)
	}
	return id
}

// provisionTenantProject creates one more project through the corpus's OWN
// tenant-provisioning function and applies the tenant history to it. Nothing
// here writes DDL of its own: the point of the test is the shape the corpus
// makes.
func provisionTenantProject(t *testing.T, pool *pgxpool.Pool, projectID int) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	if _, err := pool.Exec(ctx, `
INSERT INTO centry.project (id, name, owner_id, create_success)
VALUES ($1, 'import fixture project', 1, true) ON CONFLICT (id) DO NOTHING`, projectID); err != nil {
		t.Fatalf("create project %d: %v", projectID, err)
	}
	if _, err := pool.Exec(ctx, `SELECT create_tenant_schema($1)`, tenantSchema(projectID)); err != nil {
		t.Fatalf("create tenant schema for project %d: %v", projectID, err)
	}
	if err := migrate.New(pool, migrations.Files).ApplyTenant(ctx, int64(projectID)); err != nil {
		t.Fatalf("apply the tenant history to project %d: %v", projectID, err)
	}
	// The caller must be a principal this deployment really holds, so that the
	// "user we do not hold" test below is about the payload and not about the
	// caller.
	if _, err := pool.Exec(ctx, `
INSERT INTO auth_core__user (id, email, name)
VALUES ($1, 'importer@example.invalid', 'Import Caller') ON CONFLICT (id) DO NOTHING`, importCallerID); err != nil {
		t.Fatalf("create the calling principal: %v", err)
	}
}

// newImportCorpusPool builds a throwaway database holding ONLY what a deployed
// installation of this repository holds: the bootstrap schema plus the embedded
// shared and tenant histories, applied by the same runner elitea-migrate uses.
func newImportCorpusPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	const environment = "ELITEA_TEST_DATABASE_URL"
	databaseURL := testpg.DSN(os.Getenv(environment))
	if databaseURL == "" {
		t.Skipf("set %s to run the PostgreSQL service-integration test", environment)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
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

	databaseName := fmt.Sprintf("elitea_import_it_%d_%d", os.Getpid(), time.Now().UnixNano())
	quotedDatabase := pgx.Identifier{databaseName}.Sanitize()
	if _, err := adminPool.Exec(ctx, "CREATE DATABASE "+quotedDatabase); err != nil {
		adminPool.Close()
		t.Fatalf("create isolated integration database: %v", err)
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
		t.Fatalf("open isolated integration database: %v", err)
	}
	t.Cleanup(func() {
		pool.Close()
		dropCtx, dropCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer dropCancel()
		if _, err := adminPool.Exec(dropCtx, "DROP DATABASE "+quotedDatabase+" WITH (FORCE)"); err != nil {
			t.Errorf("drop isolated integration database: %v", err)
		}
		adminPool.Close()
	})

	// The bootstrap schema, then the ledgered corpus. Nothing is created here by
	// hand — that is the whole point of the file.
	if err := db.RunMigrations(ctx, pool); err != nil {
		t.Fatalf("apply the bootstrap schema: %v", err)
	}
	runner := migrate.New(pool, migrations.Files)
	if err := runner.ApplyShared(ctx); err != nil {
		t.Fatalf("apply shared migrations: %v", err)
	}
	if err := runner.ApplyTenant(ctx, 1); err != nil {
		t.Fatalf("apply tenant migrations to p_1: %v", err)
	}
	return pool
}
