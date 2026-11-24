package migrations_test

// shared/0271's grants against a clean database and its per-project override
// block: manage to project admins only, list (redacted rule names) to admin,
// editor and viewer. Strings come from the production constants.

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/v2/dbactions"
)

const dbActionRulesMigration = "0271_db_action_rules.sql"

func TestCleanDatabaseGrantsDBActionRulePermissions(t *testing.T) {
	pool := newMigratedPool(t)
	if holders := defaultModeGrantHolders(t, pool, dbactions.PermissionManage); !slices.Equal(holders, []string{"admin"}) {
		t.Fatalf("default-mode holders of %s = %v, want [admin]", dbactions.PermissionManage, holders)
	}
	if holders := defaultModeGrantHolders(t, pool, dbactions.PermissionList); !slices.Equal(holders, defaultModeRoles) {
		t.Fatalf("default-mode holders of %s = %v, want %v", dbactions.PermissionList, holders, defaultModeRoles)
	}
}

func TestProjectRolesResolveDBActionRulePermissions(t *testing.T) {
	pool := newMigratedPool(t)
	for i, role := range []string{"admin", "editor", "viewer"} {
		userID := 17201 + i
		if _, err := pool.Exec(context.Background(), `INSERT INTO public.auth_core__user (id, email, name) VALUES ($1, $2, $2)
ON CONFLICT (id) DO NOTHING`, userID, role+"-0271@test.local"); err != nil {
			t.Fatal(err)
		}
		seedRoleMembership(t, pool, 17201+i, role, userID)
		resolution := resolveDefaultModeFor(t, pool, strconv.Itoa(userID), "1")
		manage := slices.Contains(resolution.Permissions, dbactions.PermissionManage)
		list := slices.Contains(resolution.Permissions, dbactions.PermissionList)
		if manage != (role == "admin") || !list {
			t.Errorf("%s resolves manage=%v list=%v", role, manage, list)
		}
	}
}

func TestDBActionRuleGrantsReachProjectsWithSavedMatrices(t *testing.T) {
	pool := newMigratedPool(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	adminRole := seedRoleMembership(t, pool, 17211, "admin", 1)
	viewerRole := seedRoleMembership(t, pool, 17212, "viewer", 1)
	if _, err := pool.Exec(ctx, `
INSERT INTO public.auth_core__project_role (project_id, name) VALUES (1, 'auditor')
ON CONFLICT (project_id, name) DO NOTHING`); err != nil {
		t.Fatal(err)
	}
	var auditorRole int
	if err := pool.QueryRow(ctx, `SELECT id FROM public.auth_core__project_role WHERE project_id = 1 AND name = 'auditor'`).Scan(&auditorRole); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
INSERT INTO public.auth_core__project_role_permission (project_id, role_id, permission)
VALUES (1, $1, 'models.chat.messages.create'), (1, $2, 'models.chat.messages.create'), (1, $3, 'models.chat.messages.create')`,
		adminRole, viewerRole, auditorRole); err != nil {
		t.Fatal(err)
	}
	if resolution := resolveDefaultModeFor(t, pool, "1", "1"); slices.Contains(resolution.Permissions, dbactions.PermissionList) {
		t.Fatalf("the fixture does not suppress the central fallback: %v", resolution.Permissions)
	}

	body, err := os.ReadFile(filepath.Join("shared", dbActionRulesMigration))
	if err != nil {
		t.Fatal(err)
	}
	grants := grantBlock(t, string(body))
	for range 2 { // idempotent
		if _, err := pool.Exec(ctx, grants); err != nil {
			t.Fatalf("re-run 0271's grant block: %v", err)
		}
	}

	rows := func(roleID int, permission string) int {
		var count int
		if err := pool.QueryRow(ctx, `
SELECT count(*) FROM public.auth_core__project_role_permission
WHERE project_id = 1 AND role_id = $1 AND permission = $2`, roleID, permission).Scan(&count); err != nil {
			t.Fatal(err)
		}
		return count
	}
	for _, want := range []struct {
		role       int
		name       string
		permission string
		rows       int
	}{
		{adminRole, "admin", dbactions.PermissionManage, 1},
		{adminRole, "admin", dbactions.PermissionList, 1},
		{viewerRole, "viewer", dbactions.PermissionManage, 0},
		{viewerRole, "viewer", dbactions.PermissionList, 1},
		{auditorRole, "auditor", dbactions.PermissionManage, 0},
		{auditorRole, "auditor", dbactions.PermissionList, 0},
	} {
		if got := rows(want.role, want.permission); got != want.rows {
			t.Errorf("%s holds %d row(s) of %s, want %d", want.name, got, want.permission, want.rows)
		}
	}
	if resolution := resolveDefaultModeFor(t, pool, "1", "1"); !slices.Contains(resolution.Permissions, dbactions.PermissionList) {
		t.Fatalf("a viewer with a saved matrix does not resolve %s after 0271 (resolved %v)", dbactions.PermissionList, resolution.Permissions)
	}
}
