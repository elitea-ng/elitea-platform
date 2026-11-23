package migrations_test

// Shared 0260 (issue 2499, ADR-0059 R0.10a): elitea_auth.admin_role_grants is
// an append-only history, and the auth_core__project_user_role (user_id)
// index is added where the table exists and skipped where it does not.

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	dbschema "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/db/schema"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/testpg"
	migrations "github.com/elitea-ng/elitea-platform/services/elitea-main/migrations"
)

const adminRoleGrantsMigration = "shared/0260_admin_role_grants.sql"

func TestAdminRoleGrantsRefusesUpdateDeleteAndTruncate(t *testing.T) {
	pool := newMigratedPool(t)
	ctx := context.Background()

	var id int64
	if err := pool.QueryRow(ctx, `
INSERT INTO elitea_auth.admin_role_grants (user_id, role_name, role_mode, action, actor_user_id, source)
VALUES (1, 'admin', 'administration', 'granted', 1, 'console')
RETURNING id`).Scan(&id); err != nil {
		t.Fatalf("insert a history row: %v", err)
	}

	for name, statement := range map[string]string{
		"UPDATE":   `UPDATE elitea_auth.admin_role_grants SET action = 'revoked' WHERE id = $1`,
		"DELETE":   `DELETE FROM elitea_auth.admin_role_grants WHERE id = $1`,
		"TRUNCATE": `TRUNCATE elitea_auth.admin_role_grants`,
	} {
		var err error
		if strings.Contains(statement, "$1") {
			_, err = pool.Exec(ctx, statement, id)
		} else {
			_, err = pool.Exec(ctx, statement)
		}
		if err == nil || !strings.Contains(err.Error(), "append-only") {
			t.Fatalf("%s on the history was not refused by the trigger: %v", name, err)
		}
	}

	var action string
	if err := pool.QueryRow(ctx,
		`SELECT action FROM elitea_auth.admin_role_grants WHERE id = $1`, id).Scan(&action); err != nil {
		t.Fatalf("re-read the history row: %v", err)
	}
	if action != "granted" {
		t.Fatalf("the history row changed to %q", action)
	}

	// The constraints: an unknown source, and a console change with no actor.
	for _, statement := range []string{
		`INSERT INTO elitea_auth.admin_role_grants (user_id, role_name, role_mode, action, actor_user_id, source)
		 VALUES (1, 'admin', 'administration', 'granted', 1, 'hand')`,
		`INSERT INTO elitea_auth.admin_role_grants (user_id, role_name, role_mode, action, source)
		 VALUES (1, 'admin', 'administration', 'granted', 'console')`,
		`INSERT INTO elitea_auth.admin_role_grants (user_id, role_name, role_mode, action, actor_user_id, source)
		 VALUES (1, 'admin', 'administration', 'granted', 1, 'bootstrap')`,
		`INSERT INTO elitea_auth.admin_role_grants (user_id, role_name, role_mode, action, actor_user_id, source)
		 VALUES (1, 'admin', 'administration', 'changed', 1, 'api')`,
	} {
		if _, err := pool.Exec(ctx, statement); err == nil {
			t.Fatalf("the history accepted %q", statement)
		}
	}

	if !indexExists(t, pool, "public", "auth_core__project_user_role_user_idx") {
		t.Fatal("0260 did not add the auth_core__project_user_role (user_id) index")
	}
}

// On a database without the auth_core tables, the file creates the history
// and skips the index, and it does so twice (it is idempotent).
func TestAdminRoleGrantsAppliesWithoutAuthCore(t *testing.T) {
	pool := newEmptyDatabase(t)
	ctx := context.Background()
	body, err := fs.ReadFile(migrations.Files, adminRoleGrantsMigration)
	if err != nil {
		t.Fatalf("read %s: %v", adminRoleGrantsMigration, err)
	}
	for range 2 {
		if err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, string(body))
			return err
		}); err != nil {
			t.Fatalf("apply 0260 to a database without auth_core: %v", err)
		}
	}

	var table, membership *string
	if err := pool.QueryRow(ctx, `
SELECT to_regclass('elitea_auth.admin_role_grants')::text,
       to_regclass('public.auth_core__project_user_role')::text`).Scan(&table, &membership); err != nil {
		t.Fatalf("to_regclass: %v", err)
	}
	if table == nil {
		t.Fatal("0260 created no history table on a database without auth_core")
	}
	if membership != nil {
		t.Fatal("the fixture is not a database without auth_core")
	}
	if indexExists(t, pool, "public", "auth_core__project_user_role_user_idx") {
		t.Fatal("0260 created the membership index on a database without the table")
	}
	if _, err := pool.Exec(ctx, `
INSERT INTO elitea_auth.admin_role_grants (user_id, role_name, role_mode, action, source)
VALUES (1, 'super_admin', 'administration', 'granted', 'bootstrap')`); err != nil {
		t.Fatalf("the history refused a bootstrap row: %v", err)
	}
	if _, err := pool.Exec(ctx, `DELETE FROM elitea_auth.admin_role_grants`); err == nil {
		t.Fatal("DELETE was not refused on a database without auth_core")
	}
}

// The baseline projection (internal/db/schema/auth_core_baseline.sql) copies
// the table so fixtures built on it can run the role writes. The copy must be
// the same table, or those fixtures test a different shape.
func TestAdminRoleGrantsProjectionMatchesTheMigration(t *testing.T) {
	body, err := fs.ReadFile(migrations.Files, adminRoleGrantsMigration)
	if err != nil {
		t.Fatalf("read %s: %v", adminRoleGrantsMigration, err)
	}
	migration := adminRoleGrantsTable(t, string(body))
	projection := adminRoleGrantsTable(t, dbschema.AuthCoreBaselineSQLCProjection)
	if migration != projection {
		t.Fatalf("auth_core_baseline.sql's admin_role_grants differs from %s:\nmigration:  %s\nprojection: %s",
			adminRoleGrantsMigration, migration, projection)
	}
}

// adminRoleGrantsTable is the CREATE TABLE statement for the history, with
// comments dropped and whitespace collapsed.
func adminRoleGrantsTable(t *testing.T, sql string) string {
	t.Helper()
	const head = "CREATE TABLE IF NOT EXISTS elitea_auth.admin_role_grants ("
	start := strings.Index(sql, head)
	if start < 0 {
		t.Fatalf("no %q in the file", head)
	}
	end := strings.Index(sql[start:], "\n);")
	if end < 0 {
		t.Fatal("the CREATE TABLE has no end")
	}
	var kept []string
	for line := range strings.SplitSeq(sql[start:start+end+3], "\n") {
		if code, _, _ := strings.Cut(line, "--"); strings.TrimSpace(code) != "" {
			kept = append(kept, strings.Join(strings.Fields(code), " "))
		}
	}
	return strings.Join(kept, " ")
}

func indexExists(t *testing.T, pool *pgxpool.Pool, schema, name string) bool {
	t.Helper()
	var present bool
	if err := pool.QueryRow(context.Background(), `
SELECT EXISTS (SELECT 1 FROM pg_indexes WHERE schemaname = $1 AND indexname = $2)`,
		schema, name).Scan(&present); err != nil {
		t.Fatalf("read pg_indexes: %v", err)
	}
	return present
}

// newEmptyDatabase is an isolated database with nothing in it: no bootstrap
// schema, no corpus.
func newEmptyDatabase(t *testing.T) *pgxpool.Pool {
	t.Helper()
	databaseURL := testpg.DSN(os.Getenv(databaseURLEnv))
	if databaseURL == "" {
		t.Skipf("set %s to run the migration-corpus integration test", databaseURLEnv)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	adminPool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		t.Fatalf("open admin pool: %v", err)
	}
	defer adminPool.Close()
	databaseName := fmt.Sprintf("elitea_grants_%d_%d", os.Getpid(), time.Now().UnixNano())
	if _, err := adminPool.Exec(ctx, "CREATE DATABASE "+pgx.Identifier{databaseName}.Sanitize()); err != nil {
		t.Fatalf("create isolated database: %v", err)
	}
	config, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	config.ConnConfig.Database = databaseName
	config.MaxConns = 2
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
