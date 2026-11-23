package migrations_test

// ADR-0059 R0.8a (issue 2497), rule (e): on a legacy-backed database shared
// 0263 gives the administration `system` role a seed row marked
// `unassignable`, and the assignment API then gives that role to nobody, a
// global administrator included.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/adminaccess"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/v2/admin"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/auth"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/legacyrbac"
)

func TestTheUnassignableSystemRoleIsAssignedToNobody(t *testing.T) {
	pool := newLegacyPoolBefore0263(t)
	applySharedCorpus(t, pool)
	if marker := seedRows(t, pool)[adminaccess.RoleSystem]; marker != string(adminaccess.MarkerUnassignable) {
		t.Fatalf("premise: the system seed row is %q, want %q", marker, adminaccess.MarkerUnassignable)
	}

	callers := map[string]string{
		"r08a-super@autotest.local": adminaccess.RoleSuperAdmin,
		"r08a-admin@autotest.local": adminaccess.RoleAdmin,
	}
	execSQL(t, pool, `INSERT INTO public.auth_core__user (email, name) VALUES
		('r08a-super@autotest.local', 'Super'), ('r08a-admin@autotest.local', 'Admin'),
		('r08a-target@autotest.local', 'Target')`)
	for email, role := range callers {
		execSQL(t, pool, fmt.Sprintf(`INSERT INTO public.auth_core__user_role (user_id, role_id)
			SELECT account.id, role.id FROM public.auth_core__user account, public.auth_core__role role
			WHERE account.email = '%s' AND role.mode = 'administration' AND role.name = '%s'`, email, role))
	}
	var target int
	queryRow(t, pool, `SELECT id FROM public.auth_core__user WHERE email = 'r08a-target@autotest.local'`, &target)

	for email, role := range callers {
		router := assignmentAPI(t, pool, email)
		// The modes route accepts any role name of the mode; the seed row
		// refuses this one.
		status, body := postJSON(t, router, "/admin/modes/administration",
			map[string]any{"user_id": target, "mode": "administration", "role": adminaccess.RoleSystem})
		if status != http.StatusForbidden || body["code"] != adminaccess.CodeSetLocked {
			t.Errorf("%s assigning system on the modes route: %d %v, want 403 %s",
				role, status, body, adminaccess.CodeSetLocked)
		}
		// The Users page route has no fixed name list since ADR-0059 R0.8b
		// (issue 2574): it accepts any role of the mode, and the seed row
		// refuses this one there too, in both request forms.
		for _, request := range []map[string]any{
			{"action": "set_admin_role", "user_id": target, "role_name": adminaccess.RoleSystem},
			{"action": "set_admin_role", "user_id": target, "role_names": []string{adminaccess.RoleViewer, adminaccess.RoleSystem}},
		} {
			status, body = postJSON(t, router, "/admin/auth_users/administration", request)
			if status != http.StatusForbidden || body["code"] != adminaccess.CodeSetLocked {
				t.Errorf("%s assigning system on the users route (%v): %d %v, want 403 %s",
					role, request, status, body, adminaccess.CodeSetLocked)
			}
		}
	}
	var held int
	queryRow(t, pool, fmt.Sprintf(`SELECT count(*) FROM public.auth_core__user_role WHERE user_id = %d`, target), &held)
	if held != 0 {
		t.Fatalf("the target holds %d roles after the refusals", held)
	}
}

// assignmentAPI mounts both assignment routes for one caller, resolved by the
// real legacyrbac resolver.
func assignmentAPI(t *testing.T, pool *pgxpool.Pool, email string) http.Handler {
	t.Helper()
	var id int
	queryRow(t, pool, fmt.Sprintf(`SELECT id FROM public.auth_core__user WHERE email = '%s'`, email), &id)
	principal := auth.User{ID: fmt.Sprint(id), UserID: fmt.Sprint(id)}
	handler := admin.NewHandler(pool, admin.WithPermissionResolver(legacyrbac.NewPostgresResolver(pool)))
	router := chi.NewRouter()
	router.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			next.ServeHTTP(w, r.WithContext(auth.ContextWithUser(r.Context(), principal)))
		})
	})
	router.Post("/admin/modes/administration", handler.ModesAssign)
	router.Post("/admin/auth_users/{mode}", handler.AuthUsersAction)
	return router
}

func postJSON(t *testing.T, router http.Handler, path string, body any) (int, map[string]any) {
	t.Helper()
	encoded, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(encoded))
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	decoded := map[string]any{}
	_ = json.Unmarshal(recorder.Body.Bytes(), &decoded)
	return recorder.Code, decoded
}
