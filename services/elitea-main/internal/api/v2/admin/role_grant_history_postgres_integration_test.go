package admin_test

// The history of central role assignments (issue 2499, ADR-0059 R0.10a). Both
// assignment routes now write a difference: each change writes one row of
// elitea_auth.admin_role_grants in its own transaction, and a role the user
// already holds writes nothing and leaves the assignment row as it was.

import (
	"context"
	"fmt"
	"net/http"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/v2/admin"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/auth"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/legacyrbac"
)

type grantRow struct {
	user, actor        int
	role, mode, action string
	source             string
}

func grantHistory(t *testing.T, pool *pgxpool.Pool, userID int) []grantRow {
	t.Helper()
	rows, err := pool.Query(context.Background(), `
SELECT user_id, COALESCE(actor_user_id, 0), role_name, role_mode, action, source
FROM elitea_auth.admin_role_grants
WHERE user_id = $1
ORDER BY id`, userID)
	if err != nil {
		t.Fatalf("read the history: %v", err)
	}
	defer rows.Close()
	var history []grantRow
	for rows.Next() {
		var row grantRow
		if err := rows.Scan(&row.user, &row.actor, &row.role, &row.mode, &row.action, &row.source); err != nil {
			t.Fatalf("scan the history: %v", err)
		}
		history = append(history, row)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read the history: %v", err)
	}
	return history
}

// assignmentIdentity is the row's id and xmin: a row that was deleted and
// re-inserted, or updated, has a new one.
func assignmentIdentity(t *testing.T, pool *pgxpool.Pool, userID int, role string) string {
	t.Helper()
	var identity string
	if err := pool.QueryRow(context.Background(), `
SELECT assignment.id::text || '/' || assignment.xmin::text
FROM public.auth_core__user_role assignment
JOIN public.auth_core__role role ON role.id = assignment.role_id
WHERE assignment.user_id = $1 AND role.mode = 'administration' AND role.name = $2`,
		userID, role).Scan(&identity); err != nil {
		t.Fatalf("read the %s assignment of user %d: %v", role, userID, err)
	}
	return identity
}

// asBrowser mounts both assignment routes for a caller signed in through a
// server-side browser session, whose changes the history records as
// `console`.
func (e escalationEnvironment) asBrowser(email string) chi.Router {
	return e.asPrincipal(email, func(principal *auth.User) { principal.BrowserSessionRef = 77 })
}

// asPrincipal mounts both assignment routes for a caller whose principal
// `shape` adjusts.
func (e escalationEnvironment) asPrincipal(email string, shape func(*auth.User)) chi.Router {
	id := fmt.Sprint(e.ids[email])
	principal := &auth.User{ID: id, UserID: id}
	shape(principal)
	handler := admin.NewHandler(e.pool, admin.WithPermissionResolver(legacyrbac.NewPostgresResolver(e.pool)))
	router := chi.NewRouter()
	router.Mount("/o", modesRouter(handler, principal))
	router.Mount("/u", adminUsersRouter(handler, principal))
	return router
}

func TestAssigningAHeldRoleWritesNoHistoryAndKeepsTheRow(t *testing.T) {
	env := newEscalationEnvironment(t)
	target := env.ids[escalationTarget] // holds admin
	before := assignmentIdentity(t, env.pool, target, "admin")

	expectStatus(t, adminDo(t, env.as(escalationSuper), http.MethodPost, "/u/admin/auth_users/administration",
		map[string]any{"action": "set_admin_role", "user_id": target, "role_name": "admin"}), http.StatusOK)
	expectStatus(t, adminDo(t, env.as(escalationSuper), http.MethodPost, "/o/admin/modes/administration",
		map[string]any{"user_id": target, "mode": "administration", "role": "admin"}), http.StatusOK)

	if after := assignmentIdentity(t, env.pool, target, "admin"); after != before {
		t.Fatalf("the held assignment was rewritten: %s became %s", before, after)
	}
	if history := grantHistory(t, env.pool, target); len(history) != 0 {
		t.Fatalf("assigning a held role wrote history: %+v", history)
	}
}

func TestTheUsersRouteRecordsAGrantAndARevocation(t *testing.T) {
	env := newEscalationEnvironment(t)
	plain := env.ids[escalationPlainTarget]
	actor := env.ids[escalationSuper]

	expectStatus(t, adminDo(t, env.as(escalationSuper), http.MethodPost, "/u/admin/auth_users/administration",
		map[string]any{"action": "set_admin_role", "user_id": plain, "role_name": "editor"}), http.StatusOK)
	expectStatus(t, adminDo(t, env.as(escalationSuper), http.MethodPost, "/u/admin/auth_users/administration",
		map[string]any{"action": "set_admin_role", "user_id": plain, "role_name": nil}), http.StatusOK)

	want := []grantRow{
		{user: plain, actor: actor, role: "editor", mode: "administration", action: "granted", source: "api"},
		{user: plain, actor: actor, role: "editor", mode: "administration", action: "revoked", source: "api"},
	}
	if got := grantHistory(t, env.pool, plain); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("history = %+v, want %+v", got, want)
	}
}

func TestTheUsersRouteRecordsAReplacementAsTwoChanges(t *testing.T) {
	env := newEscalationEnvironment(t)
	target := env.ids[escalationTarget] // holds admin
	actor := env.ids[escalationSuper]

	expectStatus(t, adminDo(t, env.asBrowser(escalationSuper), http.MethodPost, "/u/admin/auth_users/administration",
		map[string]any{"action": "set_admin_role", "user_id": target, "role_name": "editor"}), http.StatusOK)

	want := []grantRow{
		{user: target, actor: actor, role: "admin", mode: "administration", action: "revoked", source: "console"},
		{user: target, actor: actor, role: "editor", mode: "administration", action: "granted", source: "console"},
	}
	if got := grantHistory(t, env.pool, target); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("history = %+v, want %+v", got, want)
	}
}

func TestTheModesRouteRecordsAGrantAndARevocation(t *testing.T) {
	env := newEscalationEnvironment(t)
	plain := env.ids[escalationPlainTarget]
	actor := env.ids[escalationSuper]
	router := env.asBrowser(escalationSuper)

	expectStatus(t, adminDo(t, router, http.MethodPost, "/o/admin/modes/administration",
		map[string]any{"user_id": plain, "mode": "administration", "role": "editor"}), http.StatusOK)
	expectStatus(t, adminDo(t, router, http.MethodDelete,
		fmt.Sprintf("/o/admin/modes/administration?id=%d:administration:editor", plain), nil), http.StatusOK)
	// Removing what the user does not hold is a 404 and writes nothing.
	expectStatus(t, adminDo(t, router, http.MethodDelete,
		fmt.Sprintf("/o/admin/modes/administration?id=%d:administration:editor", plain), nil), http.StatusNotFound)

	want := []grantRow{
		{user: plain, actor: actor, role: "editor", mode: "administration", action: "granted", source: "console"},
		{user: plain, actor: actor, role: "editor", mode: "administration", action: "revoked", source: "console"},
	}
	if got := grantHistory(t, env.pool, plain); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("history = %+v, want %+v", got, want)
	}
}

// The source by credential: the Form plane and the session cookie carry
// AuthType "session" and no session ref, and are a browser; a token is not.
func TestTheSourceFollowsTheCredential(t *testing.T) {
	for _, tc := range []struct {
		name   string
		shape  func(p *auth.User, tokenID string)
		source string
	}{
		{"form session", func(p *auth.User, _ string) { p.AuthType = "session" }, "console"},
		{"server-side session", func(p *auth.User, _ string) {
			p.AuthType = "session"
			p.BrowserSessionRef = 9
		}, "console"},
		{"token", func(p *auth.User, token string) { p.AuthType = "token"; p.TokenID = token }, "api"},
		{"session-typed token", func(p *auth.User, token string) { p.AuthType = "session"; p.TokenID = token }, "api"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := newEscalationEnvironment(t)
			plain := env.ids[escalationPlainTarget]
			var tokenID string
			if err := env.pool.QueryRow(context.Background(), `
INSERT INTO public.auth_core__token (user_id, name, uuid)
VALUES ($1, 'history-source', '6f1c1a52-2499-4d0a-9c1e-000000002499')
RETURNING id::text`, env.ids[escalationSuper]).Scan(&tokenID); err != nil {
				t.Fatalf("seed the caller's token: %v", err)
			}
			shape := func(p *auth.User) { tc.shape(p, tokenID) }
			expectStatus(t, adminDo(t, env.asPrincipal(escalationSuper, shape), http.MethodPost,
				"/u/admin/auth_users/administration",
				map[string]any{"action": "set_admin_role", "user_id": plain, "role_name": "editor"}), http.StatusOK)
			history := grantHistory(t, env.pool, plain)
			if len(history) != 1 || history[0].source != tc.source {
				t.Fatalf("history = %+v, want one %s row", history, tc.source)
			}
		})
	}
}

// A refused or failed change writes no history: the row is in the
// transaction the refusal rolls back.
func TestAnUndefinedRoleRollsBackTheRevocationAndItsHistory(t *testing.T) {
	env := newEscalationEnvironment(t)
	target := env.ids[escalationTarget] // holds admin
	before := assignmentIdentity(t, env.pool, target, "admin")

	expectStatus(t, adminDo(t, env.as(escalationSuper), http.MethodPost, "/o/admin/modes/administration",
		map[string]any{"user_id": target, "mode": "administration", "role": "no_such_role"}), http.StatusBadRequest)

	if after := assignmentIdentity(t, env.pool, target, "admin"); after != before {
		t.Fatalf("a refused assignment moved the held role: %s became %s", before, after)
	}
	if history := grantHistory(t, env.pool, target); len(history) != 0 {
		t.Fatalf("a refused assignment wrote history: %+v", history)
	}
}
