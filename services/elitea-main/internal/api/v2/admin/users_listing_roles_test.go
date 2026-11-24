package admin_test

// The Users listing reports the set of roles, not the highest of four names
// (ADR-0059 decision 7, "Checks by role name", story R0.13).
//
// Before R0.13 the listing ranked the four legacy names first and the kept
// single `admin_role` field was the highest of them; a person who held only a
// narrow role read as no administrator in that field. The set is now listed by
// name, and a person with only `llm_manager` is listed with it. It runs on
// PostgreSQL (newEscalationEnvironment).

import (
	"net/http"
	"slices"
	"testing"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/adminaccess"
)

func TestTheUsersListingShowsANarrowRoleHeldAlone(t *testing.T) {
	env := newEscalationEnvironment(t)
	seedBuiltInRoles(t, env, adminaccess.RoleLLMManager)
	giveRole(t, env, escalationPlainTarget, adminaccess.RoleLLMManager)

	row := listedUser(t, env.as(escalationSuper), env.ids[escalationPlainTarget])
	if !slices.Equal(row.AdminRoles, []string{adminaccess.RoleLLMManager}) || !row.IsAdmin {
		t.Fatalf("listing = %+v, want admin_roles [llm_manager] and is_admin true", row)
	}
	// The single legacy field is gone (R0.8c): the set is the only answer.
	if row.AdminRole != nil {
		t.Fatalf("admin_role = %s, want the field absent", row.AdminRole)
	}
}

// The listing orders the set by name only: a custom role that sorts before
// `super_admin` is listed before it.
func TestTheUsersListingRanksNoRoleNameAboveAnother(t *testing.T) {
	env := newEscalationEnvironment(t)
	giveRole(t, env, escalationSuper, "empty_role")

	row := listedUser(t, env.as(escalationSuper), env.ids[escalationSuper])
	if !slices.Equal(row.AdminRoles, []string{"empty_role", "super_admin"}) {
		t.Fatalf("admin_roles = %v, want [empty_role super_admin]", row.AdminRoles)
	}
	if row.AdminRole != nil {
		t.Fatalf("admin_role = %s, want the field absent", row.AdminRole)
	}
}

// The demotion guard tests the permission admin.auth.users.super_admin, not
// the caller's role name: a custom role that holds the permission may take
// super_admin away, and the admin role, which does not, may not.
func TestTheSuperAdminDemotionGuardTestsThePermission(t *testing.T) {
	env := newEscalationEnvironment(t)
	if _, err := env.pool.Exec(t.Context(), `
INSERT INTO public.auth_core__role (name, mode) VALUES ('platform_owner', 'administration');
INSERT INTO public.auth_core__role_permission (role_id, permission)
SELECT owner.id, granted.permission
FROM public.auth_core__role owner,
     (SELECT DISTINCT grant_row.permission
      FROM public.auth_core__role_permission grant_row
      JOIN public.auth_core__role super ON super.id = grant_row.role_id
      WHERE super.name = 'super_admin' AND super.mode = 'administration') AS granted
WHERE owner.name = 'platform_owner' AND owner.mode = 'administration';`); err != nil {
		t.Fatalf("seed a custom role with the permission: %v", err)
	}
	giveRole(t, env, escalationUsersAdmin, "platform_owner")
	giveRole(t, env, escalationPlainTarget, adminaccess.RoleSuperAdmin)
	target := env.ids[escalationPlainTarget]

	// admin does not hold admin.auth.users.super_admin: refused.
	expectRefusal(t, adminDo(t, env.as(escalationAdmin), http.MethodPost, usersAction,
		forUser(setRoles(), target)), adminaccess.CodeSuperAdminRequired)
	expectRoles(t, env, escalationPlainTarget, adminaccess.RoleSuperAdmin)

	// A role of another name that holds it: allowed.
	expectStatus(t, adminDo(t, env.as(escalationUsersAdmin), http.MethodPost, usersAction,
		forUser(setRoles(), target)), http.StatusOK)
	expectRoles(t, env, escalationPlainTarget)
}
