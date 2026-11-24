package eliteacore_test

// Central project membership cannot widen the caller's own set (ADR-0059
// R0.6a, issue 2495), against the REAL legacyrbac resolver: the caller holds
// the built-in project_administrator role in the administration mode, so the
// route gates admit it, and the #2555 rule still refuses every path that
// reaches its own account. The bulk invite and the project matrix paths are in
// internal/api/v2/admin/project_administrator_self_postgres_integration_test.go,
// beside the fixture those handlers need.

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/adminaccess"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/v2/eliteacore"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/auth"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/legacyrbac"
)

const projectAdministratorEmail = "e2e-project-administrator@autotest.local"

// projectAdministratorEnvironment seeds the built-in project_administrator
// role in the administration mode, with the registry's set, and one account
// that holds it and no project role.
func projectAdministratorEnvironment(t *testing.T) (*pgxpool.Pool, chi.Router, int) {
	t.Helper()
	pool := newUsersWritePostgresPool(t)
	prepareEscalationFixture(t, pool)
	role, _ := adminaccess.LookupBuiltInRole(adminaccess.RoleProjectAdministrator)
	ctx := context.Background()
	for _, statement := range []string{
		`INSERT INTO public.auth_core__role (name, mode) VALUES ('project_administrator', 'administration')`,
		fmt.Sprintf(`INSERT INTO public.auth_core__role_permission (role_id, permission)
			SELECT role.id, permission FROM public.auth_core__role role,
			unnest(ARRAY['%s']::text[]) AS permission
			WHERE role.name = 'project_administrator' AND role.mode = 'administration'`,
			strings.Join(role.Set(), "','")),
		fmt.Sprintf(`INSERT INTO public.auth_core__user (email, name) VALUES ('%s', 'Project administrator')`,
			projectAdministratorEmail),
		fmt.Sprintf(`INSERT INTO public.auth_core__user_role (user_id, role_id)
			SELECT account.id, role.id FROM public.auth_core__user account, public.auth_core__role role
			WHERE account.email = '%s' AND role.name = 'project_administrator' AND role.mode = 'administration'`,
			projectAdministratorEmail),
	} {
		if _, err := pool.Exec(ctx, statement); err != nil {
			t.Fatalf("seed %q: %v", statement, err)
		}
	}
	_, id := escalationRouter(t, pool, projectAdministratorEmail)
	// A browser session: a credential of another kind resolves no
	// administration set (ADR-0059 R1.2a).
	principal := auth.User{ID: fmt.Sprint(id), UserID: fmt.Sprint(id), CredentialKind: auth.CredentialKindSession}
	resolver := legacyrbac.NewPostgresResolver(pool)
	router := usersWriteRouterAs(eliteacore.NewHandler(pool, eliteacore.WithPermissionResolver(resolver)), principal)
	// The premise: the real resolver gives the caller the role's set
	// centrally, projects.members.manage among it.
	resolution, err := resolver.ResolvePermissions(ctx, principal, adminaccess.ModeAdministration, "")
	if err != nil || !strings.Contains(strings.Join(resolution.Permissions, ","), adminaccess.ProjectsMembersManage) {
		t.Fatalf("premise: the caller resolves %v (%v) centrally, want projects.members.manage", resolution.Permissions, err)
	}
	return pool, router, id
}

func expectSelfEscalation(t *testing.T, status int, body []byte) {
	t.Helper()
	if status != http.StatusForbidden || refusalCode(t, body) != adminaccess.CodeSelfEscalation {
		t.Fatalf("status = %d body %s, want 403 %s", status, body, adminaccess.CodeSelfEscalation)
	}
}

func TestAProjectAdministratorCannotAddItselfAsAdminThroughTheMemberCreate(t *testing.T) {
	pool, router, id := projectAdministratorEnvironment(t)
	recorder := usersWriteDo(t, router, http.MethodPost, centralMembersPath,
		map[string]any{"emails": []string{projectAdministratorEmail}, "roles": []string{"admin"}})
	expectSelfEscalation(t, recorder.Code, recorder.Body.Bytes())
	if roles := projectRolesOf(t, pool, id); len(roles) != 0 {
		t.Fatalf("roles after the refusal = %v, want none", roles)
	}
}

func TestAProjectAdministratorCannotAddItselfThroughTheMemberUpdate(t *testing.T) {
	pool, router, id := projectAdministratorEnvironment(t)
	recorder := usersWriteDo(t, router, http.MethodPut, centralMembersPath,
		map[string]any{"userId": fmt.Sprint(id), "roles": []string{"admin"}})
	expectSelfEscalation(t, recorder.Code, recorder.Body.Bytes())
	if roles := projectRolesOf(t, pool, id); len(roles) != 0 {
		t.Fatalf("roles after the refusal = %v, want none", roles)
	}
}

// Leaving a project widens nothing, so the central removal of its own account
// passes.
func TestAProjectAdministratorRemovesItselfFromAProject(t *testing.T) {
	pool, router, id := projectAdministratorEnvironment(t)
	if _, err := pool.Exec(context.Background(), `
INSERT INTO public.auth_core__project_user_role (project_id, user_id, role_id)
SELECT $1, $2, id FROM public.auth_core__project_role WHERE project_id = $1 AND name = 'editor'`,
		usersWriteProjectID, id); err != nil {
		t.Fatal(err)
	}
	recorder := usersWriteDo(t, router, http.MethodDelete, fmt.Sprintf("%s?id=%d", centralMembersPath, id), nil)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d body %s, want 200", recorder.Code, recorder.Body.String())
	}
	if roles := projectRolesOf(t, pool, id); len(roles) != 0 {
		t.Fatalf("roles after leaving = %v, want none", roles)
	}
}
