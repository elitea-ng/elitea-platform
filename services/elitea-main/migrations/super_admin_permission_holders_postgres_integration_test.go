package migrations_test

// Who holds admin.auth.users.super_admin (ADR-0059 decision 7, "Checks by role
// name", story R0.13).
//
// The guard on giving and taking the super_admin role tests the permission
// admin.auth.users.super_admin, not the caller's role name. That is the name
// test made exact only while no other role holds the permission, so this pins
// it on both database fixtures, fresh (001_initial.sql plus the shared corpus)
// and legacy (001_initial.sql with the central roles of
// testdata/postgres/legacy-rbac-matrix.json, then the corpus):
//
//   - no role other than the administration super_admin holds it, and on the
//     legacy database also the administration system role, which is never
//     assigned (R0.8a, R0.11), and the legacy roles of the same two names in
//     the default and developer modes, which the guard does not read;
//   - the Roles matrix adds it to no other role, for a global administrator
//     too (R0.8a, CodeReservedPermission);
//   - the demotion guard refuses a caller without the permission, and admits
//     a caller whose role has another name and holds it.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/adminaccess"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/v2/admin"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/auth"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/legacyrbac"
)

func TestOnlySuperAdminHoldsTheSuperAdminPermission(t *testing.T) {
	for name, fixture := range map[string]struct {
		pool func(*testing.T) *pgxpool.Pool
		want []string
	}{
		"fresh":  {newFreshDatabase, []string{"administration:super_admin"}},
		// The legacy platform also granted it to its super_admin and system
		// roles of the default and developer modes. The guard reads the
		// administration mode only (adminaccess.Caller.SuperAdmin), and every
		// holder is named super_admin or system.
		"legacy": {newLegacyDatabase, []string{
			"administration:super_admin", "administration:system",
			"default:super_admin", "default:system", "developer:super_admin", "developer:system",
		}},
	} {
		t.Run(name, func(t *testing.T) {
			pool := fixture.pool(t)
			if got := superAdminPermissionHolders(t, pool); !slices.Equal(got, fixture.want) {
				t.Fatalf("holders of %s = %v, want %v", adminaccess.SuperAdminPermission, got, fixture.want)
			}

			seedSuperAdminCallers(t, pool)
			// The matrix adds it to no other role, for a global
			// administrator too.
			matrix := matrixRead(t, pool, "r013-super@autotest.local")
			for i, row := range matrix {
				if row["name"] == adminaccess.SuperAdminPermission {
					matrix[i]["r013_custom"] = true
				}
			}
			status, body := matrixSave(t, pool, "r013-super@autotest.local", matrix)
			if status != http.StatusForbidden || body["code"] != adminaccess.CodeReservedPermission {
				t.Fatalf("adding %s to a custom role: %d %v, want 403 %s",
					adminaccess.SuperAdminPermission, status, body, adminaccess.CodeReservedPermission)
			}
			if got := superAdminPermissionHolders(t, pool); !slices.Equal(got, fixture.want) {
				t.Fatalf("after the refused save the holders are %v, want %v", got, fixture.want)
			}

			// The demotion guard tests the permission: admin does not hold
			// it, so taking super_admin is refused.
			var target int
			queryRow(t, pool, `SELECT id FROM public.auth_core__user WHERE email = 'r013-target@autotest.local'`, &target)
			status, body = postJSON(t, assignmentAPI(t, pool, "r013-admin@autotest.local"),
				"/admin/auth_users/administration",
				map[string]any{"action": "set_admin_role", "user_id": target, "role_names": []string{}})
			if status != http.StatusForbidden || body["code"] != adminaccess.CodeSuperAdminRequired {
				t.Fatalf("admin demoting a super_admin: %d %v, want 403 %s",
					status, body, adminaccess.CodeSuperAdminRequired)
			}
			// A caller whose role has another name and carries the
			// permission (written by SQL here: no API adds it) passes the
			// guard. The name super_admin is not what it tests.
			execSQL(t, pool, `INSERT INTO public.auth_core__role_permission (role_id, permission)
				SELECT id, 'admin.auth.users.super_admin' FROM public.auth_core__role
				WHERE mode = 'administration' AND name = 'r013_owner'`)
			status, body = postJSON(t, assignmentAPI(t, pool, "r013-owner@autotest.local"),
				"/admin/auth_users/administration",
				map[string]any{"action": "set_admin_role", "user_id": target, "role_names": []string{}})
			if status != http.StatusOK {
				t.Fatalf("a holder of the permission under another role name demoting a super_admin: %d %v, want 200",
					status, body)
			}
		})
	}
}

func newFreshDatabase(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pool := newEmptyDatabase(t)
	applyBootstrapSchema(t, pool)
	applySharedCorpus(t, pool)
	return pool
}

func newLegacyDatabase(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pool := newEmptyDatabase(t)
	applyBootstrapSchema(t, pool)
	loadLegacyRBACMatrix(t, pool)
	applySharedCorpus(t, pool)
	return pool
}

// superAdminPermissionHolders lists `mode:name` of every role, central or
// project, that holds the permission, sorted.
func superAdminPermissionHolders(t *testing.T, pool *pgxpool.Pool) []string {
	t.Helper()
	rows, err := pool.Query(t.Context(), `
SELECT role.mode || ':' || role.name
FROM public.auth_core__role role
JOIN public.auth_core__role_permission grant_row ON grant_row.role_id = role.id
WHERE grant_row.permission = $1
UNION
SELECT 'project ' || override.project_id || ':' || project_role.name
FROM public.auth_core__project_role_permission override
JOIN public.auth_core__project_role project_role ON project_role.id = override.role_id
WHERE override.permission = $1
ORDER BY 1`, adminaccess.SuperAdminPermission)
	if err != nil {
		t.Fatalf("read the holders: %v", err)
	}
	holders, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatalf("read the holders: %v", err)
	}
	return holders
}

// seedSuperAdminCallers makes a super_admin, an admin, a holder of the
// custom role r013_owner (no permission yet), a custom role r013_custom and
// a super_admin target.
func seedSuperAdminCallers(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	execSQL(t, pool, `INSERT INTO public.auth_core__role (name, mode) VALUES
		('r013_owner', 'administration'), ('r013_custom', 'administration')`)
	execSQL(t, pool, `INSERT INTO public.auth_core__user (email, name) VALUES
		('r013-super@autotest.local', 'Super'), ('r013-admin@autotest.local', 'Admin'),
		('r013-owner@autotest.local', 'Owner'), ('r013-target@autotest.local', 'Target')`)
	execSQL(t, pool, `INSERT INTO public.auth_core__user_role (user_id, role_id)
		SELECT account.id, role.id FROM public.auth_core__user account
		JOIN (VALUES ('r013-super@autotest.local', 'super_admin'), ('r013-admin@autotest.local', 'admin'),
		             ('r013-owner@autotest.local', 'r013_owner'), ('r013-owner@autotest.local', 'admin'),
		             ('r013-target@autotest.local', 'super_admin'))
		  AS pair(email, role_name) ON pair.email = account.email
		JOIN public.auth_core__role role ON role.name = pair.role_name AND role.mode = 'administration'`)
}

const centralMatrixPath = "/admin/permissions/administration/administration"

func matrixRouter(t *testing.T, pool *pgxpool.Pool, email string) http.Handler {
	t.Helper()
	var id int
	queryRow(t, pool, fmt.Sprintf(`SELECT id FROM public.auth_core__user WHERE email = '%s'`, email), &id)
	principal := auth.User{ID: fmt.Sprint(id), UserID: fmt.Sprint(id), CredentialKind: auth.CredentialKindSession}
	handler := admin.NewHandler(pool, admin.WithPermissionResolver(legacyrbac.NewPostgresResolver(pool)))
	router := chi.NewRouter()
	router.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			next.ServeHTTP(w, r.WithContext(auth.ContextWithUser(r.Context(), principal)))
		})
	})
	router.Get("/admin/permissions/{scope}/{mode}", handler.AdminPermissions)
	router.Put("/admin/permissions/{scope}/{mode}", handler.AdminPermissionsSave)
	return router
}

func matrixRead(t *testing.T, pool *pgxpool.Pool, email string) []map[string]any {
	t.Helper()
	recorder := httptest.NewRecorder()
	matrixRouter(t, pool, email).ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, centralMatrixPath, nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("read the matrix: %d %s", recorder.Code, recorder.Body.String())
	}
	var body struct {
		Rows []map[string]any `json:"rows"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode the matrix: %v", err)
	}
	return body.Rows
}

func matrixSave(t *testing.T, pool *pgxpool.Pool, email string, rows []map[string]any) (int, map[string]any) {
	t.Helper()
	encoded, err := json.Marshal(rows)
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPut, centralMatrixPath, bytes.NewReader(encoded))
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	matrixRouter(t, pool, email).ServeHTTP(recorder, request)
	decoded := map[string]any{}
	_ = json.Unmarshal(recorder.Body.Bytes(), &decoded)
	return recorder.Code, decoded
}
