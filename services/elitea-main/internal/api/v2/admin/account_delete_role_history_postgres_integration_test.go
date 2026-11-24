package admin_test

// Deleting an account revokes its central roles with a history row (issue
// 2922). auth_core__user_role cascades from auth_core__user, so before this
// the delete dropped every role the account held, a super_admin included, and
// the grant history still showed the role as granted.

import (
	"context"
	"fmt"
	"net/http"
	"testing"
)

func TestDeletingAnAccountRecordsTheRevocationOfItsRoles(t *testing.T) {
	env := newEscalationEnvironment(t)
	target := env.ids[escalationTarget] // holds admin
	actor := env.ids[escalationSuper]
	// A central default-mode role is a central assignment too.
	if _, err := env.pool.Exec(context.Background(), `
INSERT INTO public.auth_core__user_role (user_id, role_id)
SELECT $1, id FROM public.auth_core__role WHERE mode = 'default' AND name = 'viewer'`, target); err != nil {
		t.Fatalf("give the target a default-mode role: %v", err)
	}

	expectStatus(t, adminDo(t, env.asBrowser(escalationSuper), http.MethodPost, usersAction,
		map[string]any{"action": "delete", "users": []map[string]any{{"id": target}}}), http.StatusOK)

	var remaining int
	if err := env.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM public.auth_core__user WHERE id = $1`, target).Scan(&remaining); err != nil {
		t.Fatal(err)
	}
	if remaining != 0 {
		t.Fatalf("the account was not deleted")
	}
	want := []grantRow{
		{user: target, actor: actor, role: "admin", mode: "administration", action: "revoked", source: "console"},
		{user: target, actor: actor, role: "viewer", mode: "default", action: "revoked", source: "console"},
	}
	if got := grantHistory(t, env.pool, target); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("history = %+v, want %+v", got, want)
	}
}

// Deleting an account takes its administration roles, so the rules of a role
// removal apply: a caller without super_admin cannot delete a super_admin,
// and nothing is deleted or recorded.
func TestDeletingASuperAdminNeedsSuperAdmin(t *testing.T) {
	env := newEscalationEnvironment(t)
	superAdmin := env.ids[escalationSuper]
	recorder := adminDo(t, env.as(escalationUsersAdmin), http.MethodPost, usersAction,
		map[string]any{"action": "delete", "users": []map[string]any{{"id": superAdmin}}})
	expectRefusal(t, recorder, "role_super_admin_required")

	var remaining int
	if err := env.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM public.auth_core__user WHERE id = $1`, superAdmin).Scan(&remaining); err != nil {
		t.Fatal(err)
	}
	if remaining != 1 {
		t.Fatalf("the super_admin was deleted by a caller without super_admin")
	}
	if history := grantHistory(t, env.pool, superAdmin); len(history) != 0 {
		t.Fatalf("a refused delete wrote history: %+v", history)
	}
}

// Without the seed record (a database whose auth_core tables arrived after
// shared 0263) nothing is marked unassignable, and `system` is still refused
// by name.
func TestSystemStaysUnassignableWithoutTheSeedRecord(t *testing.T) {
	env := newEscalationEnvironment(t)
	for _, statement := range []string{
		`DROP TABLE elitea_auth.admin_role_seeds`,
		`INSERT INTO public.auth_core__role (name, mode) VALUES ('system', 'administration') ON CONFLICT DO NOTHING`,
	} {
		if _, err := env.pool.Exec(context.Background(), statement); err != nil {
			t.Fatalf("%s: %v", statement, err)
		}
	}
	plain := env.ids[escalationPlainTarget]
	expectRefusal(t, adminDo(t, env.as(escalationSuper), http.MethodPost, usersAction,
		forUser(setRoles("system"), plain)), "role_set_locked")
	if history := grantHistory(t, env.pool, plain); len(history) != 0 {
		t.Fatalf("a refused request wrote history: %+v", history)
	}
}

// An account with no role writes no history; the delete is unchanged.
func TestDeletingAnAccountWithoutRolesWritesNoHistory(t *testing.T) {
	env := newEscalationEnvironment(t)
	plain := env.ids[escalationPlainTarget]
	expectStatus(t, adminDo(t, env.as(escalationSuper), http.MethodPost, usersAction,
		map[string]any{"action": "delete", "users": []map[string]any{{"id": plain}}}), http.StatusOK)
	if history := grantHistory(t, env.pool, plain); len(history) != 0 {
		t.Fatalf("history = %+v, want none", history)
	}
}
