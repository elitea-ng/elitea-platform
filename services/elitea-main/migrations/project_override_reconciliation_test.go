package migrations_test

// shared/0090_project_override_reconciliation.sql, and the rule that keeps it
// true.
//
// # The defect
//
// legacyrbac's projectPermissions() reads the central default-mode grants under
// `WHERE NOT EXISTS (SELECT 1 FROM project_permissions)`
// (internal/infra/legacyrbac/postgres.go). Suppression is ALL OR NOTHING per
// caller: one auth_core__project_role_permission row on any project role the
// caller holds discards the entire central set for that caller in that project.
//
// Every permission migration in this corpus writes CENTRAL rows only, and the
// product's own admin console is what creates the override rows ("Apply to
// Projects", the public/support matrix, the personal/team matrix). So an
// operator presses one button, and every platform grant that ships afterwards
// silently fails to reach those projects. The route the grant was written for
// answers 403 "insufficient permissions" to every member, with nothing naming
// the cause.
//
// # Why a source-shape gate as well as a behaviour test
//
// 0089 reconciles the corpus as of 0089. Nothing in the resolver changes, so a
// migration added later reintroduces the same hole the moment it grants
// centrally and stops there. TestALaterGrantMigrationCarriesItsOwnOverrideBlock
// is the gate for that, and it needs no database, so it runs in the ordinary
// unit build.

import (
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// reconciliationMigration is the file under test.
const reconciliationMigration = "shared/0090_project_override_reconciliation.sql"

// reconciliationHead is the last migration 0089 itself reconciles. A shared
// migration numbered above this one must deliver its own grants.
const reconciliationHead = 89

/* ── the source-shape gate ─────────────────────────────────────────────── */

// A grant migration added after 0089 must write the project-override rows too.
//
// It cannot be done by a second reconciliation file: 0089 has been applied on
// real databases and migrations are checksum-immutable, so it cannot be
// re-run. The companion block belongs in the new migration, beside the central
// INSERT it mirrors.
func TestALaterGrantMigrationCarriesItsOwnOverrideBlock(t *testing.T) {
	t.Parallel()

	entries, err := os.ReadDir(filepath.Join("shared"))
	if err != nil {
		t.Fatalf("read migrations/shared: %v", err)
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".sql") {
			continue
		}
		number, err := strconv.Atoi(strings.SplitN(entry.Name(), "_", 2)[0])
		if err != nil || number <= reconciliationHead {
			continue
		}
		body, err := os.ReadFile(filepath.Join("shared", entry.Name()))
		if err != nil {
			t.Fatalf("read %s: %v", entry.Name(), err)
		}
		text := string(body)
		if !strings.Contains(text, "INSERT INTO public.auth_core__role_permission") ||
			!strings.Contains(text, "role.mode = 'default'") {
			continue
		}
		if strings.Contains(text, "auth_core__project_role_permission") {
			continue
		}
		t.Errorf("shared/%s grants a default-mode permission centrally and writes no "+
			"auth_core__project_role_permission row.\n"+
			"  Every project that carries its own permission rows discards the central set\n"+
			"  wholly (internal/infra/legacyrbac/postgres.go), so this grant reaches none of\n"+
			"  them and the route it was written for answers 403 to every member there.\n"+
			"  shared/%s reconciled the corpus up to %d; a later file has to deliver its own.",
			entry.Name(), reconciliationMigration, reconciliationHead)
	}
}

/* ── the behaviour ─────────────────────────────────────────────────────── */

// A central grant reaches a project that carries its own permission rows, and
// a pair some project already states for itself is left alone.
//
// The migration runs inside newMigratedPool, before this test can seed
// anything, so the file's own SQL is applied a second time here. It is
// idempotent by construction, and running the real statement is the point: a
// copy of it in this file would measure the copy.
func TestReconciliationDeliversCentralGrantsToAProjectWithItsOwnRows(t *testing.T) {
	pool := newMigratedPool(t)

	// One member of project 1, holding a project `admin` role that states one
	// permission of its own. That single row suppresses the whole central set.
	seedUser(t, pool, 890)
	adminRoleID := seedRoleMembership(t, pool, 8901, "admin", 890)
	seedOverrideRows(t, pool, 1, adminRoleID, "models.something.else")

	before := resolveDefaultModeFor(t, pool, "890", "1")
	if !slices.Equal(before.Permissions, []string{"models.something.else"}) {
		t.Fatalf("permissions before reconciliation = %v, want only the project's own grant; "+
			"this test cannot discriminate anything if the fallback is not suppressed here",
			before.Permissions)
	}

	// A SECOND project states `configuration.secrets.secret.create` for an
	// `editor` role. That makes the pair (editor, …create) one an operator can
	// have decided about, so 0089 must leave it alone everywhere.
	seedProjectRole(t, pool, 8902, 2, "editor")
	seedOverrideRows(t, pool, 2, 8902, "configuration.secrets.secret.create")
	// A THIRD project's `editor` states something unrelated. It must receive
	// the central editor grants that no project states, and NOT the one the
	// second project states.
	seedProjectRole(t, pool, 8903, 3, "editor")
	seedOverrideRows(t, pool, 3, 8903, "models.something.else")

	applyReconciliation(t, pool)

	after := resolveDefaultModeFor(t, pool, "890", "1")
	if !slices.Contains(after.Permissions, "models.something.else") {
		t.Errorf("reconciliation dropped the project's own grant: %v", after.Permissions)
	}
	for _, permission := range []string{
		"configuration.secrets.secret.create",
		"configuration.artifacts.artifacts.view",
	} {
		if !slices.Contains(after.Permissions, permission) {
			t.Errorf("the project admin still does not resolve %s after reconciliation. "+
				"Every platform grant stays out of reach of this project.", permission)
		}
	}

	third := overrideRowsFor(t, pool, 3, 8903)
	if slices.Contains(third, "configuration.secrets.secret.create") {
		t.Errorf("reconciliation wrote (editor, configuration.secrets.secret.create) into "+
			"project 3, but another project states that pair for itself. An operator can "+
			"have removed it on purpose, and this file must not reverse that: %v", third)
	}
	if !slices.Contains(third, "configuration.secrets.secret.delete") {
		t.Errorf("reconciliation did not deliver (editor, configuration.secrets.secret.delete) "+
			"to project 3, and no project states that pair, so nobody can have revoked it: %v",
			third)
	}

	// A role that carries NO rows of its own still falls back, so it must not
	// be handed a snapshot it never had.
	viewerRoleID := seedProjectRole(t, pool, 8904, 1, "viewer")
	applyReconciliation(t, pool)
	if rows := overrideRowsFor(t, pool, 1, viewerRoleID); len(rows) != 0 {
		t.Errorf("reconciliation wrote rows for a role that had none, which switches that "+
			"role from the central fallback to a frozen snapshot: %v", rows)
	}
}

/* ── harness ───────────────────────────────────────────────────────────── */

// applyReconciliation runs the migration file's own SQL.
func applyReconciliation(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	body, err := os.ReadFile(reconciliationMigration)
	if err != nil {
		t.Fatalf("read %s: %v", reconciliationMigration, err)
	}
	ctx, cancel := testContext()
	defer cancel()
	if _, err := pool.Exec(ctx, string(body)); err != nil {
		t.Fatalf("apply %s: %v", reconciliationMigration, err)
	}
}

func seedUser(t *testing.T, pool *pgxpool.Pool, userID int) {
	t.Helper()
	ctx, cancel := testContext()
	defer cancel()
	if _, err := pool.Exec(ctx, `
INSERT INTO public.auth_core__user (id, email, name)
VALUES ($1, 'reconciliation@example.com', 'Reconciliation')`, userID); err != nil {
		t.Fatalf("seed the member: %v", err)
	}
}

// seedProjectRole returns the id the role actually has.
//
// `roleID` is the id to mint it WITH, and it is used only when the project does
// not already carry that role. shared/0111 gives project 1 the four roles every
// provisioned project has, so `viewer` and `admin` are already there under ids
// this fixture did not choose.
func seedProjectRole(t *testing.T, pool *pgxpool.Pool, roleID, projectID int, name string) int {
	t.Helper()
	ctx, cancel := testContext()
	defer cancel()
	if _, err := pool.Exec(ctx, `
INSERT INTO public.auth_core__project_role (id, project_id, name) VALUES ($1, $2, $3)
ON CONFLICT (project_id, name) DO NOTHING`,
		roleID, projectID, name); err != nil {
		t.Fatalf("seed project role %d: %v", roleID, err)
	}
	var assigned int
	if err := pool.QueryRow(ctx,
		`SELECT id FROM public.auth_core__project_role WHERE project_id = $1 AND name = $2`,
		projectID, name).Scan(&assigned); err != nil {
		t.Fatalf("resolve project role %q of project %d: %v", name, projectID, err)
	}
	return assigned
}

func seedOverrideRows(t *testing.T, pool *pgxpool.Pool, projectID, roleID int, permissions ...string) {
	t.Helper()
	ctx, cancel := testContext()
	defer cancel()
	for _, permission := range permissions {
		if _, err := pool.Exec(ctx, `
INSERT INTO public.auth_core__project_role_permission (project_id, role_id, permission)
VALUES ($1, $2, $3)`, projectID, roleID, permission); err != nil {
			t.Fatalf("seed the per-project grant %q: %v", permission, err)
		}
	}
}

func overrideRowsFor(t *testing.T, pool *pgxpool.Pool, projectID, roleID int) []string {
	t.Helper()
	ctx, cancel := testContext()
	defer cancel()
	rows, err := pool.Query(ctx, `
SELECT permission
FROM public.auth_core__project_role_permission
WHERE project_id = $1 AND role_id = $2
ORDER BY permission`, projectID, roleID)
	if err != nil {
		t.Fatalf("read the per-project grants: %v", err)
	}
	defer rows.Close()
	permissions := []string{}
	for rows.Next() {
		var permission string
		if err := rows.Scan(&permission); err != nil {
			t.Fatalf("scan a per-project grant: %v", err)
		}
		permissions = append(permissions, permission)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate the per-project grants: %v", err)
	}
	return permissions
}
