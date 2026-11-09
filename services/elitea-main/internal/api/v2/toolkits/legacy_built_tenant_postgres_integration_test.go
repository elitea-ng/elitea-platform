package toolkits

// Main's toolkit routes against a project schema the legacy runtime built and
// the production upgrade path then migrated.
//
// Start: p_1.elitea_tools has no owner_id, env_vars or uuid, and p_1 has no
// index_meta table (internal/infra/db/dbtest/legacyshape). The database is
// upgraded exactly as Main starts up: Bootstrap, shared history, tenant
// history for project 1 (tenant migration 0164 repairs the shape).
//
// Asserts, through the real handlers:
//   - the export of a legacy toolkit answers 200 with its name and env_vars;
//   - the index list answers an empty array (legacy index metadata is not
//     ported, the table starts empty);
//   - create stores the project as owner_id, and fork copies it.
//
// Runs when ELITEA_TEST_DATABASE_URL is set; skips otherwise.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/db/dbtest/legacyshape"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/db/migrate"
	bootstrapschema "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/db/migrations"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/testpg"
	platformmigrations "github.com/elitea-ng/elitea-platform/services/elitea-main/migrations"
)

var legacyBuiltTenantDBSeq atomic.Uint64

// newUpgradedLegacyBuiltPool builds the legacy shape in an empty database and
// upgrades it by the production path.
func newUpgradedLegacyBuiltPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	databaseURL := testpg.DSN(os.Getenv(postgresIntegrationDatabaseURL))
	if databaseURL == "" {
		t.Skipf("set %s to run the legacy-built tenant tests", postgresIntegrationDatabaseURL)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	adminPool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		t.Fatalf("open admin pool: %v", err)
	}
	defer adminPool.Close()
	name := fmt.Sprintf("elitea_legacytenant_tk_%d_%d_%d", os.Getpid(), time.Now().UnixNano(), legacyBuiltTenantDBSeq.Add(1))
	if _, err := adminPool.Exec(ctx, "CREATE DATABASE "+pgx.Identifier{name}.Sanitize()); err != nil {
		t.Fatalf("create database: %v", err)
	}
	config, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		t.Fatalf("parse %s: %v", postgresIntegrationDatabaseURL, err)
	}
	config.ConnConfig.Database = name
	config.MaxConns = 6
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		t.Fatalf("open pool: %v", err)
	}
	t.Cleanup(func() {
		pool.Close()
		dropCtx, dropCancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer dropCancel()
		dropPool, dropErr := pgxpool.New(dropCtx, databaseURL)
		if dropErr != nil {
			return
		}
		defer dropPool.Close()
		_, _ = dropPool.Exec(dropCtx, "DROP DATABASE IF EXISTS "+pgx.Identifier{name}.Sanitize()+" WITH (FORCE)")
	})

	if _, err := pool.Exec(ctx, bootstrapschema.Initial); err != nil {
		t.Fatalf("apply the legacy-era schema: %v", err)
	}
	if _, err := pool.Exec(ctx, legacyshape.BuiltSQL); err != nil {
		t.Fatalf("build the legacy-built shape: %v", err)
	}
	if _, err := migrate.Bootstrap(ctx, pool, bootstrapschema.Initial); err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	runner := migrate.New(pool, platformmigrations.Files)
	if err := runner.ApplyShared(ctx); err != nil {
		t.Fatalf("ApplyShared: %v", err)
	}
	if err := runner.ApplyTenant(ctx, 1); err != nil {
		t.Fatalf("ApplyTenant(1): %v", err)
	}
	return pool
}

func legacyBuiltToolkitRouter(pool *pgxpool.Pool) chi.Router {
	handler := NewHandler(pool)
	router := chi.NewRouter()
	router.Get("/toolkit_export/prompt_lib/{projectID}/{toolkitID}", handler.ExportToolkit)
	router.Get("/index_meta/prompt_lib/{projectID}/{toolkitID}", handler.IndexMeta)
	router.Post("/tools/prompt_lib/{projectID}", handler.Create)
	router.Post("/fork_toolkit/prompt_lib/{projectID}", handler.ForkToolkit)
	return router
}

func postLegacyBuiltJSON(t *testing.T, router chi.Router, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	encoded, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, path, strings.NewReader(string(encoded)))
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	return recorder
}

func TestAnUpgradedLegacyBuiltProjectExportsItsToolkits(t *testing.T) {
	pool := newUpgradedLegacyBuiltPool(t)
	router := legacyBuiltToolkitRouter(pool)

	recorder := doToolkitRequest(t, router, http.MethodGet, "/toolkit_export/prompt_lib/1/1")
	if recorder.Code != http.StatusOK {
		t.Fatalf("export of legacy toolkit 1 = %d %s; want 200", recorder.Code, recorder.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode export: %v", err)
	}
	if body["name"] != "legacy toolkit one" || body["type"] != "github" {
		t.Errorf("export = %#v; want the legacy toolkit's name and type", body)
	}
	envVars, present := body["env_vars"].(map[string]any)
	if !present || len(envVars) != 0 {
		t.Errorf("env_vars = %#v; want an empty object", body["env_vars"])
	}
}

func TestAnUpgradedLegacyBuiltProjectListsNoIndexesForALegacyToolkit(t *testing.T) {
	pool := newUpgradedLegacyBuiltPool(t)
	router := legacyBuiltToolkitRouter(pool)

	recorder := doToolkitRequest(t, router, http.MethodGet, "/index_meta/prompt_lib/1/1")
	if recorder.Code != http.StatusOK {
		t.Fatalf("index list = %d %s; want 200", recorder.Code, recorder.Body.String())
	}
	var rows []any
	if err := json.Unmarshal(recorder.Body.Bytes(), &rows); err != nil || rows == nil || len(rows) != 0 {
		t.Fatalf("index list body = %s (%v); want an empty array", recorder.Body.String(), err)
	}
}

func TestAnUpgradedLegacyBuiltProjectCreatesAndForksToolkitsOwnedByTheProject(t *testing.T) {
	pool := newUpgradedLegacyBuiltPool(t)
	router := legacyBuiltToolkitRouter(pool)
	ctx := context.Background()

	created := postLegacyBuiltJSON(t, router, "/tools/prompt_lib/1", map[string]any{
		"name": "created after the upgrade",
		"type": "custom",
	})
	if created.Code != http.StatusOK && created.Code != http.StatusCreated {
		t.Fatalf("create = %d %s; want success", created.Code, created.Body.String())
	}
	var owner int
	if err := pool.QueryRow(ctx,
		`SELECT owner_id FROM p_1.elitea_tools WHERE name = 'created after the upgrade'`).Scan(&owner); err != nil || owner != 1 {
		t.Fatalf("created toolkit owner_id = %d, %v; want project 1", owner, err)
	}

	forked := postLegacyBuiltJSON(t, router, "/fork_toolkit/prompt_lib/1", map[string]any{"source_id": "1"})
	if forked.Code != http.StatusOK {
		t.Fatalf("fork = %d %s; want 200", forked.Code, forked.Body.String())
	}
	var forkOwner int
	var forkEnv string
	if err := pool.QueryRow(ctx,
		`SELECT owner_id, env_vars::text FROM p_1.elitea_tools WHERE name = 'legacy toolkit one (copy)'`).Scan(&forkOwner, &forkEnv); err != nil {
		t.Fatalf("read the fork: %v", err)
	}
	if forkOwner != 1 || forkEnv != "{}" {
		t.Fatalf("fork owner_id = %d, env_vars = %s; want 1 and {}", forkOwner, forkEnv)
	}
}
