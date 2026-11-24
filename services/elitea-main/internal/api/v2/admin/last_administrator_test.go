package admin_test

// The last-administrator rule on the admin routes (ADR-0059 R0.11, issue
// 2500): the last unsuspended holder of admin.auth.users.super_admin cannot be
// demoted, deleted or suspended, by anyone, the global administrator included.
// Each refusal is a 409 with code last_administrator and a reason that names
// the operator command, and is followed by a read that proves nothing moved.
//
// The fixture is newEscalationEnvironment: esc-super is the one holder of the
// permission (the super_admin role); esc-admin and esc-target hold `admin`.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/adminaccess"
)

func userSuspend(userID int) string {
	return fmt.Sprintf("/u/admin/user_suspend/administration/%d", userID)
}

// expectLastAdministrator asserts the 409 and its reason.
func expectLastAdministrator(t *testing.T, recorder *httptest.ResponseRecorder) {
	t.Helper()
	if recorder.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409 (body %s)", recorder.Code, recorder.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode %q: %v", recorder.Body.String(), err)
	}
	if body["code"] != adminaccess.CodeLastAdministrator {
		t.Fatalf("code = %v, want %s", body["code"], adminaccess.CodeLastAdministrator)
	}
	message, _ := body["error"].(string)
	if !strings.Contains(message, adminaccess.SuccessorCommand) {
		t.Fatalf("the reason must name %q for a successor: %q", adminaccess.SuccessorCommand, message)
	}
}

func accountExists(t *testing.T, env escalationEnvironment, id int) bool {
	t.Helper()
	var exists bool
	if err := env.pool.QueryRow(t.Context(),
		`SELECT EXISTS (SELECT 1 FROM public.auth_core__user WHERE id = $1)`, id).Scan(&exists); err != nil {
		t.Fatalf("read account %d: %v", id, err)
	}
	return exists
}

func accountSuspended(t *testing.T, env escalationEnvironment, id int) bool {
	t.Helper()
	var suspended bool
	if err := env.pool.QueryRow(t.Context(),
		`SELECT suspended FROM public.auth_core__user WHERE id = $1`, id).Scan(&suspended); err != nil {
		t.Fatalf("read account %d: %v", id, err)
	}
	return suspended
}

// Acceptance (1): demoting the last holder answers 409 with a reason, on both
// assignment routes, the set form and the single form of the Users route
// included.
func TestDemotingTheLastAdministratorIsRefusedOnBothAssignmentRoutes(t *testing.T) {
	env := newEscalationEnvironment(t)
	super := env.ids[escalationSuper]
	caller := env.as(escalationSuper)

	for name, request := range map[string]struct {
		method, target string
		body           any
	}{
		"Users route, set form":    {http.MethodPost, usersAction, forUser(setRoles(), super)},
		"Users route, single form": {http.MethodPost, usersAction, map[string]any{"action": "set_admin_role", "user_id": super, "role_name": "admin"}},
		"modes route":              {http.MethodDelete, modesRemove(super, adminaccess.SuperAdminRole), nil},
	} {
		t.Run(name, func(t *testing.T) {
			expectLastAdministrator(t, adminDo(t, caller, request.method, request.target, request.body))
			expectNothingWritten(t, env, escalationSuper, adminaccess.SuperAdminRole)
		})
	}
}

// The rule refuses only the LAST holder: with a second holder of the
// permission, the same demotion goes through.
func TestDemotingOneOfTwoAdministratorsIsAllowed(t *testing.T) {
	env := newEscalationEnvironment(t)
	giveRole(t, env, escalationTarget, adminaccess.SuperAdminRole)

	expectStatus(t, adminDo(t, env.as(escalationSuper), http.MethodDelete,
		modesRemove(env.ids[escalationTarget], adminaccess.SuperAdminRole), nil), http.StatusOK)
	expectRoles(t, env, escalationTarget, "admin")

	// Now esc-super is the last one again.
	expectLastAdministrator(t, adminDo(t, env.as(escalationSuper), http.MethodDelete,
		modesRemove(env.ids[escalationSuper], adminaccess.SuperAdminRole), nil))
}

// Acceptance (5): deleting the last holder through AuthUsersAction, single
// and bulk, answers 409. The bulk request is refused whole: the other account
// of the selection is not deleted first.
func TestDeletingTheLastAdministratorIsRefused(t *testing.T) {
	env := newEscalationEnvironment(t)
	super, plain := env.ids[escalationSuper], env.ids[escalationPlainTarget]
	caller := env.as(escalationSuper)

	t.Run("single", func(t *testing.T) {
		expectLastAdministrator(t, adminDo(t, caller, http.MethodPost, usersAction, map[string]any{
			"action": "delete", "users": []map[string]any{{"id": super}},
		}))
		if !accountExists(t, env, super) {
			t.Fatal("the last administrator was deleted")
		}
		expectRoles(t, env, escalationSuper, adminaccess.SuperAdminRole)
	})
	t.Run("bulk", func(t *testing.T) {
		expectLastAdministrator(t, adminDo(t, caller, http.MethodPost, usersAction, map[string]any{
			"action": "delete", "users": []map[string]any{{"id": plain}, {"id": super}},
		}))
		if !accountExists(t, env, super) || !accountExists(t, env, plain) {
			t.Fatal("a refused bulk delete must delete none of the selection")
		}
		if history := grantHistory(t, env.pool, super); len(history) != 0 {
			t.Fatalf("a refused delete wrote history: %+v", history)
		}
	})
}

// Acceptance (5): suspending the last holder through UserSuspend answers 409.
// (Suspending one who is not the last goes through:
// last_administrator_postgres_integration_test.go, on a database with the
// session tables the suspension revokes.)
func TestSuspendingTheLastAdministratorIsRefused(t *testing.T) {
	env := newEscalationEnvironment(t)
	caller := env.as(escalationSuper)

	expectLastAdministrator(t, adminDo(t, caller, http.MethodPut,
		userSuspend(env.ids[escalationSuper]), map[string]any{"suspended": true}))
	if accountSuspended(t, env, env.ids[escalationSuper]) {
		t.Fatal("the last administrator was suspended")
	}
}

// A suspended holder is no administrator: with the only other holder
// suspended, esc-super is still the last one, and an unsuspended second holder
// lifts the rule.
func TestASuspendedHolderDoesNotCount(t *testing.T) {
	env := newEscalationEnvironment(t)
	giveRole(t, env, escalationTarget, adminaccess.SuperAdminRole)
	if _, err := env.pool.Exec(t.Context(),
		`UPDATE public.auth_core__user SET suspended = true WHERE id = $1`, env.ids[escalationTarget]); err != nil {
		t.Fatalf("suspend the second holder: %v", err)
	}
	expectLastAdministrator(t, adminDo(t, env.as(escalationSuper), http.MethodDelete,
		modesRemove(env.ids[escalationSuper], adminaccess.SuperAdminRole), nil))
}
