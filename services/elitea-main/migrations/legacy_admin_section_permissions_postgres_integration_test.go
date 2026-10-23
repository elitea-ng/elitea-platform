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

	// What the legacy runtime's admin module registered for its Roles and
	// Projects sections, in a database that was later migrated.
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

	rows, err := pool.Query(ctx, `
SELECT grant_row.permission
FROM public.auth_core__role_permission AS grant_row
JOIN public.auth_core__role AS role ON role.id = grant_row.role_id
WHERE role.mode = 'administration' AND role.name = 'admin'
ORDER BY 1`)
	if err != nil {
		t.Fatalf("read central administration grants: %v", err)
	}
	var central []string
	for rows.Next() {
		var permission string
		if err := rows.Scan(&permission); err != nil {
			t.Fatalf("scan: %v", err)
		}
		central = append(central, permission)
	}
	if err := rows.Err(); err != nil {
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
}
