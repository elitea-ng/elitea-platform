package admin_test

// The role rules ADR-0059 R0.8a adds to the #2555 fix (issue 2497, decision 3
// "No escalation, on every write path"):
//
//   - not yourself: a caller changes no administration role of its own, on
//     either assignment route and in either direction (403
//     role_self_assignment), except a standing global administrator;
//   - seeded only: a permission the registry marks seeded_only is added to no
//     role through the API (403 role_permission_reserved), a global
//     administrator included;
//   - the subset rule and the own-role lock of #2555, for a custom role that
//     holds configuration.roles.permissions.edit;
//   - `break_glass` is not a role name (400).
//
// Every caller is resolved by the real legacyrbac resolver from the fixture's
// rows (newEscalationEnvironment), and each refusal is followed by a SQL read
// that proves nothing moved.

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"testing"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/adminaccess"
)

const (
	usersAction = "/u/admin/auth_users/administration"
	modesAssign = "/o/admin/modes/administration"
)

func modesRemove(userID int, role string) string {
	return fmt.Sprintf("/o/admin/modes/administration?id=%d:administration:%s", userID, role)
}

// giveRole adds an administration role to a user beside the ones it holds.
func giveRole(t *testing.T, env escalationEnvironment, email, role string) {
	t.Helper()
	if _, err := env.pool.Exec(context.Background(), `
INSERT INTO public.auth_core__user_role (user_id, role_id)
SELECT $1, id FROM public.auth_core__role WHERE mode = 'administration' AND name = $2`,
		env.ids[email], role); err != nil {
		t.Fatalf("give %s the %s role: %v", email, role, err)
	}
}

func expectRoles(t *testing.T, env escalationEnvironment, email string, want ...string) {
	t.Helper()
	got := adminRoleNamesSQL(t, env.pool, env.ids[email])
	slices.Sort(got)
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Fatalf("%s holds %v, want %v", email, got, want)
	}
}

/* ── not yourself ──────────────────────────────────────────────────────── */

// The Users page route, giving: the admin holds every permission of editor,
// so only the not-yourself rule refuses it.
func TestSelfAssignmentIsRefusedOnTheUsersRoute(t *testing.T) {
	env := newEscalationEnvironment(t)
	expectRefusal(t, adminDo(t, env.as(escalationAdmin), http.MethodPost, usersAction,
		map[string]any{"action": "set_admin_role", "user_id": env.ids[escalationAdmin], "role_name": "editor"}),
		adminaccess.CodeSelfAssignment)
	expectRoles(t, env, escalationAdmin, "admin")
}

// The Users page route, taking: a null role removes every administration role.
func TestSelfRemovalIsRefusedOnTheUsersRoute(t *testing.T) {
	env := newEscalationEnvironment(t)
	expectRefusal(t, adminDo(t, env.as(escalationAdmin), http.MethodPost, usersAction,
		map[string]any{"action": "set_admin_role", "user_id": env.ids[escalationAdmin], "role_name": nil}),
		adminaccess.CodeSelfAssignment)
	expectRoles(t, env, escalationAdmin, "admin")
}

// POST /admin/modes/administration: empty_role holds nothing, so the subset
// rule would let it through; the not-yourself rule refuses it.
func TestSelfAssignmentIsRefusedOnTheModesRoute(t *testing.T) {
	env := newEscalationEnvironment(t)
	expectRefusal(t, adminDo(t, env.as(escalationModes), http.MethodPost, modesAssign,
		map[string]any{"user_id": env.ids[escalationModes], "mode": "administration", "role": "empty_role"}),
		adminaccess.CodeSelfAssignment)
	expectRoles(t, env, escalationModes, "modes_operator")
}

// DELETE /admin/modes/administration: the caller holds every permission of
// its own role, so only the not-yourself rule refuses it.
func TestSelfRemovalIsRefusedOnTheModesRoute(t *testing.T) {
	env := newEscalationEnvironment(t)
	expectRefusal(t, adminDo(t, env.as(escalationModes), http.MethodDelete,
		modesRemove(env.ids[escalationModes], "modes_operator"), nil),
		adminaccess.CodeSelfAssignment)
	expectRoles(t, env, escalationModes, "modes_operator")
}

// A write that leaves the caller's own roles as they are changes nothing, so
// the rule does not refuse it: re-saving the own role, removing a role the
// caller does not hold (404, as before).
func TestASelfWriteThatChangesNothingIsNotRefused(t *testing.T) {
	env := newEscalationEnvironment(t)
	expectStatus(t, adminDo(t, env.as(escalationAdmin), http.MethodPost, usersAction,
		map[string]any{"action": "set_admin_role", "user_id": env.ids[escalationAdmin], "role_name": "admin"}),
		http.StatusOK)
	expectRoles(t, env, escalationAdmin, "admin")
	expectStatus(t, adminDo(t, env.as(escalationModes), http.MethodDelete,
		modesRemove(env.ids[escalationModes], "empty_role"), nil), http.StatusNotFound)
	expectRoles(t, env, escalationModes, "modes_operator")
}

/* ── the global administrator's exemption ─────────────────────────────── */

func TestASuperAdminAssignsARoleToItself(t *testing.T) {
	env := newEscalationEnvironment(t)
	expectStatus(t, adminDo(t, env.as(escalationSuper), http.MethodPost, modesAssign,
		map[string]any{"user_id": env.ids[escalationSuper], "mode": "administration", "role": "empty_role"}),
		http.StatusOK)
	// The route adds a role (ADR-0059 R0.8b): super_admin stays beside it.
	expectRoles(t, env, escalationSuper, "empty_role", "super_admin")
}

func TestASuperAdminRemovesARoleFromItself(t *testing.T) {
	env := newEscalationEnvironment(t)
	giveRole(t, env, escalationSuper, "empty_role")
	expectStatus(t, adminDo(t, env.as(escalationSuper), http.MethodDelete,
		modesRemove(env.ids[escalationSuper], "empty_role"), nil), http.StatusOK)
	expectRoles(t, env, escalationSuper, "super_admin")
}

func TestASuperAdminEditsACustomRoleItHolds(t *testing.T) {
	env := newEscalationEnvironment(t)
	giveRole(t, env, escalationSuper, "matrix_editor")
	expectStatus(t, saveCell(t, env.as(escalationSuper), centralMatrix, "models.alpha.view", "matrix_editor", true),
		http.StatusOK)
	if !contains(grantsSQL(t, env.pool, "administration", "matrix_editor"), "models.alpha.view") {
		t.Fatal("the grant was not written")
	}
}

/* ── seeded only binds the global administrator too ───────────────────── */

func expectSeededOnlyRefused(t *testing.T, permission string) {
	t.Helper()
	if !adminaccess.SeededOnly(permission) {
		t.Fatalf("premise: %s is not seeded_only in the registry", permission)
	}
	env := newEscalationEnvironment(t)
	// What the seeding migrations do: the name is granted to super_admin, so
	// the matrix shows its row (a seeded_only name is in the catalogue only
	// where a migration granted it).
	if _, err := env.pool.Exec(context.Background(), `
INSERT INTO public.auth_core__role_permission (role_id, permission)
SELECT id, $1 FROM public.auth_core__role WHERE mode = 'administration' AND name = 'super_admin'
ON CONFLICT DO NOTHING`, permission); err != nil {
		t.Fatalf("seed %s on super_admin: %v", permission, err)
	}
	expectRefusal(t, saveCell(t, env.as(escalationSuper), centralMatrix, permission, "empty_role", true),
		adminaccess.CodeReservedPermission)
	if contains(grantsSQL(t, env.pool, "administration", "empty_role"), permission) {
		t.Fatalf("empty_role holds %s after the refusal", permission)
	}
}

func TestASuperAdminCannotAddTheSuperAdminPermissionToACustomRole(t *testing.T) {
	expectSeededOnlyRefused(t, adminaccess.AdminAuthUsersSuperAdmin)
}

func TestASuperAdminCannotAddIdentityTrustToACustomRole(t *testing.T) {
	expectSeededOnlyRefused(t, adminaccess.IdentitySSOTrustManage)
}

func TestASuperAdminCannotAddBreakGlassReviewToACustomRole(t *testing.T) {
	expectSeededOnlyRefused(t, adminaccess.AccessBreakglassReview)
}

// The project-plane writes refuse the seeded_only names too: the personal
// and team project matrix and the central `default` template, for a global
// administrator.
func TestSeededOnlyNamesAreRefusedOnTheProjectPlaneWrites(t *testing.T) {
	env := newEscalationEnvironment(t)
	router := env.as(escalationSuper)
	for _, permission := range []string{adminaccess.IdentitySSOTrustManage, adminaccess.AccessBreakglassManage} {
		// The matrices show the row once a migration granted the name
		// somewhere (an unknown name is a 400 before any rule runs).
		if _, err := env.pool.Exec(context.Background(), `
INSERT INTO public.auth_core__role_permission (role_id, permission)
SELECT id, $1 FROM public.auth_core__role WHERE mode = 'default' AND name = 'system'
ON CONFLICT DO NOTHING`, permission); err != nil {
			t.Fatal(err)
		}
		expectRefusal(t, adminDo(t, router, http.MethodPut, "/p/admin/user_project_permissions/administration",
			map[string]any{"editor": []string{permission}}), adminaccess.CodeReservedPermission)
		if contains(projectGrantsSQL(t, env.pool, 3, "editor"), permission) {
			t.Fatalf("the personal project editor holds %s after the refusal", permission)
		}
		expectRefusal(t, saveCell(t, router, "/m/admin/permissions/administration/default", permission, "editor", true),
			adminaccess.CodeReservedPermission)
		if contains(grantsSQL(t, env.pool, "default", "editor"), permission) {
			t.Fatalf("the default editor holds %s after the refusal", permission)
		}
	}
}

/* ── a custom role that edits the matrix ──────────────────────────────── */

// matrix_editor holds configuration.roles.permissions.edit and .view only.

func TestAMatrixEditorCannotAddAPermissionItDoesNotHold(t *testing.T) {
	env := newEscalationEnvironment(t)
	expectRefusal(t, saveCell(t, env.as(escalationMatrix), centralMatrix, "admin.auth.users", "empty_role", true),
		adminaccess.CodePermissionNotHeld)
	if contains(grantsSQL(t, env.pool, "administration", "empty_role"), "admin.auth.users") {
		t.Fatal("the refused grant was written")
	}
}

func TestAMatrixEditorCannotRemoveAPermissionItDoesNotHold(t *testing.T) {
	env := newEscalationEnvironment(t)
	expectRefusal(t, saveCell(t, env.as(escalationMatrix), centralMatrix, "modes.users", "modes_operator", false),
		adminaccess.CodePermissionNotHeld)
	if !contains(grantsSQL(t, env.pool, "administration", "modes_operator"), "modes.users") {
		t.Fatal("the refused revoke was written")
	}
}

func TestAMatrixEditorCannotEditARoleItHolds(t *testing.T) {
	env := newEscalationEnvironment(t)
	expectRefusal(t, saveCell(t, env.as(escalationMatrix), centralMatrix,
		"configuration.roles.permissions.view", "matrix_editor", false),
		adminaccess.CodeOwnRoleLocked)
	if !contains(grantsSQL(t, env.pool, "administration", "matrix_editor"), "configuration.roles.permissions.view") {
		t.Fatal("the refused revoke was written")
	}
}

/* ── the reserved role name ───────────────────────────────────────────── */

func TestBreakGlassIsNotARoleName(t *testing.T) {
	env := newEscalationEnvironment(t)
	router := env.as(escalationSuper)
	expectStatus(t, adminDo(t, router, http.MethodPost, centralRoles,
		map[string]string{"name": adminaccess.ReservedRoleName}), http.StatusBadRequest)
	if roleExists(t, env.pool, adminaccess.ReservedRoleName) {
		t.Fatal("the refused role was created")
	}
	// In any case.
	expectStatus(t, adminDo(t, router, http.MethodPost, centralRoles,
		map[string]string{"name": "Break_Glass"}), http.StatusBadRequest)
	// Nor the new name of a rename.
	expectStatus(t, adminDo(t, router, http.MethodPut, centralRoles,
		map[string]string{"name": "empty_role", "new_name": adminaccess.ReservedRoleName}), http.StatusBadRequest)
	if roleExists(t, env.pool, adminaccess.ReservedRoleName) {
		t.Fatal("the refused rename was written")
	}
}
