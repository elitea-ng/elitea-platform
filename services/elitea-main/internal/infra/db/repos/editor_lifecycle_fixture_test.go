package repos

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/db/migrate"
	bootstrapschema "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/db/migrations"
	platformmigrations "github.com/EliteaAI/elitea-platform/services/elitea-main/migrations"
	"github.com/jackc/pgx/v5/pgxpool"
)

const editorLifecycleFixtureDatabase = "elitea_it_editor_lifecycle_20261002"
const editorLifecycleFixtureURL = "ELITEA_EDITOR_TEST_DATABASE_URL"
const editorLifecycleFixtureRequired = "ELITEA_REQUIRE_EDITOR_POSTGRES_TEST"

// This fixture never uses the package-wide template or creates/deletes a database.
func editorLifecycleFixtureConfig(raw string) (*pgxpool.Config, error) {
	if raw == "" {
		return nil, errors.New("explicit disposable editor fixture required")
	}
	config, err := pgxpool.ParseConfig(raw)
	if err != nil {
		return nil, errors.New("invalid disposable editor fixture configuration")
	}
	conn := config.ConnConfig
	if conn.Database != editorLifecycleFixtureDatabase || conn.Host != "127.0.0.1" || conn.Port != 15444 || len(conn.Fallbacks) != 0 {
		return nil, errors.New("refuse a different database or fixture listener")
	}
	config.MaxConns = 4
	config.MinConns = 0
	conn.RuntimeParams["application_name"] = "elitea-editor-lifecycle-private-gate"
	conn.RuntimeParams["statement_timeout"] = "30000"
	return config, nil
}

func TestEditorLifecycleFixtureGuard(t *testing.T) {
	for _, raw := range []string{
		"", "postgres://127.0.0.1:15444/postgres?sslmode=disable",
		"postgres://127.0.0.1:15444/elitea?sslmode=disable",
		"postgres://localhost:15444/" + editorLifecycleFixtureDatabase + "?sslmode=disable",
		"postgres://127.0.0.1:5432/" + editorLifecycleFixtureDatabase + "?sslmode=disable",
		"postgres://127.0.0.1:15444/" + editorLifecycleFixtureDatabase + "?sslmode=prefer",
		"host=127.0.0.1,localhost port=15444 dbname=" + editorLifecycleFixtureDatabase,
	} {
		if _, err := editorLifecycleFixtureConfig(raw); err == nil {
			t.Errorf("unsafe configuration accepted")
		}
	}
	if _, err := editorLifecycleFixtureConfig("postgres://127.0.0.1:15444/" + editorLifecycleFixtureDatabase + "?sslmode=disable"); err != nil {
		t.Fatal(err)
	}
}

func editorLifecyclePostgresPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	raw := os.Getenv(editorLifecycleFixtureURL)
	if raw == "" {
		if required := os.Getenv(editorLifecycleFixtureRequired); required == "true" || required == "1" {
			t.Fatal("ELITEA_EDITOR_TEST_DATABASE_URL is required for editor PostgreSQL acceptance")
		}
		t.Skip("set ELITEA_EDITOR_TEST_DATABASE_URL to run the disposable editor PostgreSQL fixture")
	}
	for _, key := range []string{"ELITEA_TEST_DATABASE_URL", "ELITEA_TEST_USE_SERVICE_DATABASE_URL", "DATABASE_URL"} {
		if os.Getenv(key) != "" {
			t.Fatalf("refuse existing TestMain/service database configuration: %s", key)
		}
	}
	config, err := editorLifecycleFixtureConfig(raw)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 180*time.Second)
	defer cancel()
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		t.Fatal("open disposable editor fixture failed")
	}
	t.Cleanup(pool.Close)
	var name string
	var version int
	if err := pool.QueryRow(ctx, `SELECT current_database(),current_setting('server_version_num')::integer`).Scan(&name, &version); err != nil {
		t.Fatal("verify disposable editor fixture failed")
	}
	if name != editorLifecycleFixtureDatabase || version < 180000 || version >= 190000 {
		t.Fatal("server-side fixture identity mismatch")
	}

	// Bootstrap is the deployment-owned source. The package template seed only
	// models legacy ALTER targets, so it cannot exercise real application writes.
	sum := sha256.Sum256([]byte(bootstrapschema.Initial))
	fingerprint, err := postgresIntegrationSpec().Fingerprint()
	if err != nil {
		t.Fatal(err)
	}
	fingerprint += ":" + hex.EncodeToString(sum[:])
	var marker *string
	if err := pool.QueryRow(ctx, `SELECT to_regclass('public.elitea_editor_lifecycle_fixture')::text`).Scan(&marker); err != nil {
		t.Fatal(err)
	}
	if marker == nil {
		var objects int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM pg_catalog.pg_class c JOIN pg_catalog.pg_namespace n ON n.oid=c.relnamespace WHERE n.nspname NOT IN ('pg_catalog','information_schema') AND n.nspname NOT LIKE 'pg_toast%' AND c.relkind IN ('r','p','v','m','S')`).Scan(&objects); err != nil {
			t.Fatal(err)
		}
		if objects != 0 {
			t.Fatal("refuse a nonempty database without exact private fixture provenance")
		}
		if _, err := pool.Exec(ctx, `CREATE TABLE public.elitea_editor_lifecycle_fixture (id boolean PRIMARY KEY CHECK(id), fingerprint text NOT NULL)`); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx, `INSERT INTO public.elitea_editor_lifecycle_fixture VALUES (TRUE,$1)`, fingerprint); err != nil {
			t.Fatal(err)
		}
	} else {
		var stored string
		if err := pool.QueryRow(ctx, `SELECT fingerprint FROM public.elitea_editor_lifecycle_fixture WHERE id`).Scan(&stored); err != nil || stored != fingerprint {
			t.Fatal("refuse a different fixture migration/source fingerprint")
		}
	}
	if _, err := migrate.Bootstrap(ctx, pool, bootstrapschema.Initial); err != nil {
		t.Fatalf("normal bootstrap: %v", err)
	}
	runner := migrate.New(pool, platformmigrations.Files)
	if err := runner.ApplyShared(ctx); err != nil {
		t.Fatalf("normal shared migrations: %v", err)
	}
	if err := runner.ApplyTenant(ctx, 1); err != nil {
		t.Fatalf("normal tenant migrations: %v", err)
	}
	if err := runner.CheckHead(ctx, migrate.ScopeShared, "platform"); err != nil {
		t.Fatal(err)
	}
	if err := runner.CheckHead(ctx, migrate.ScopeTenant, "1"); err != nil {
		t.Fatal(err)
	}
	t.Logf("verified disposable fixture %s PostgreSQL %d; normal migration heads checked", name, version)
	return pool
}
