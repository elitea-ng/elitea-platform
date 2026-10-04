package migrations_test

// shared/0136: the admin-only project settings grant (#6789) and the removal of
// permission strings no code checks (#6874).
//
// Every case runs the ledgered corpus on a fresh database, then runs the
// migration's own SQL again where a case needs rows that only a pylon-managed
// database carries. Re-running the real file is the point: a copy of the SQL
// here would measure the copy.
//
// Requires a PostgreSQL service (ELITEA_TEST_DATABASE_URL).

import (
	"os"
	"regexp"
	"slices"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	projectSettingsMigration = "shared/0136_project_settings_permission_and_dead_permissions.sql"
	projectSettingsEdit      = "models.project_settings.edit"
)

var deadPermissionArray = regexp.MustCompile(`(?s)dead_permission text\[\] := ARRAY\[(.*?)\];`)

// retiredBy0136 reads the dead_permission array from 0136, so a test that
// compares against the legacy matrix can leave out the strings 0136 removes
// on purpose. Reading the file keeps one list, not a copy.
func retiredBy0136(t *testing.T) map[string]bool {
	t.Helper()
	body, err := os.ReadFile(projectSettingsMigration)
	if err != nil {
		t.Fatalf("read %s: %v", projectSettingsMigration, err)
	}
	match := deadPermissionArray.FindSubmatch(body)
	if match == nil {
		t.Fatalf("%s carries no dead_permission array", projectSettingsMigration)
	}
	retired := map[string]bool{}
	for _, literal := range regexp.MustCompile(`'([^']+)'`).FindAllSubmatch(match[1], -1) {
		retired[string(literal[1])] = true
	}
	if len(retired) == 0 {
		t.Fatalf("%s retires no permission; the array shape changed", projectSettingsMigration)
	}
	return retired
}

func applyProjectSettingsMigration(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	body, err := os.ReadFile(projectSettingsMigration)
	if err != nil {
		t.Fatalf("read %s: %v", projectSettingsMigration, err)
	}
	ctx, cancel := testContext()
	defer cancel()
	if _, err := pool.Exec(ctx, string(body)); err != nil {
		t.Fatalf("apply %s: %v", projectSettingsMigration, err)
	}
}

func seedNamedUser(t *testing.T, pool *pgxpool.Pool, userID int, email string) {
	t.Helper()
	ctx, cancel := testContext()
	defer cancel()
	if _, err := pool.Exec(ctx, `
INSERT INTO public.auth_core__user (id, email, name) VALUES ($1, $2, $2)`, userID, email); err != nil {
		t.Fatalf("seed user %d: %v", userID, err)
	}
}

func centralDefaultGrants(t *testing.T, pool *pgxpool.Pool, roleName string) []string {
	t.Helper()
	ctx, cancel := testContext()
	defer cancel()
	rows, err := pool.Query(ctx, `
SELECT grant_row.permission
FROM public.auth_core__role_permission AS grant_row
JOIN public.auth_core__role AS role ON role.id = grant_row.role_id
WHERE role.mode = 'default' AND role.name = $1
ORDER BY 1`, roleName)
	if err != nil {
		t.Fatalf("read central grants of %s: %v", roleName, err)
	}
	defer rows.Close()
	var permissions []string
	for rows.Next() {
		var permission string
		if err := rows.Scan(&permission); err != nil {
			t.Fatalf("scan central grant: %v", err)
		}
		permissions = append(permissions, permission)
	}
	return permissions
}

// On a clean database the project admin resolves the settings permission and
// the editor and the viewer do not. The editor keeps the context permission.
func TestProjectSettingsEditIsTheProjectAdminsAlone(t *testing.T) {
	pool := newMigratedPool(t)

	for _, role := range []struct {
		name   string
		userID int
	}{
		{"admin", 13601},
		{"editor", 13602},
		{"viewer", 13603},
	} {
		seedNamedUser(t, pool, role.userID, role.name+"-0136@example.com")
		seedRoleMembership(t, pool, role.userID, role.name, role.userID)
	}
	for _, role := range []struct {
		userID  string
		name    string
		granted bool
	}{
		{"13601", "admin", true},
		{"13602", "editor", false},
		{"13603", "viewer", false},
	} {
		resolution := resolveDefaultModeFor(t, pool, role.userID, "1")
		if got := slices.Contains(resolution.Permissions, projectSettingsEdit); got != role.granted {
			t.Errorf("project %s resolves %s = %v, want %v", role.name, projectSettingsEdit, got, role.granted)
		}
	}
	editor := resolveDefaultModeFor(t, pool, "13602", "1")
	if !slices.Contains(editor.Permissions, "models.project_context.edit") {
		t.Errorf("the editor lost models.project_context.edit; #6789 restricts the settings, not the context")
	}
}

// The per-project delivery: a project whose admin role carries override rows
// would otherwise never see the new grant, because one override row discards
// the whole central set.
func TestProjectSettingsEditReachesAProjectWithItsOwnRows(t *testing.T) {
	pool := newMigratedPool(t)

	adminRole := seedProjectRole(t, pool, 13611, 2, "admin")
	seedOverrideRows(t, pool, 2, adminRole, "models.project_context.edit")
	editorRole := seedProjectRole(t, pool, 13612, 2, "editor")
	seedOverrideRows(t, pool, 2, editorRole, "models.project_context.edit")
	// A role with no rows of its own keeps the central fallback.
	viewerRole := seedProjectRole(t, pool, 13613, 2, "viewer")

	applyProjectSettingsMigration(t, pool)

	if rows := overrideRowsFor(t, pool, 2, adminRole); !slices.Contains(rows, projectSettingsEdit) {
		t.Errorf("project 2 admin override rows = %v, want %s delivered", rows, projectSettingsEdit)
	}
	if rows := overrideRowsFor(t, pool, 2, editorRole); slices.Contains(rows, projectSettingsEdit) {
		t.Errorf("project 2 editor override rows = %v; the editor must not get %s", rows, projectSettingsEdit)
	}
	if rows := overrideRowsFor(t, pool, 2, viewerRole); len(rows) != 0 {
		t.Errorf("project 2 viewer got override rows %v; a role without rows must keep the fallback", rows)
	}
}

// The dead strings go from the central matrix and from a per-project matrix
// that keeps a live row. A (project, role) pair whose ONLY rows are dead keeps
// them: deleting them would switch that role to the full central set.
func TestDeadPermissionStringsAreRemovedWithoutWideningARole(t *testing.T) {
	pool := newMigratedPool(t)
	ctx, cancel := testContext()
	defer cancel()

	// What pylon re-seeds on a shared database, and what the old Roles matrix
	// saved when an operator clicked a group toggle.
	if _, err := pool.Exec(ctx, `
INSERT INTO public.auth_core__role_permission (role_id, permission)
SELECT role.id, dead.permission
FROM public.auth_core__role AS role
CROSS JOIN (VALUES ('projects'), ('admin'), ('models.promptlib_shared.collections.list')) AS dead(permission)
WHERE role.mode = 'default' AND role.name = 'editor'
ON CONFLICT DO NOTHING`); err != nil {
		t.Fatalf("seed central dead rows: %v", err)
	}
	editorRole := seedProjectRole(t, pool, 13621, 3, "editor")
	seedOverrideRows(t, pool, 3, editorRole, "models.chat.messages.list", "configurations", "invites.platform")
	viewerRole := seedProjectRole(t, pool, 13622, 3, "viewer")
	seedOverrideRows(t, pool, 3, viewerRole, "configurations")

	applyProjectSettingsMigration(t, pool)
	// Idempotent: a second run changes nothing and does not fail.
	applyProjectSettingsMigration(t, pool)

	central := centralDefaultGrants(t, pool, "editor")
	for _, dead := range []string{"projects", "admin", "models.promptlib_shared.collections.list"} {
		if slices.Contains(central, dead) {
			t.Errorf("central editor grants still carry the dead string %q", dead)
		}
	}
	if !slices.Contains(central, "models.project_context.edit") {
		t.Errorf("the cleanup removed a live central editor grant: %v", central)
	}

	if rows := overrideRowsFor(t, pool, 3, editorRole); !slices.Equal(rows, []string{"models.chat.messages.list"}) {
		t.Errorf("project 3 editor rows = %v, want only the live row", rows)
	}
	if rows := overrideRowsFor(t, pool, 3, viewerRole); !slices.Equal(rows, []string{"configurations"}) {
		t.Errorf("project 3 viewer rows = %v; the only-dead pair must keep its row so the "+
			"viewer does not fall back to the central set", rows)
	}
}
