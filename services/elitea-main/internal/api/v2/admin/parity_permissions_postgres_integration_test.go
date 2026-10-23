package admin_test

// #255: the four central permissions the new admin routes are gated on have to
// be HELD by somebody, or the routes are 403-for-everyone — the exact outcome
// migrations/shared/0060 and 001_initial.sql's RBAC block exist to prevent.
//
// Two databases are checked because two are what deployments actually are:
//
//   - a FRESH one, bootstrapped from internal/infra/db/migrations/001_initial.sql;
//   - an EXISTING one that already ran 0060, which therefore skips it forever
//     (0060 returns early once any administration-mode role exists). Adding the
//     grants to 0060's VALUES list would have covered the first case and left
//     the second one refusing every caller, which is why 0061 exists.

import (
	"context"
	"fmt"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/testpg"
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// parityPermissions is the set issue 255's routes resolve, and the roles that
// must hold them. Transcribed from the legacy `check_api` declarations; see
// migrations/shared/0061_admin_parity_permissions.sql for the two
// transcription traps.
var parityPermissions = []string{
	"configuration.roles.user_project_permissions.edit",
	"configuration.roles.user_project_permissions.view",
	"modes.users",
	"runtime.admin.published_agents",
}

func heldPermissions(t *testing.T, pool *pgxpool.Pool, role string) []string {
	t.Helper()
	rows, err := pool.Query(context.Background(), `
SELECT grant_row.permission
FROM public.auth_core__role_permission grant_row
JOIN public.auth_core__role role ON role.id = grant_row.role_id
WHERE role.mode = 'administration' AND role.name = $1
  AND grant_row.permission = ANY($2::text[])
ORDER BY 1`, role, parityPermissions)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	held := []string{}
	for rows.Next() {
		var permission string
		if err := rows.Scan(&permission); err != nil {
			t.Fatal(err)
		}
		held = append(held, permission)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	sort.Strings(held)
	return held
}

func TestBootstrapSchemaGrantsTheParityPermissions(t *testing.T) {
	pool := newBootstrapPool(t)
	applySQLFile(t, pool, filepath.Join(
		"..", "..", "..", "infra", "db", "migrations", "001_initial.sql"))

	for _, role := range []string{"super_admin", "admin"} {
		if held := heldPermissions(t, pool, role); !equalStrings(held, parityPermissions) {
			t.Errorf("the administration %s role holds %v, want %v", role, held, parityPermissions)
		}
	}
	// editor and viewer are granted nothing by every declaration.
	for _, role := range []string{"editor", "viewer"} {
		if held := heldPermissions(t, pool, role); len(held) != 0 {
			t.Errorf("the administration %s role holds %v, want none", role, held)
		}
	}
}

func TestSharedMigrationGrantsTheParityPermissionsToAnExistingDeployment(t *testing.T) {
	pool := newBootstrapPool(t)
	ctx := context.Background()

	// A deployment that already ran 0060: administration roles exist, and the
	// four permissions do not.
	for _, statement := range []string{
		`CREATE TABLE public.auth_core__role (
			id serial PRIMARY KEY, name varchar(64) NOT NULL, mode varchar(64) NOT NULL,
			UNIQUE (name, mode))`,
		`CREATE TABLE public.auth_core__role_permission (
			id serial PRIMARY KEY,
			role_id integer NOT NULL REFERENCES public.auth_core__role(id) ON DELETE CASCADE,
			permission varchar(64), UNIQUE (role_id, permission))`,
		`INSERT INTO public.auth_core__role (name, mode) VALUES
			('super_admin', 'administration'), ('admin', 'administration'),
			('editor', 'administration'), ('viewer', 'administration')`,
	} {
		if _, err := pool.Exec(ctx, statement); err != nil {
			t.Fatalf("seed %q: %v", statement, err)
		}
	}
	for _, role := range []string{"super_admin", "admin"} {
		if held := heldPermissions(t, pool, role); len(held) != 0 {
			t.Fatalf("fixture already grants %v to %s", held, role)
		}
	}

	migration := filepath.Join("..", "..", "..", "..",
		"migrations", "shared", "0061_admin_parity_permissions.sql")
	applySQLFile(t, pool, migration)

	for _, role := range []string{"super_admin", "admin"} {
		if held := heldPermissions(t, pool, role); !equalStrings(held, parityPermissions) {
			t.Errorf("after 0061 the administration %s role holds %v, want %v",
				role, held, parityPermissions)
		}
	}
	for _, role := range []string{"editor", "viewer"} {
		if held := heldPermissions(t, pool, role); len(held) != 0 {
			t.Errorf("after 0061 the administration %s role holds %v, want none", role, held)
		}
	}

	// Re-applying is a no-op rather than a unique-violation: migrations are
	// re-run against restored snapshots more often than anyone plans for.
	applySQLFile(t, pool, migration)
	if held := heldPermissions(t, pool, "admin"); !equalStrings(held, parityPermissions) {
		t.Errorf("after a second 0061 the admin role holds %v, want %v", held, parityPermissions)
	}
}

func applySQLFile(t *testing.T, pool *pgxpool.Pool, path string) {
	t.Helper()
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	if _, err := pool.Exec(context.Background(), string(content)); err != nil {
		t.Fatalf("apply %s: %v", path, err)
	}
}

func newBootstrapPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	const environment = "ELITEA_TEST_DATABASE_URL"
	databaseURL := testpg.DSN(os.Getenv(environment))
	if databaseURL == "" {
		t.Skipf("set %s to run the PostgreSQL service-integration test", environment)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
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

	databaseName := fmt.Sprintf("elitea_admin_parity_it_%d_%d", os.Getpid(), time.Now().UnixNano())
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
		if _, dropErr := adminPool.Exec(context.Background(), "DROP DATABASE "+quotedDatabase+" WITH (FORCE)"); dropErr != nil {
			t.Errorf("drop database after pool open failure: %v", dropErr)
		}
		adminPool.Close()
		t.Fatalf("open isolated PostgreSQL integration database: %v", err)
	}
	t.Cleanup(func() {
		pool.Close()
		// 120 s, not the old 20 s to 30 s. This DROP queues behind the
		// CREATE DATABASE calls of every package that `go test ./...` runs at
		// the same time, so the wait is server load and not a hang. Two full
		// runs failed here with "drop isolated ... database: timeout" (#409).
		dropCtx, dropCancel := context.WithTimeout(context.Background(), 120*time.Second)
		defer dropCancel()
		if _, err := adminPool.Exec(dropCtx, "DROP DATABASE "+quotedDatabase+" WITH (FORCE)"); err != nil {
			t.Errorf("drop isolated PostgreSQL integration database: %v", err)
		}
		adminPool.Close()
	})
	return pool
}
