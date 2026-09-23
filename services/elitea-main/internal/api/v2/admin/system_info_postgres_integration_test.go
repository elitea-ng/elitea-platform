package admin_test

// Acceptance for the migration-head half of `GET /admin/system_info/prompt_lib`
// (#892). Unlike the internal unit tests in system_info_internal_test.go —
// which only ever see a nil pool — this asserts against a database that has
// actually run the REAL ledgered shared migration corpus, so the `migrations`
// component reports the version elitea-migrate itself would have recorded
// last, not a value this test invented.
//
// Same template technique the moderation and projectprovisioning integration
// suites use (dbtest.EnsureTemplate, built once in TestMain and copied per
// test): replaying the whole shared corpus per test would be the #409/#425
// slow-suite defect this package's other integration tests already avoid.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/admin"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/db/dbtest"
	platformmigrations "github.com/EliteaAI/elitea-platform/services/elitea-main/migrations"
)

const systemInfoDatabaseURLEnv = "ELITEA_TEST_DATABASE_URL"
const systemInfoBootstrapSchema = "../../../infra/db/migrations/001_initial.sql"

var systemInfoTemplate string

func TestMain(m *testing.M) {
	databaseURL := os.Getenv(systemInfoDatabaseURLEnv)
	if databaseURL == "" {
		os.Exit(m.Run())
	}

	bootstrap, err := os.ReadFile(systemInfoBootstrapSchema)
	if err != nil {
		fmt.Fprintf(os.Stderr, "read bootstrap schema: %v\n", err)
		os.Exit(1)
	}

	ctx, cancel := dbtest.BuildContext(context.Background())
	adminPool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		fmt.Fprintf(os.Stderr, "open admin pool: %v\n", err)
		cancel()
		os.Exit(1)
	}
	templateName, err := dbtest.EnsureTemplate(ctx, adminPool, dbtest.Spec{
		Files: platformmigrations.Files,
		Seed:  string(bootstrap),
	})
	adminPool.Close()
	cancel()
	if err != nil {
		fmt.Fprintf(os.Stderr, "build system_info template: %v\n", err)
		os.Exit(1)
	}
	systemInfoTemplate = templateName
	os.Exit(m.Run())
}

func newSystemInfoPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	databaseURL := os.Getenv(systemInfoDatabaseURLEnv)
	if databaseURL == "" {
		t.Skipf("set %s to run the PostgreSQL service-integration test", systemInfoDatabaseURLEnv)
	}
	if systemInfoTemplate == "" {
		t.Fatalf("TestMain did not build the system_info template")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	adminPool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		t.Fatalf("open admin pool: %v", err)
	}
	defer adminPool.Close()

	databaseName := fmt.Sprintf("elitea_system_info_%d_%d", os.Getpid(), time.Now().UnixNano())
	if err := dbtest.CreateFromTemplate(ctx, adminPool, systemInfoTemplate, databaseName); err != nil {
		t.Fatalf("create isolated database: %v", err)
	}

	config, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		t.Fatalf("parse %s: %v", systemInfoDatabaseURLEnv, err)
	}
	config.ConnConfig.Database = databaseName
	config.MaxConns = 4
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		t.Fatalf("open isolated pool: %v", err)
	}
	t.Cleanup(func() {
		pool.Close()
		dropCtx, dropCancel := context.WithTimeout(context.Background(), 120*time.Second)
		defer dropCancel()
		dropPool, dropErr := pgxpool.New(dropCtx, databaseURL)
		if dropErr != nil {
			return
		}
		defer dropPool.Close()
		_, _ = dropPool.Exec(dropCtx,
			"DROP DATABASE IF EXISTS "+pgx.Identifier{databaseName}.Sanitize()+" WITH (FORCE)")
	})
	return pool
}

// TestSystemInfoReportsTheRealMigrationHead is the acceptance guard: on a
// database that ran the actual ledgered shared corpus, `components` must
// carry a `migrations` entry whose version is the SAME value the ledger
// itself holds — read independently here with a plain SQL query, not through
// any helper the handler shares — plus the binary's own version, unaffected
// by whether a database is present.
func TestSystemInfoReportsTheRealMigrationHead(t *testing.T) {
	pool := newSystemInfoPool(t)

	var wantHead int64
	if err := pool.QueryRow(context.Background(),
		`SELECT MAX(version) FROM elitea_runtime.schema_migrations WHERE target_kind = 'shared'`,
	).Scan(&wantHead); err != nil {
		t.Fatalf("read the real migration head: %v", err)
	}
	if wantHead <= 0 {
		t.Fatalf("the ledgered shared corpus recorded no migrations at all — template build is broken")
	}

	recorder := httptest.NewRecorder()
	admin.NewHandler(pool).SystemInfo(recorder, httptest.NewRequest(http.MethodGet, "/admin/system_info/prompt_lib", nil))

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d (%s)", recorder.Code, http.StatusOK, recorder.Body.String())
	}
	var body struct {
		Components []struct {
			Name    string `json:"name"`
			Version string `json:"version"`
		} `json:"components"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode response: %v (%q)", err, recorder.Body.String())
	}

	var gotMigrations, gotMain string
	var sawMigrations, sawMain bool
	for _, component := range body.Components {
		switch component.Name {
		case "migrations":
			gotMigrations, sawMigrations = component.Version, true
		case "elitea-main":
			gotMain, sawMain = component.Version, true
		}
	}
	if !sawMain {
		t.Errorf("components carries no elitea-main entry: %v", body.Components)
	} else if gotMain == "" {
		t.Errorf("elitea-main component reports an empty version")
	}
	if !sawMigrations {
		t.Fatalf("components carries no migrations entry with a real database: %v", body.Components)
	}
	if want := fmt.Sprintf("%04d", wantHead); gotMigrations != want {
		t.Errorf("migrations version = %q, want %q (the ledger's real MAX(version))", gotMigrations, want)
	}
}
