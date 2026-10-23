package middleware

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/auth"
)

func TestRequireProjectAccess(t *testing.T) {
	tests := []struct {
		name string
		user auth.User
		row  fakeRow
		want int
	}{
		{name: "member allowed", user: auth.User{ID: "7"}, row: fakeRow{vals: []any{true, true}}, want: http.StatusNoContent},
		{name: "non member forbidden", user: auth.User{ID: "7"}, row: fakeRow{vals: []any{false, true}}, want: http.StatusForbidden},
		// A non-member of a project that does not exist still reads 403. The
		// refusal must not become an enumeration oracle for project ids.
		{name: "non member of an absent project forbidden", user: auth.User{ID: "7"}, row: fakeRow{vals: []any{false, false}}, want: http.StatusForbidden},
		{name: "missing identity unauthorized", want: http.StatusUnauthorized},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			querier := &fakeQuerier{handler: func(string, ...any) pgx.Row { return tc.row }}
			r := chi.NewRouter()
			r.With(requireProjectAccess(querier)).Get("/projects/{projectID}", func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusNoContent)
			})
			req := httptest.NewRequest(http.MethodGet, "/projects/12", nil)
			if tc.user.ID != "" {
				req = req.WithContext(auth.ContextWithUser(req.Context(), tc.user))
			}
			res := httptest.NewRecorder()
			r.ServeHTTP(res, req)
			if res.Code != tc.want {
				t.Fatalf("status = %d, want %d", res.Code, tc.want)
			}
		})
	}
}

// TestRequireProjectAccess_NilPool guards against a nil *pgxpool.Pool being
// boxed into the projectAccessQuerier interface, which would produce a
// non-nil interface value and panic inside pgxpool's Acquire instead of
// returning the intended 503.
func TestRequireProjectAccess_NilPool(t *testing.T) {
	var nilPool *pgxpool.Pool

	r := chi.NewRouter()
	r.With(RequireProjectAccess(nilPool)).Get("/projects/{projectID}", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	req := httptest.NewRequest(http.MethodGet, "/projects/12", nil)
	req = req.WithContext(auth.ContextWithUser(req.Context(), auth.User{ID: "7"}))
	res := httptest.NewRecorder()

	r.ServeHTTP(res, req)

	if res.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d", res.Code, http.StatusServiceUnavailable)
	}
}

// TestRequireProjectAccessAdmitsOnlyTheAdministrationModeSuperAdmin pins the
// role predicate the middleware sends to PostgreSQL.
//
// DEFECT: the central-administrator branch matched on the role NAME alone
// (`role.name = 'super_admin'`). auth_core__role is keyed UNIQUE (name, mode)
// (internal/infra/db/migrations/001_initial.sql:649-650). A database restored
// from a legacy dump carries a `super_admin` role in the `default` and
// `developer` modes as well as `administration`
// (testdata/postgres/legacy-rbac-matrix.json). A holder of one of those roles
// was therefore admitted to EVERY project in the deployment, although the role
// grants no central access. Only the `administration` mode does.
//
// The assertion reads the SQL because the decision is made inside PostgreSQL:
// the middleware receives one boolean and cannot tell which branch produced it.
func TestRequireProjectAccessAdmitsOnlyTheAdministrationModeSuperAdmin(t *testing.T) {
	captured := ""
	querier := &fakeQuerier{handler: func(sql string, _ ...any) pgx.Row {
		captured = sql
		return fakeRow{vals: []any{true, true}}
	}}

	r := chi.NewRouter()
	r.With(requireProjectAccess(querier)).Get("/projects/{projectID}", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	req := httptest.NewRequest(http.MethodGet, "/projects/12", nil)
	req = req.WithContext(auth.ContextWithUser(req.Context(), auth.User{ID: "7"}))
	r.ServeHTTP(httptest.NewRecorder(), req)

	if !strings.Contains(captured, "role.name = 'super_admin'") {
		t.Fatalf("membership query lost its super_admin branch: %s", captured)
	}
	if !strings.Contains(captured, "role.mode = 'administration'") {
		t.Fatalf("membership query matches a super_admin role in ANY mode, so a "+
			"default-mode or developer-mode holder enters every project: %s", captured)
	}
}

// TestRequireProjectAccessAnswers404ForAnAbsentProject covers the second column
// of the same query.
//
// DEFECT: the query asked only "may this user enter?". A central administrator
// is admitted to every project id, including one that does not exist. The
// request therefore reached a handler that built the schema name `p_<id>`.
// That handler failed with SQLSTATE 3F000 (invalid_schema_name). Every such
// handler reported that as a
// generic 500 — for example GET /api/v2/social/feedbacks/default/999999 — so a
// deleted or unknown project was indistinguishable from a broken server.
func TestRequireProjectAccessAnswers404ForAnAbsentProject(t *testing.T) {
	querier := &fakeQuerier{handler: func(string, ...any) pgx.Row {
		// allowed (central administrator), but no project row.
		return fakeRow{vals: []any{true, false}}
	}}

	reached := false
	r := chi.NewRouter()
	r.With(requireProjectAccess(querier)).Get("/projects/{projectID}", func(w http.ResponseWriter, _ *http.Request) {
		reached = true
		w.WriteHeader(http.StatusNoContent)
	})
	req := httptest.NewRequest(http.MethodGet, "/projects/999999", nil)
	req = req.WithContext(auth.ContextWithUser(req.Context(), auth.User{ID: "7"}))
	res := httptest.NewRecorder()
	r.ServeHTTP(res, req)

	if reached {
		t.Fatal("the handler ran for a project that does not exist; it now builds p_999999 and answers 500")
	}
	if res.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", res.Code)
	}
}
