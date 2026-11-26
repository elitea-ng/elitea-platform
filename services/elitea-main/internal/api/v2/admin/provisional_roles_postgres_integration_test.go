package admin_test

// What an elevated person gives to someone else is provisional (ADR-0059
// decision 5; stories R2.12a, issue 2526, and R2.12b, issue 2606): the central
// role routes and the bulk invite write a row of
// elitea_auth.admin_provisional_grants, never auth_core__user_role or
// auth_core__project_user_role, and say so in the response.

import (
	"context"
	"net/http"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/db/provisionalgrants"
)

func provisionalRows(t *testing.T, pool *pgxpool.Pool, kind string, userID int) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(), `
SELECT count(*) FROM elitea_auth.admin_provisional_grants WHERE kind = $1 AND user_id = $2`, kind, userID).Scan(&n); err != nil {
		t.Fatalf("read the provisional grants: %v", err)
	}
	return n
}

func TestAnElevatedRoleAssignmentIsProvisional(t *testing.T) {
	provisionalgrants.EnableForTest(t)
	env := elevatedAdminEnvironment(t)
	router := env.asSession(escalationAdmin, elevatedSessionRef)
	plain := env.ids[escalationPlainTarget]

	recorder := adminDo(t, router, http.MethodPost, usersAction,
		map[string]any{"action": "set_admin_role", "user_id": plain, "role_name": "editor"})
	expectStatus(t, recorder, http.StatusOK)
	var body struct {
		Provisional          bool     `json:"provisional"`
		ProvisionalRoleNames []string `json:"provisional_role_names"`
	}
	decodeJSONBody(t, recorder.Body.Bytes(), &body)
	if !body.Provisional || len(body.ProvisionalRoleNames) != 1 || body.ProvisionalRoleNames[0] != "editor" {
		t.Fatalf("response = %s, want the role marked provisional", recorder.Body.String())
	}
	if names := adminRoleNamesSQL(t, env.pool, plain); len(names) != 0 {
		t.Fatalf("the target holds %v standing, want none", names)
	}
	if n := provisionalRows(t, env.pool, provisionalgrants.KindRole, plain); n != 1 {
		t.Fatalf("%d provisional role rows, want 1", n)
	}

	// The modes route gives provisionally too.
	other := env.ids[escalationUsersAdmin]
	recorder = adminDo(t, router, http.MethodPost, "/o/admin/modes/administration",
		map[string]any{"user_id": other, "mode": "administration", "role": "editor"})
	expectStatus(t, recorder, http.StatusOK)
	var modes struct {
		Provisional bool `json:"provisional"`
	}
	decodeJSONBody(t, recorder.Body.Bytes(), &modes)
	if !modes.Provisional {
		t.Fatalf("modes response = %s, want provisional", recorder.Body.String())
	}
	if names := adminRoleNamesSQL(t, env.pool, other); len(names) != 1 || names[0] != "users_operator" {
		t.Fatalf("the modes target holds %v standing, want only its own users_operator", names)
	}
	if n := provisionalRows(t, env.pool, provisionalgrants.KindRole, other); n != 1 {
		t.Fatalf("%d provisional role rows of the modes target, want 1", n)
	}
}

// A standing assignment (no elevation) is unchanged: it writes the role.
func TestAStandingRoleAssignmentIsNotProvisional(t *testing.T) {
	provisionalgrants.EnableForTest(t)
	env := newEscalationEnvironment(t)
	plain := env.ids[escalationPlainTarget]
	recorder := adminDo(t, env.as(escalationSuper), http.MethodPost, usersAction,
		map[string]any{"action": "set_admin_role", "user_id": plain, "role_name": "editor"})
	expectStatus(t, recorder, http.StatusOK)
	var body struct {
		Provisional bool `json:"provisional"`
	}
	decodeJSONBody(t, recorder.Body.Bytes(), &body)
	if body.Provisional || provisionalRows(t, env.pool, provisionalgrants.KindRole, plain) != 0 {
		t.Fatalf("a standing assignment was provisional: %s", recorder.Body.String())
	}
	expectRoles(t, env, escalationPlainTarget, "editor")
}

func TestAnElevatedBulkInviteIsProvisional(t *testing.T) {
	provisionalgrants.EnableForTest(t)
	env := elevatedAdminEnvironment(t)
	router := env.asSession(escalationAdmin, elevatedSessionRef)
	plain := env.ids[escalationPlainTarget]

	recorder := adminDo(t, router, http.MethodPost, "/b/admin/invites_bulk/administration",
		map[string]any{"users": []int{plain}, "projects": []int{2}, "role": "editor"})
	expectStatus(t, recorder, http.StatusOK)
	var summary struct {
		Added   int `json:"added"`
		Results []struct {
			Outcome     string `json:"outcome"`
			Provisional bool   `json:"provisional"`
		} `json:"results"`
	}
	decodeJSONBody(t, recorder.Body.Bytes(), &summary)
	if summary.Added != 1 || len(summary.Results) != 1 || !summary.Results[0].Provisional {
		t.Fatalf("summary = %s, want one provisional addition", recorder.Body.String())
	}
	if names := storedRoleNames(t, env.pool, 2, int64(plain)); len(names) != 0 {
		t.Fatalf("the target holds %v standing in project 2, want none", names)
	}
	if n := provisionalRows(t, env.pool, provisionalgrants.KindMembership, plain); n != 1 {
		t.Fatalf("%d provisional membership rows, want 1", n)
	}
}
