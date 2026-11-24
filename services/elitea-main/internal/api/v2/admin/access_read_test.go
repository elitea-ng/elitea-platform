package admin_test

// The Access reads (ADR-0059 R0.10b, issue 2588): who holds which
// administration role, and the grant history. Both are gated on
// access.roles.view or the names it narrows, so a security auditor reads them
// without the user directory.
//
// The gates are mounted as internal/api/router.go mounts them, over the real
// legacyrbac resolver of the escalation fixture.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/adminaccess"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/middleware"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/v2/admin"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/auth"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/legacyrbac"
)

const (
	accessReader    = "r010b-reader@autotest.local"
	accessAuditor   = "r010b-auditor@autotest.local"
	accessNobody    = "r010b-nobody@autotest.local"
	accessRoles     = "/admin/access/roles"
	accessHistory   = "/admin/access/history"
	accessTokens    = "/admin/access/tokens"
	accessDirectory = "/admin/auth_users/administration?limit=100&offset=0"
)

// gatedAccessRouter mounts the two Access reads and the user directory with
// the gates of internal/api/router.go.
func (e escalationEnvironment) gatedAccessRouter(email string) chi.Router {
	id := fmt.Sprint(e.ids[email])
	principal := auth.User{ID: id, UserID: id, CredentialKind: auth.CredentialKindSession}
	resolver := legacyrbac.NewPostgresResolver(e.pool)
	handler := admin.NewHandler(e.pool, admin.WithPermissionResolver(resolver))
	central := func(permissions ...string) func(http.Handler) http.Handler {
		return middleware.RequireCentralPermissions(resolver, auth.PermissionModeAdministration, permissions...)
	}
	router := chi.NewRouter()
	router.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			next.ServeHTTP(w, r.WithContext(auth.ContextWithUser(r.Context(), principal)))
		})
	})
	accessGate := central(adminaccess.AccessRolesView, adminaccess.AdminAuthUsers, adminaccess.ModesUsers)
	router.With(accessGate).Get(accessRoles, handler.AccessRoles)
	router.With(accessGate).Get(accessHistory, handler.AccessHistory)
	router.With(accessGate).Get(accessTokens, handler.AccessTokens)
	router.With(central(adminaccess.AdminAuthUsers)).Get("/admin/auth_users/{mode}", handler.AuthUsers)
	return router
}

// newAccessEnvironment is the escalation fixture plus a holder of a role whose
// only administration permission is access.roles.view, a security auditor,
// and an account with no administration permission.
func newAccessEnvironment(t *testing.T) escalationEnvironment {
	t.Helper()
	env := newEscalationEnvironment(t)
	seedBuiltInRoles(t, env, adminaccess.RoleSecurityAuditor, adminaccess.RoleLLMManager)
	for _, statement := range []string{
		`INSERT INTO public.auth_core__role (name, mode) VALUES ('access_reader', 'administration')`,
		`INSERT INTO public.auth_core__role_permission (role_id, permission)
			SELECT id, 'access.roles.view' FROM public.auth_core__role
			WHERE name = 'access_reader' AND mode = 'administration'`,
	} {
		if _, err := env.pool.Exec(t.Context(), statement); err != nil {
			t.Fatalf("seed %q: %v", statement, err)
		}
	}
	for _, email := range []string{accessReader, accessAuditor, accessNobody} {
		if _, err := env.pool.Exec(t.Context(),
			`INSERT INTO public.auth_core__user (email, name) VALUES ($1, $1)`, email); err != nil {
			t.Fatalf("create %s: %v", email, err)
		}
		env.ids[email] = userID(t, env.pool, email)
	}
	giveRole(t, env, accessReader, "access_reader")
	giveRole(t, env, accessAuditor, adminaccess.RoleSecurityAuditor)
	return env
}

type accessHolder struct {
	UserID int    `json:"user_id"`
	Email  string `json:"email"`
	Roles  []struct {
		Name    string  `json:"name"`
		BuiltIn bool    `json:"built_in"`
		Marker  *string `json:"marker"`
	} `json:"roles"`
}

type accessGrant struct {
	UserID      int       `json:"user_id"`
	UserEmail   *string   `json:"user_email"`
	RoleName    string    `json:"role_name"`
	RoleMode    string    `json:"role_mode"`
	Action      string    `json:"action"`
	ActorUserID *int      `json:"actor_user_id"`
	ActorEmail  *string   `json:"actor_email"`
	OccurredAt  time.Time `json:"occurred_at"`
	Source      string    `json:"source"`
}

func decodeRows[T any](t *testing.T, body []byte) (int, []T) {
	t.Helper()
	var page struct {
		Total int `json:"total"`
		Rows  []T `json:"rows"`
	}
	if err := json.Unmarshal(body, &page); err != nil {
		t.Fatalf("decode %s: %v", body, err)
	}
	return page.Total, page.Rows
}

// Acceptance: the history route returns who granted a role, when and from
// which source, newest first, and filters by user and by role.
func TestTheAccessHistoryNamesWhoGrantedARoleWhenAndFromWhere(t *testing.T) {
	env := newAccessEnvironment(t)
	plain := env.ids[escalationPlainTarget]
	actor := env.ids[escalationSuper]
	before := time.Now().Add(-time.Minute)
	expectStatus(t, adminDo(t, env.asBrowser(escalationSuper), http.MethodPost, usersAction,
		forUser(setRoles(adminaccess.RoleLLMManager), plain)), http.StatusOK)
	expectStatus(t, adminDo(t, env.as(escalationSuper), http.MethodPost, usersAction,
		forUser(setRoles(), plain)), http.StatusOK)

	recorder := adminDo(t, env.gatedAccessRouter(accessAuditor), http.MethodGet,
		fmt.Sprintf("%s?user_id=%d", accessHistory, plain), nil)
	expectStatus(t, recorder, http.StatusOK)
	total, rows := decodeRows[accessGrant](t, recorder.Body.Bytes())
	if total != 2 || len(rows) != 2 {
		t.Fatalf("history = %d %+v, want two rows", total, rows)
	}
	// Newest first: the revocation, then the grant.
	revoked, granted := rows[0], rows[1]
	if revoked.Action != "revoked" || granted.Action != "granted" {
		t.Fatalf("order = %s, %s; want revoked then granted", revoked.Action, granted.Action)
	}
	if granted.RoleName != adminaccess.RoleLLMManager || granted.RoleMode != "administration" ||
		granted.UserID != plain || granted.UserEmail == nil || *granted.UserEmail != escalationPlainTarget {
		t.Fatalf("granted row = %+v", granted)
	}
	if granted.ActorUserID == nil || *granted.ActorUserID != actor ||
		granted.ActorEmail == nil || *granted.ActorEmail != escalationSuper {
		t.Fatalf("granted row names actor %v %v, want %d %s", granted.ActorUserID, granted.ActorEmail, actor, escalationSuper)
	}
	if granted.Source != "console" || revoked.Source != "console" {
		t.Fatalf("sources = %s, %s; want console for both browser sessions", granted.Source, revoked.Source)
	}
	if granted.OccurredAt.Before(before) || granted.OccurredAt.After(time.Now().Add(time.Minute)) {
		t.Fatalf("granted at %s, want about now", granted.OccurredAt)
	}

	// The role filter, and a page of one.
	recorder = adminDo(t, env.gatedAccessRouter(accessAuditor), http.MethodGet,
		fmt.Sprintf("%s?role=%s&limit=1", accessHistory, adminaccess.RoleLLMManager), nil)
	expectStatus(t, recorder, http.StatusOK)
	total, rows = decodeRows[accessGrant](t, recorder.Body.Bytes())
	if total != 2 || len(rows) != 1 || rows[0].Action != "revoked" {
		t.Fatalf("role filter = %d %+v, want total 2 and the newest row", total, rows)
	}
	recorder = adminDo(t, env.gatedAccessRouter(accessAuditor), http.MethodGet,
		accessHistory+"?role=no_such_role", nil)
	expectStatus(t, recorder, http.StatusOK)
	if total, rows = decodeRows[accessGrant](t, recorder.Body.Bytes()); total != 0 || len(rows) != 0 {
		t.Fatalf("an unknown role lists %d %+v", total, rows)
	}
	for _, bad := range []string{"?limit=0", "?limit=201", "?offset=-1", "?user_id=x"} {
		expectStatus(t, adminDo(t, env.gatedAccessRouter(accessAuditor), http.MethodGet, accessHistory+bad, nil),
			http.StatusBadRequest)
	}
}

// Acceptance: a holder of only access.roles.view (the security auditor) reads
// both routes and not the user directory; a caller with no administration
// permission reads neither.
func TestTheAccessReadsAreForTheAuditorAndNotTheDirectory(t *testing.T) {
	env := newAccessEnvironment(t)
	for _, email := range []string{accessReader, accessAuditor} {
		reader := env.gatedAccessRouter(email)
		expectStatus(t, adminDo(t, reader, http.MethodGet, accessRoles, nil), http.StatusOK)
		expectStatus(t, adminDo(t, reader, http.MethodGet, accessHistory, nil), http.StatusOK)
		expectStatus(t, adminDo(t, reader, http.MethodGet, accessDirectory, nil), http.StatusForbidden)
	}

	nobody := env.gatedAccessRouter(accessNobody)
	expectStatus(t, adminDo(t, nobody, http.MethodGet, accessRoles, nil), http.StatusForbidden)
	expectStatus(t, adminDo(t, nobody, http.MethodGet, accessHistory, nil), http.StatusForbidden)

	// The names access.roles.view narrows open the reads too.
	for _, email := range []string{escalationUsersAdmin, escalationModes} {
		expectStatus(t, adminDo(t, env.gatedAccessRouter(email), http.MethodGet, accessRoles, nil), http.StatusOK)
	}
}

// Acceptance: the holders route lists a user with two roles once with both,
// and lists nobody who holds no administration role.
func TestTheHoldersRouteListsEachHolderOnceAndNobodyElse(t *testing.T) {
	env := newAccessEnvironment(t)
	giveRole(t, env, escalationPlainTarget, adminaccess.RoleLLMManager)
	giveRole(t, env, escalationPlainTarget, "empty_role")
	// A built-in role is recorded in the seed record; a custom one is not.
	if _, err := env.pool.Exec(t.Context(), `
INSERT INTO elitea_auth.admin_role_seeds (role_id, name, mode, marker, seeded_by)
SELECT id, name, mode, 'declared', 'access_read_test' FROM public.auth_core__role WHERE mode = 'administration' AND name = $1`,
		adminaccess.RoleLLMManager); err != nil {
		t.Fatalf("record llm_manager as built-in: %v", err)
	}

	recorder := adminDo(t, env.gatedAccessRouter(accessAuditor), http.MethodGet, accessRoles, nil)
	expectStatus(t, recorder, http.StatusOK)
	total, holders := decodeRows[accessHolder](t, recorder.Body.Bytes())
	if total != len(holders) {
		t.Fatalf("total %d for %d rows", total, len(holders))
	}
	var plainRows []accessHolder
	for _, holder := range holders {
		if holder.UserID == env.ids[escalationPlainTarget] {
			plainRows = append(plainRows, holder)
		}
		if holder.UserID == env.ids[accessNobody] {
			t.Fatalf("an account with no administration role is listed: %+v", holder)
		}
		if len(holder.Roles) == 0 {
			t.Fatalf("a holder with no role is listed: %+v", holder)
		}
	}
	if len(plainRows) != 1 {
		t.Fatalf("the two-role holder is listed %d times: %+v", len(plainRows), plainRows)
	}
	plain := plainRows[0]
	names := []string{}
	for _, role := range plain.Roles {
		names = append(names, role.Name)
		switch role.Name {
		case adminaccess.RoleLLMManager:
			if !role.BuiltIn || role.Marker == nil || *role.Marker != "declared" {
				t.Fatalf("llm_manager = %+v, want built-in and declared", role)
			}
		case "empty_role":
			if role.BuiltIn || role.Marker != nil {
				t.Fatalf("empty_role = %+v, want custom", role)
			}
		}
	}
	if !slices.Equal(names, []string{"empty_role", adminaccess.RoleLLMManager}) || plain.Email != escalationPlainTarget {
		t.Fatalf("holder = %+v, want both roles by name", plain)
	}
}
