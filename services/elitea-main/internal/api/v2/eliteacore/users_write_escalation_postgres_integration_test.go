package eliteacore_test

// The no-escalation rule on project membership (issue 2488). The caller's
// permissions come from the REAL legacyrbac resolver over the fixture, so the
// rule is proved against what the route gates resolve, not against a stub.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/v2/eliteacore"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/auth"
	dbschema "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/db/schema"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/legacyrbac"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/rolepolicy"
)

// superAdminCaller holds admin.auth.users.super_admin in every mode, so the
// fixtures that are not about the rule are exempt from it.
type superAdminCaller struct{}

func (superAdminCaller) ResolvePermissions(context.Context, auth.User, string, string) (auth.PermissionResolution, error) {
	return auth.PermissionResolution{UserID: 1, Permissions: []string{rolepolicy.SuperAdminPermission}}, nil
}

// prepareEscalationFixture gives the project roles their own grants: `admin`
// holds three permissions, `editor` one of them.
func prepareEscalationFixture(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	prepareUsersWriteFixture(t, pool)
	// The resolver refuses a project it cannot find, so the project row exists.
	if _, err := pool.Exec(context.Background(), dbschema.CentryProjectsBaselineSQLCProjection); err != nil {
		t.Fatalf("apply the centry projects baseline: %v", err)
	}
	for _, statement := range []string{
		fmt.Sprintf(`INSERT INTO centry.project (id, name, owner_id, keycloak_groups, create_success)
			VALUES (%d, 'escalation', 1, '{}', true) ON CONFLICT (id) DO NOTHING`, usersWriteProjectID),
		fmt.Sprintf(`INSERT INTO public.auth_core__project_role_permission (project_id, role_id, permission)
			SELECT role.project_id, role.id, grant_row.permission
			FROM public.auth_core__project_role role
			JOIN (VALUES ('admin', 'configuration.users.users.edit'), ('admin', 'configuration.users.users.delete'),
			             ('admin', 'models.secrets.view'), ('editor', 'configuration.users.users.edit'),
			             ('editor', 'configuration.users.users.delete'))
			  AS grant_row(role_name, permission) ON grant_row.role_name = role.name
			WHERE role.project_id = %d`, usersWriteProjectID),
	} {
		if _, err := pool.Exec(context.Background(), statement); err != nil {
			t.Fatalf("seed %q: %v", statement, err)
		}
	}
}

func escalationRouter(t *testing.T, pool *pgxpool.Pool, email string) (chi.Router, int) {
	t.Helper()
	var id int
	if err := pool.QueryRow(context.Background(),
		`SELECT id FROM public.auth_core__user WHERE email = $1`, email).Scan(&id); err != nil {
		t.Fatalf("look up %s: %v", email, err)
	}
	handler := eliteacore.NewHandler(pool,
		eliteacore.WithPermissionResolver(legacyrbac.NewPostgresResolver(pool)))
	return usersWriteRouterAs(handler, auth.User{ID: fmt.Sprint(id), UserID: fmt.Sprint(id)}), id
}

func projectRolesOf(t *testing.T, pool *pgxpool.Pool, userID int) []string {
	t.Helper()
	rows, err := pool.Query(context.Background(), `
SELECT role.name FROM public.auth_core__project_user_role assignment
JOIN public.auth_core__project_role role ON role.id = assignment.role_id
WHERE assignment.project_id = $1 AND assignment.user_id = $2`, usersWriteProjectID, userID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	names := []string{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatal(err)
		}
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func refusalCode(t *testing.T, body []byte) string {
	t.Helper()
	var decoded map[string]any
	if err := json.Unmarshal(body, &decoded); err != nil {
		t.Fatalf("decode %q: %v", body, err)
	}
	code, _ := decoded["code"].(string)
	return code
}

func TestAProjectEditorCannotPromoteItselfToProjectAdmin(t *testing.T) {
	pool := newUsersWritePostgresPool(t)
	prepareEscalationFixture(t, pool)
	router, memberID := escalationRouter(t, pool, "e2e-member@autotest.local")

	recorder := usersWriteDo(t, router, http.MethodPut, fmt.Sprintf("/admin/users/default/%d", usersWriteProjectID),
		map[string]any{"userId": fmt.Sprint(memberID), "roles": []string{"admin"}})
	if recorder.Code != http.StatusForbidden || refusalCode(t, recorder.Body.Bytes()) != rolepolicy.CodePermissionNotHeld {
		t.Fatalf("status = %d body %s, want 403 %s", recorder.Code, recorder.Body.String(), rolepolicy.CodePermissionNotHeld)
	}
	if roles := projectRolesOf(t, pool, memberID); fmt.Sprint(roles) != "[editor]" {
		t.Fatalf("roles after the refusal = %v, want [editor]", roles)
	}
}

func TestAProjectEditorCannotRemoveAProjectAdmin(t *testing.T) {
	pool := newUsersWritePostgresPool(t)
	prepareEscalationFixture(t, pool)
	router, _ := escalationRouter(t, pool, "e2e-member@autotest.local")
	_, adminID := escalationRouter(t, pool, "e2e-admin@autotest.local")

	recorder := usersWriteDo(t, router, http.MethodDelete,
		fmt.Sprintf("/admin/users/default/%d?id=%d", usersWriteProjectID, adminID), nil)
	if recorder.Code != http.StatusForbidden {
		t.Fatalf("status = %d body %s, want 403", recorder.Code, recorder.Body.String())
	}
	if roles := projectRolesOf(t, pool, adminID); fmt.Sprint(roles) != "[admin]" {
		t.Fatalf("roles after the refusal = %v, want [admin]", roles)
	}
}

func TestAProjectAdminStillChangesRoles(t *testing.T) {
	pool := newUsersWritePostgresPool(t)
	prepareEscalationFixture(t, pool)
	router, _ := escalationRouter(t, pool, "e2e-admin@autotest.local")
	_, memberID := escalationRouter(t, pool, "e2e-member@autotest.local")

	recorder := usersWriteDo(t, router, http.MethodPut,
		fmt.Sprintf("/admin/users/default/%d", usersWriteProjectID),
		map[string]any{"userId": fmt.Sprint(memberID), "roles": []string{"admin"}})
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d body %s, want 200", recorder.Code, recorder.Body.String())
	}
	if roles := projectRolesOf(t, pool, memberID); fmt.Sprint(roles) != "[admin]" {
		t.Fatalf("roles = %v, want [admin]", roles)
	}
}

func TestAnOperatorCannotGiveItsOwnAccountAProjectRole(t *testing.T) {
	pool := newUsersWritePostgresPool(t)
	prepareEscalationFixture(t, pool)
	// The member reaches the ADMINISTRATION route: its gate is central, and
	// the caller is an operator acting on a project.
	router, memberID := escalationRouter(t, pool, "e2e-member@autotest.local")

	recorder := usersWriteDo(t, router, http.MethodPut,
		fmt.Sprintf("/admin/users/administration/%d", usersWriteProjectID),
		map[string]any{"userId": fmt.Sprint(memberID), "roles": []string{"admin"}})
	if recorder.Code != http.StatusForbidden || refusalCode(t, recorder.Body.Bytes()) != rolepolicy.CodeSelfEscalation {
		t.Fatalf("status = %d body %s, want 403 %s", recorder.Code, recorder.Body.String(), rolepolicy.CodeSelfEscalation)
	}

	// Another account is the operator's to place.
	recorder = usersWriteDo(t, router, http.MethodPost,
		fmt.Sprintf("/admin/users/administration/%d", usersWriteProjectID),
		map[string]any{"emails": []string{"e2e-newcomer@autotest.local"}, "roles": []string{"admin"}})
	if recorder.Code != http.StatusOK {
		t.Fatalf("inviting another account: status = %d body %s, want 200", recorder.Code, recorder.Body.String())
	}
}
