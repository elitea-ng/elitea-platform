package migrations_test

// shared/0172: the removal of `configuration.roles` and `projects.projects`,
// two legacy admin section names no code checks. The same rule as 0136: the
// central rows go, and a per-project row goes only where its (project, role)
// pair keeps a live row.
//
// Requires a PostgreSQL service (ELITEA_TEST_DATABASE_URL).

import (
	"os"
	"slices"
	"testing"
)

const legacyAdminSectionsMigration = "shared/0172_retire_legacy_admin_section_permissions.sql"

func TestLegacyAdminSectionNamesAreRemovedWithoutWideningARole(t *testing.T) {
	pool := newMigratedPool(t)
	ctx, cancel := testContext()
	defer cancel()

	// Rows carrying the two section names, which no migration seeds.
	if _, err := pool.Exec(ctx, `
INSERT INTO public.auth_core__role_permission (role_id, permission)
SELECT role.id, dead.permission
FROM public.auth_core__role AS role
CROSS JOIN (VALUES ('configuration.roles'), ('projects.projects')) AS dead(permission)
WHERE role.mode = 'administration' AND role.name = 'admin'
ON CONFLICT DO NOTHING`); err != nil {
		t.Fatalf("seed central dead rows: %v", err)
	}
	editorRole := seedProjectRole(t, pool, 17201, 3, "editor")
	seedOverrideRows(t, pool, 3, editorRole, "models.chat.messages.list", "projects.projects")
	viewerRole := seedProjectRole(t, pool, 17202, 3, "viewer")
	seedOverrideRows(t, pool, 3, viewerRole, "configuration.roles")

	// A direct user grant, which the resolver reads as a plain set.
	seedNamedUser(t, pool, 17204, "sections@example.com")
	if _, err := pool.Exec(ctx, `
WITH scope AS (INSERT INTO public.auth_core__scope (name) VALUES ('0172') RETURNING id)
INSERT INTO public.auth_core__user_permission (user_id, scope_id, permission)
SELECT 17204, scope.id, grant_row.permission
FROM scope CROSS JOIN (VALUES ('projects.projects'), ('models.chat.messages.list')) AS grant_row(permission)`); err != nil {
		t.Fatalf("seed user rows: %v", err)
	}

	body, err := os.ReadFile(legacyAdminSectionsMigration)
	if err != nil {
		t.Fatalf("read %s: %v", legacyAdminSectionsMigration, err)
	}
	// Idempotent: a second run changes nothing and does not fail.
	for range 2 {
		if _, err := pool.Exec(ctx, string(body)); err != nil {
			t.Fatalf("apply %s: %v", legacyAdminSectionsMigration, err)
		}
	}

	centralRows, err := pool.Query(ctx, `
SELECT grant_row.permission
FROM public.auth_core__role_permission AS grant_row
JOIN public.auth_core__role AS role ON role.id = grant_row.role_id
WHERE role.mode = 'administration' AND role.name = 'admin'
ORDER BY 1`)
	if err != nil {
		t.Fatalf("read central administration grants: %v", err)
	}
	defer centralRows.Close()
	var central []string
	for centralRows.Next() {
		var permission string
		if err := centralRows.Scan(&permission); err != nil {
			t.Fatalf("scan: %v", err)
		}
		central = append(central, permission)
	}
	if err := centralRows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	for _, dead := range []string{"configuration.roles", "projects.projects"} {
		if slices.Contains(central, dead) {
			t.Errorf("central administration admin grants still carry the dead string %q", dead)
		}
	}
	for _, live := range []string{"configuration.roles.permissions.view", "projects.projects.projects.view"} {
		if !slices.Contains(central, live) {
			t.Errorf("the cleanup removed the live grant %q: %v", live, central)
		}
	}

	if rows := overrideRowsFor(t, pool, 3, editorRole); !slices.Equal(rows, []string{"models.chat.messages.list"}) {
		t.Errorf("project 3 editor rows = %v, want only the live row", rows)
	}
	if rows := overrideRowsFor(t, pool, 3, viewerRole); !slices.Equal(rows, []string{"configuration.roles"}) {
		t.Errorf("project 3 viewer rows = %v; the only-dead pair must keep its row so the "+
			"viewer does not fall back to the central set", rows)
	}

	var userGrants []string
	if err := pool.QueryRow(ctx, `
SELECT coalesce(array_agg(permission ORDER BY permission), '{}')
FROM public.auth_core__user_permission WHERE user_id = 17204`).Scan(&userGrants); err != nil {
		t.Fatalf("read user grants: %v", err)
	}
	if !slices.Equal(userGrants, []string{"models.chat.messages.list"}) {
		t.Errorf("user 17204 grants = %v, want only the live row", userGrants)
	}
}
