package admin_test

// The people an elevation cannot remove, and the own roles (ADR-0059 decision
// 5, "Five rules are not per action"; story R2.8b, issue 2602). The caller is
// a standing `admin` under an ACTIVE elevation bound to the session of the
// request, resolved by the real resolver, on each path that demotes, suspends
// or deletes a person.

import (
	"context"
	"fmt"
	"net/http"
	"testing"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/adminaccess"
)

const (
	protectedReviewer = "r28b-reviewer@autotest.local" // holds access.breakglass.review
	protectedApprover = "r28b-approver@autotest.local" // holds access.breakglass.approve
)

// protectedEnvironment is the escalation environment with a reviewer, an
// approver and the caller's own eligibility role, and an active elevation of
// the `admin` caller at level.
func protectedEnvironment(t *testing.T, level string) escalationEnvironment {
	t.Helper()
	env := newEscalationEnvironment(t)
	ctx := context.Background()
	for _, statement := range []string{
		`INSERT INTO public.auth_core__role (name, mode) VALUES
			('r28b_reviewer', 'administration'), ('r28b_approver', 'administration'),
			('emergency_access', 'administration')`,
		`INSERT INTO public.auth_core__role_permission (role_id, permission)
			SELECT role.id, grant_row.permission FROM public.auth_core__role role
			JOIN (VALUES ('r28b_reviewer', 'access.breakglass.review'),
			             ('r28b_approver', 'access.breakglass.approve'),
			             ('emergency_access', 'access.breakglass.request'))
			  AS grant_row(role_name, permission) ON grant_row.role_name = role.name
			WHERE role.mode = 'administration'`,
		`INSERT INTO public.auth_core__user (email, name) VALUES
			('r28b-reviewer@autotest.local', 'Reviewer'), ('r28b-approver@autotest.local', 'Approver')`,
		`INSERT INTO public.auth_core__user_role (user_id, role_id)
			SELECT account.id, role.id FROM public.auth_core__user account
			JOIN (VALUES ('r28b-reviewer@autotest.local', 'r28b_reviewer'),
			             ('r28b-approver@autotest.local', 'r28b_approver'),
			             ('esc-admin@autotest.local', 'emergency_access'))
			  AS pair(email, role_name) ON pair.email = account.email
			JOIN public.auth_core__role role ON role.name = pair.role_name AND role.mode = 'administration'`,
	} {
		if _, err := env.pool.Exec(ctx, statement); err != nil {
			t.Fatalf("seed %q: %v", statement, err)
		}
	}
	for _, email := range []string{protectedReviewer, protectedApprover} {
		env.ids[email] = userID(t, env.pool, email)
	}
	seedActiveElevation(t, env.pool, env.ids[escalationAdmin], env.ids[escalationSuper], elevatedSessionRef, level)
	return env
}

func suspended(t *testing.T, env escalationEnvironment, email string) bool {
	t.Helper()
	var value bool
	if err := env.pool.QueryRow(context.Background(),
		`SELECT suspended FROM public.auth_core__user WHERE id = $1`, env.ids[email]).Scan(&value); err != nil {
		t.Fatalf("read %s: %v", email, err)
	}
	return value
}

func TestAnElevatedPersonCannotSuspendAReviewer(t *testing.T) {
	env := protectedEnvironment(t, adminaccess.ElevationLevelAdmin)
	router := env.asSession(escalationAdmin, elevatedSessionRef)
	for _, email := range []string{protectedReviewer, protectedApprover} {
		expectRefusal(t, adminDo(t, router, http.MethodPut,
			fmt.Sprintf("/u/admin/user_suspend/administration/%d", env.ids[email]), map[string]any{"suspended": true}),
			adminaccess.CodeElevationProtectedPerson)
		if suspended(t, env, email) {
			t.Fatalf("%s was suspended", email)
		}
	}
}

// Demoting and deleting a holder of access.breakglass.approve, one case per
// path: the Users page set, the modes removal, and the account delete.
func TestAnElevatedPersonCannotDemoteOrDeleteAnApprover(t *testing.T) {
	for _, level := range []string{adminaccess.ElevationLevelAdmin, adminaccess.ElevationLevelSuperAdmin} {
		t.Run(level, func(t *testing.T) {
			env := protectedEnvironment(t, level)
			router := env.asSession(escalationAdmin, elevatedSessionRef)
			approver := env.ids[protectedApprover]
			expectRefusal(t, adminDo(t, router, http.MethodPost, usersAction,
				map[string]any{"action": "set_admin_role", "user_id": approver, "role_names": []string{}}),
				adminaccess.CodeElevationProtectedPerson)
			expectRefusal(t, adminDo(t, router, http.MethodDelete, modesRemove(approver, "r28b_approver"), nil),
				adminaccess.CodeElevationProtectedPerson)
			expectRefusal(t, adminDo(t, router, http.MethodPost, usersAction,
				map[string]any{"action": "delete", "users": []map[string]any{{"id": approver}}}),
				adminaccess.CodeElevationProtectedPerson)
			expectRoles(t, env, protectedApprover, "r28b_approver")
		})
	}
}

// The own roles and the own eligibility, at both levels: the exemption of a
// global administrator is read from standing roles, so an elevation never
// carries it.
func TestAnElevatedPersonCannotChangeTheOwnRolesOrEligibility(t *testing.T) {
	for _, level := range []string{adminaccess.ElevationLevelAdmin, adminaccess.ElevationLevelSuperAdmin} {
		t.Run(level, func(t *testing.T) {
			env := protectedEnvironment(t, level)
			router := env.asSession(escalationAdmin, elevatedSessionRef)
			self := env.ids[escalationAdmin]
			expectRefusal(t, adminDo(t, router, http.MethodPost, modesAssign,
				map[string]any{"user_id": self, "mode": "administration", "role": "editor"}),
				adminaccess.CodeSelfAssignment)
			expectRefusal(t, adminDo(t, router, http.MethodDelete, modesRemove(self, "emergency_access"), nil),
				adminaccess.CodeSelfAssignment)
			expectRefusal(t, adminDo(t, router, http.MethodPost, usersAction,
				map[string]any{"action": "set_admin_role", "user_id": self, "role_names": []string{"admin"}}),
				adminaccess.CodeSelfAssignment)
			expectRoles(t, env, escalationAdmin, "admin", "emergency_access")
		})
	}
}
