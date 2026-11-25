package migrations_test

// Shared 0273 (issue 2532, ADR-0059 R3.2): the default of the console's
// six-area navigation switch. A new install resolves ON, the code default; a
// database that already holds an administration role assignment (an upgrade)
// resolves OFF for one release. Both are read through the production reader
// the capability document uses (internal/api/v2/adminaccess PlatformFlags).

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	console "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/v2/adminaccess"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/db/migrate"
	migrations "github.com/elitea-ng/elitea-platform/services/elitea-main/migrations"
)

const (
	consoleAreasFile      = "0273_admin_console_areas_default.sql"
	consoleAreasMigration = "shared/" + consoleAreasFile
)

func consoleAreas(t *testing.T, pool *pgxpool.Pool) bool {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	on, err := console.NewPlatformFlags(pool).ConsoleAreas(ctx)
	if err != nil {
		t.Fatalf("read admin_console_areas: %v", err)
	}
	return on
}

func consoleAreasRows(t *testing.T, pool *pgxpool.Pool) int {
	t.Helper()
	var n int
	queryRow(t, pool, `SELECT count(*) FROM centry.platform_config
		WHERE section = 'admin_console' AND key = 'admin_console_areas'`, &n)
	return n
}

// newPoolBefore0273 is the bootstrap schema and the corpus up to 0272: a new
// install one release back, with no administration role assignment.
func newPoolBefore0273(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pool := newEmptyDatabase(t)
	applyBootstrapSchema(t, pool)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if err := migrate.New(pool, withoutFile{FS: migrations.Files, hidden: consoleAreasFile}).ApplyShared(ctx); err != nil {
		t.Fatalf("apply the shared corpus without %s: %v", consoleAreasFile, err)
	}
	return pool
}

// assignAdministrator gives the bootstrap account the administration `admin`
// role, as the first sign-in of a deployment's administrator does.
func assignAdministrator(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	execSQL(t, pool, `INSERT INTO public.auth_core__user_role (user_id, role_id)
		SELECT 1, id FROM public.auth_core__role WHERE mode = 'administration' AND name = 'admin'
		ON CONFLICT (user_id, role_id) DO NOTHING`)
}

func TestConsoleAreasAreOnForAFreshDatabase(t *testing.T) {
	pool := newMigratedPool(t)
	if !consoleAreas(t, pool) {
		t.Fatal("a fresh database resolves admin_console_areas off, want the code default on")
	}
	if n := consoleAreasRows(t, pool); n != 0 {
		t.Fatalf("a fresh database holds %d admin_console_areas rows, want none (the code default)", n)
	}
}

func TestConsoleAreasAreOffForADatabaseWithAnAdministrator(t *testing.T) {
	pool := newPoolBefore0273(t)
	assignAdministrator(t, pool)
	if !consoleAreas(t, pool) {
		t.Fatal("premise: before 0273 the switch is the code default, on")
	}
	execMigrationFile(t, pool, consoleAreasMigration)
	if consoleAreas(t, pool) {
		t.Fatal("an upgraded database resolves admin_console_areas on, want off for one release")
	}
	// Applying it again writes nothing.
	execMigrationFile(t, pool, consoleAreasMigration)
	if n := consoleAreasRows(t, pool); n != 1 {
		t.Fatalf("after two applications: %d rows, want 1", n)
	}
}

// A project role is not an administration role: a database whose people hold
// only default-mode roles is still a new console.
func TestConsoleAreasIgnoreDefaultModeAssignments(t *testing.T) {
	pool := newPoolBefore0273(t)
	execSQL(t, pool, `INSERT INTO public.auth_core__user_role (user_id, role_id)
		SELECT 1, id FROM public.auth_core__role WHERE mode = 'default' AND name = 'admin'
		ON CONFLICT (user_id, role_id) DO NOTHING`)
	execMigrationFile(t, pool, consoleAreasMigration)
	if !consoleAreas(t, pool) {
		t.Fatal("a default-mode assignment turned the switch off")
	}
}

// A value the operator stored is theirs.
func TestConsoleAreasKeepAStoredValue(t *testing.T) {
	pool := newPoolBefore0273(t)
	assignAdministrator(t, pool)
	execSQL(t, pool, `INSERT INTO centry.platform_config (section, key, value)
		VALUES ('admin_console', 'admin_console_areas', 'true'::jsonb)`)
	execMigrationFile(t, pool, consoleAreasMigration)
	if !consoleAreas(t, pool) {
		t.Fatal("0273 overwrote the operator's stored on")
	}
}

func TestConsoleAreasIsANoOpWithoutTheTables(t *testing.T) {
	pool := newEmptyDatabase(t)
	execMigrationFile(t, pool, consoleAreasMigration)
	var present bool
	queryRow(t, pool, `SELECT to_regclass('centry.platform_config') IS NOT NULL`, &present)
	if present {
		t.Fatal("0273 created centry.platform_config on an empty database")
	}
}
