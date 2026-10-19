package migrations_test

// shared/0170's grants against a clean database and its per-project override
// block (decision D4, pending confirmation): manage to project admins only,
// list (rule names) to admin, editor and viewer. The strings are read from the
// production constants, so a rename cannot leave this measuring a string
// nothing gates on.

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/httpactions"
)

const httpActionRulesMigration = "0170_http_action_rules_and_effects_v3.sql"

func TestCleanDatabaseGrantsHTTPActionRulePermissions(t *testing.T) {
	pool := newMigratedPool(t)
	if holders := defaultModeGrantHolders(t, pool, httpactions.PermissionManage); !slices.Equal(holders, []string{"admin"}) {
		t.Fatalf("default-mode holders of %s = %v, want [admin]", httpactions.PermissionManage, holders)
	}
	if holders := defaultModeGrantHolders(t, pool, httpactions.PermissionList); !slices.Equal(holders, defaultModeRoles) {
		t.Fatalf("default-mode holders of %s = %v, want %v", httpactions.PermissionList, holders, defaultModeRoles)
	}
}

// Through the real resolver on a Go-provisioned project (no per-project rows):
// an admin resolves both, an editor and a viewer resolve list only.
func TestProjectRolesResolveHTTPActionRulePermissions(t *testing.T) {
	pool := newMigratedPool(t)
	for i, role := range []string{"admin", "editor", "viewer"} {
		userID := 17001 + i
		if _, err := pool.Exec(context.Background(), `INSERT INTO public.auth_core__user (id, email, name) VALUES ($1, $2, $2)
ON CONFLICT (id) DO NOTHING`, userID, role+"-0170@test.local"); err != nil {
			t.Fatal(err)
		}
		seedRoleMembership(t, pool, 17001+i, role, userID)
		resolution := resolveDefaultModeFor(t, pool, strconv.Itoa(userID), "1")
		manage := slices.Contains(resolution.Permissions, httpactions.PermissionManage)
		list := slices.Contains(resolution.Permissions, httpactions.PermissionList)
		if manage != (role == "admin") || !list {
			t.Errorf("%s resolves manage=%v list=%v", role, manage, list)
		}
	}
}

// A project whose matrix carries override rows hides the central grants; the
// migration's second block grants the same split to those rows, by role name,
// idempotently, and to no other role.
func TestHTTPActionRuleGrantsReachProjectsWithSavedMatrices(t *testing.T) {
	pool := newMigratedPool(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	adminRole := seedRoleMembership(t, pool, 17011, "admin", 1)
	viewerRole := seedRoleMembership(t, pool, 17012, "viewer", 1)
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
	if resolution := resolveDefaultModeFor(t, pool, "1", "1"); slices.Contains(resolution.Permissions, httpactions.PermissionList) {
		t.Fatalf("the fixture does not suppress the central fallback: %v", resolution.Permissions)
	}

	body, err := os.ReadFile(filepath.Join("shared", httpActionRulesMigration))
	if err != nil {
		t.Fatal(err)
	}
	grants := grantBlock(t, string(body))
	for range 2 { // idempotent
		if _, err := pool.Exec(ctx, grants); err != nil {
			t.Fatalf("re-run 0170's grant block: %v", err)
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
		{adminRole, "admin", httpactions.PermissionManage, 1},
		{adminRole, "admin", httpactions.PermissionList, 1},
		{viewerRole, "viewer", httpactions.PermissionManage, 0},
		{viewerRole, "viewer", httpactions.PermissionList, 1},
		{auditorRole, "auditor", httpactions.PermissionManage, 0},
		{auditorRole, "auditor", httpactions.PermissionList, 0},
	} {
		if got := rows(want.role, want.permission); got != want.rows {
			t.Errorf("%s holds %d row(s) of %s, want %d", want.name, got, want.permission, want.rows)
		}
	}
	if resolution := resolveDefaultModeFor(t, pool, "1", "1"); !slices.Contains(resolution.Permissions, httpactions.PermissionList) {
		t.Fatalf("a viewer with a saved matrix does not resolve %s after 0170 (resolved %v)", httpactions.PermissionList, resolution.Permissions)
	}
}

// grantBlock is 0170's DO block. The rest of the file is DDL that ran once
// already; the grant block is the part written to be re-run.
func grantBlock(t *testing.T, migration string) string {
	t.Helper()
	start := strings.Index(migration, "DO $$")
	if start < 0 {
		t.Fatal("0170 has no DO block")
	}
	return migration[start:]
}
