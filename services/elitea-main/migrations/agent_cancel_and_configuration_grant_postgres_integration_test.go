package migrations_test

// The six permissions that agent cancel and the configuration surface require,
// measured against a CLEAN database (#359).
//
// # Why this file exists, and why no other test could catch this
//
// Each permission below gates a mounted production route:
//
//	internal/api/v2/agentexecution/cancel.go:17, CurrentAgentCancelPermission
//	    → DELETE /api/v2/elitea_core/task/prompt_lib/{projectID}/{responseMessageID}
//	      (production_router.go:162-164)
//
//	internal/api/v2/configurations/read.go:21-22
//	    → GET  /api/v2/configurations/configurations/{projectID}
//	    → GET  /api/v2/configurations/configuration/{projectID}/{configID}
//
//	internal/api/v2/configurations/mutation.go:26-28
//	    → POST, PUT and DELETE on the same two paths
//	      (production_router.go:138-148, wired at cmd/elitea-main/main.go:554,780)
//
// No migration granted them, and 001_initial.sql does not seed them. So on a
// clean database every route above answers 403 to every caller. The user cannot
// stop a running agent turn. The user cannot list, read, create, update or
// delete a configuration.
//
// The second consequence blocks chat. The chat model picker reads the
// configuration list, so a 403 there leaves the user no model to select. Chat
// stays broken on a clean deployment even after 0070 grants
// `models.chat.messages.create`.
//
// # Why the end-to-end suite could not see it
//
// apps/elitea-web/scripts/e2e-stack.sh grants permissions as PER-PROJECT
// auth_core__project_role_permission rows. Those rows suppress legacyrbac's
// central default-mode fallback completely, and supply the permissions
// directly. A pylon-backed database and a legacy dump hold that same shape. So
// only a clean Go bootstrap breaks, and no seeded suite could see it.
//
// So this file asserts against the corpus a real deployment applies, and against
// nothing else. newMigratedPool CREATEs an empty database, applies
// 001_initial.sql, then applies every shared and tenant migration through the
// real runner. No seed script runs here. Delete
// shared/0072_agent_cancel_and_configuration_permissions.sql and the first three
// tests fail; no other test does.
//
// # Both directions
//
// Only the pair of directions discriminates. A gate that refuses EVERY caller
// passes a positive-only suite, and so does a gate that admits every caller. So
// each permission is measured twice:
//
//	entitled caller passes → TestAnEditorPassesAllSixGatesOnACleanDatabase
//	unentitled caller refused → TestAViewerIsRefusedTheConfigurationWriteGates
//	                            (the three write strings, against a REAL role)
//	                          → TestACallerWithoutTheGrantsIsRefusedAtAllSixGates
//	                            (all six, against a project that suppresses the
//	                            fallback)
//
// Every assertion reads the grant ROWS or the resolver, never a status code, so
// a change to the HTTP surface cannot mask a lost grant.
//
// A non-member gains nothing from these grants. The central fallback joins
// THROUGH the caller's assigned project roles, so a non-member has no row to
// fall back from. That mechanism does not depend on which permission is granted,
// and runtime_authorizer_grant_postgres_integration_test.go already pins it.

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/agentexecution"
	configurationapi "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/configurations"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/legacyrbac"
)

// The permissions are read from the production constants, not copied, so a
// rename cannot leave this test measuring a string nothing uses.
//
// The role split is transcribed from testdata/postgres/legacy-rbac-matrix.json,
// `default` mode. `system` and `super_admin` are dropped, as everywhere else in
// this corpus: Go seeds neither role in the default mode.
const (
	agentCancelPermission         = agentexecution.CurrentAgentCancelPermission
	configurationListPermission   = configurationapi.CurrentConfigurationListPermission
	configurationGetPermission    = configurationapi.CurrentConfigurationGetPermission
	configurationCreatePermission = configurationapi.CurrentConfigurationCreatePermission
	configurationUpdatePermission = configurationapi.CurrentConfigurationUpdatePermission
	configurationDeletePermission = configurationapi.CurrentConfigurationDeletePermission
)

// The three strings the legacy matrix gives to admin, editor and viewer alike.
var readGatePermissions = []string{
	agentCancelPermission,
	configurationListPermission,
	configurationGetPermission,
}

// The three strings the legacy matrix withholds from the default-mode viewer.
var writeGatePermissions = []string{
	configurationCreatePermission,
	configurationUpdatePermission,
	configurationDeletePermission,
}

var writeGateHolders = []string{"admin", "editor"}

// A clean database grants each of the six to exactly the roles the legacy matrix
// records. This is the assertion #359 exists for.
func TestCleanDatabaseGrantsAgentCancelAndConfigurationPermissions(t *testing.T) {
	pool := newMigratedPool(t)

	for _, permission := range readGatePermissions {
		// The model catalogue read is also on the machine role's callback
		// surface (shared/0088). `system` is therefore an expected holder of
		// that one string, and of neither other.
		want := defaultModeRoles
		if permission == configurationListPermission {
			want = withMachineRole(defaultModeRoles...)
		}
		holders := defaultModeGrantHolders(t, pool, permission)
		if !slices.Equal(holders, want) {
			t.Errorf("default-mode holders of %s = %v, want %v: without the grant the route "+
				"answers 403 to every caller", permission, holders, want)
		}
	}
	for _, permission := range writeGatePermissions {
		holders := defaultModeGrantHolders(t, pool, permission)
		if !slices.Equal(holders, writeGateHolders) {
			t.Errorf("default-mode holders of %s = %v, want %v: the legacy matrix withholds "+
				"this string from the viewer", permission, holders, writeGateHolders)
		}
	}
}

// The entitled direction. An editor of a Go-provisioned project resolves all six
// permissions, so every gate admits them.
func TestAnEditorPassesAllSixGatesOnACleanDatabase(t *testing.T) {
	pool := newMigratedPool(t)
	seedRoleMembership(t, pool, 3591, "editor", 1)

	resolution := resolveDefaultModeFor(t, pool, "1", "1")

	for _, permission := range slices.Concat(readGatePermissions, writeGatePermissions) {
		if !slices.Contains(resolution.Permissions, permission) {
			t.Errorf("an editor does not resolve %s on a clean database; the route refuses the "+
				"caller (resolved %v)", permission, resolution.Permissions)
		}
	}
}

// The refused direction, against a REAL role rather than an absent one.
//
// A viewer holds the three read strings and none of the three write strings. So
// the read gates admit a viewer and the write gates refuse the same caller, in
// one resolution. A migration that granted the write strings too widely fails
// here, and a migration that granted nothing fails the read half.
func TestAViewerIsRefusedTheConfigurationWriteGates(t *testing.T) {
	pool := newMigratedPool(t)
	seedRoleMembership(t, pool, 3592, "viewer", 1)

	resolution := resolveDefaultModeFor(t, pool, "1", "1")

	for _, permission := range readGatePermissions {
		if !slices.Contains(resolution.Permissions, permission) {
			t.Errorf("a viewer does not resolve %s; the legacy matrix grants it to the "+
				"default-mode viewer (resolved %v)", permission, resolution.Permissions)
		}
	}
	for _, permission := range writeGatePermissions {
		if slices.Contains(resolution.Permissions, permission) {
			t.Errorf("a viewer resolves %s; the legacy matrix withholds it, so this grant "+
				"widens what a viewer may do", permission)
		}
	}
}

// The refused direction for all six at once, and the reason the positive test
// above means anything.
//
// A member of a project that carries its OWN per-project grant rows resolves
// only those rows. The central fallback is suppressed for that project, so the
// corpus grants do not reach it. That caller holds none of the six, and every
// gate must refuse them.
//
// This case also pins the migration's blast radius. This is the shape of every
// pylon-backed database, every legacy dump and the end-to-end stack, so the
// migration cannot change what their members may do.
func TestACallerWithoutTheGrantsIsRefusedAtAllSixGates(t *testing.T) {
	pool := newMigratedPool(t)
	roleID := seedRoleMembership(t, pool, 3593, "editor", 1)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if _, err := pool.Exec(ctx, `
INSERT INTO public.auth_core__project_role_permission (project_id, role_id, permission)
VALUES (1, $1, 'models.something.else')`, roleID); err != nil {
		t.Fatalf("seed the per-project grant: %v", err)
	}

	resolution := resolveDefaultModeFor(t, pool, "1", "1")
	if !slices.Equal(resolution.Permissions, []string{"models.something.else"}) {
		t.Fatalf("permissions = %v, want only the project's own grant; the central fallback must "+
			"not apply to a project that carries per-project rows", resolution.Permissions)
	}
	for _, permission := range slices.Concat(readGatePermissions, writeGatePermissions) {
		if slices.Contains(resolution.Permissions, permission) {
			t.Errorf("%s resolves for a caller nothing granted it; the gate admits an "+
				"unentitled caller", permission)
		}
	}
}

// Every one of the six routes resolves the DEFAULT mode, which is the mode
// 0072 grants in. A route that moved to another mode would read grants this
// migration never writes, and would answer 403 again. This test needs no
// database, so it runs on every machine.
func TestTheSixGatesResolveTheDefaultMode(t *testing.T) {
	t.Parallel()

	for name, mode := range map[string]string{
		"agent cancel":           agentexecution.CurrentAgentCancelMode,
		"configuration read":     configurationapi.CurrentConfigurationReadMode,
		"configuration mutation": configurationapi.CurrentConfigurationMutationMode,
	} {
		if mode != auth.PermissionModeDefault {
			t.Errorf("the %s route resolves the %q mode, but 0072 grants in %q",
				name, mode, auth.PermissionModeDefault)
		}
	}
}

/* ── harness ───────────────────────────────────────────────────────────── */

// resolveDefaultModeFor makes the exact call the six gates make. Each route
// resolves auth.PermissionModeDefault for the path's project.
func resolveDefaultModeFor(t *testing.T, pool *pgxpool.Pool, userID, projectID string) auth.PermissionResolution {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	resolution, err := legacyrbac.NewPostgresResolver(pool).ResolvePermissions(
		ctx,
		auth.User{ID: userID, UserID: userID},
		auth.PermissionModeDefault,
		projectID,
	)
	if err != nil {
		t.Fatalf("resolve the caller's default-mode permissions: %v", err)
	}
	return resolution
}

// seedRoleMembership gives a user a named project role in project 1.
//
// 001_initial.sql creates the dev user and the project but assigns no project
// role, and projectPermissions() reaches the central fallback only THROUGH an
// assigned project role. The role NAME is what the fallback joins on
// (legacyrbac/postgres.go:219-226), so the name selects which central grants
// apply.
//
// The shape below is the shape the project provisioner produces (#342):
// projectprovisioning/steps.go createProjectPermissions() writes one
// auth_core__project_role per central default-mode role name, and writes NO
// auth_core__project_role_permission row. It leaves the row out deliberately, so
// that the central fallback stays reachable and the corpus grants apply. So a
// member of a Go-provisioned project resolves exactly what this test measures.
// `roleID` is the id to mint the row WITH, and it is used only when project 1
// does not already carry that role. shared/0111 gives project 1 the same four
// roles every provisioned project has, so on a corpus-built database the row is
// already there and a second one violates the (project_id, name) uniqueness.
// The membership is then assigned against the id that EXISTS, which is what a
// member of a Go-provisioned project has.
func seedRoleMembership(t *testing.T, pool *pgxpool.Pool, roleID int, roleName string, userID int) int {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	if _, err := pool.Exec(ctx, `
INSERT INTO public.auth_core__project_role (id, project_id, name)
VALUES ($1, 1, $2)
ON CONFLICT (project_id, name) DO NOTHING`, roleID, roleName); err != nil {
		t.Fatalf("seed the %q project role: %v", roleName, err)
	}
	var assignedRoleID int
	if err := pool.QueryRow(ctx,
		`SELECT id FROM public.auth_core__project_role WHERE project_id = 1 AND name = $1`,
		roleName).Scan(&assignedRoleID); err != nil {
		t.Fatalf("resolve the %q project role: %v", roleName, err)
	}
	if _, err := pool.Exec(ctx, `
INSERT INTO public.auth_core__project_user_role (project_id, user_id, role_id)
VALUES (1, $1, $2)
ON CONFLICT DO NOTHING`, userID, assignedRoleID); err != nil {
		t.Fatalf("assign the %q project role: %v", roleName, err)
	}
	return assignedRoleID
}
