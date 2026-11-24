package eliteacore_test

// Central project membership (ADR-0059 R0.6a, issue 2495): the administration
// member routes, for a caller who holds the built-in project administrator set
// in the administration mode and is not a member of the project.
//
// The caller's permissions come from a resolver that answers the role's set
// centrally and nothing in a project, which is what a non-member resolves.
// The rule against the REAL resolver is in
// users_central_self_postgres_integration_test.go. The route gates are proved
// on the composed router (internal/api/central_project_admin_gates_test.go).

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/adminaccess"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/v2/eliteacore"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/audit"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/auth"
)

const centralCallerEmail = "e2e-central@autotest.local"

// centralResolver answers `central` in the administration mode and nothing in
// any project: the caller is a member of none.
type centralResolver struct {
	userID  int64
	central []string
}

func (c centralResolver) ResolvePermissions(
	_ context.Context, _ auth.User, mode string, _ string,
) (auth.PermissionResolution, error) {
	if mode == adminaccess.ModeAdministration {
		return auth.PermissionResolution{UserID: c.userID, Permissions: slices.Clone(c.central)}, nil
	}
	return auth.PermissionResolution{}, auth.ErrPermissionDenied
}

func projectAdministratorSet(t *testing.T) []string {
	t.Helper()
	role, ok := adminaccess.LookupBuiltInRole(adminaccess.RoleProjectAdministrator)
	if !ok {
		t.Fatal("project_administrator is not a built-in role")
	}
	return role.Set()
}

// centralEnvironment seeds the escalation fixture and one account that is a
// member of no project.
func centralEnvironment(t *testing.T) (*pgxpool.Pool, int) {
	t.Helper()
	pool := newUsersWritePostgresPool(t)
	prepareEscalationFixture(t, pool)
	var id int
	if err := pool.QueryRow(context.Background(),
		`INSERT INTO public.auth_core__user (email, name) VALUES ($1, 'Central') RETURNING id`,
		centralCallerEmail).Scan(&id); err != nil {
		t.Fatalf("seed the central caller: %v", err)
	}
	return pool, id
}

// auditedRouter is usersWriteRouterAs with the audit middleware's annotation
// slot on every request, so a test reads the record the middleware would hand
// to the audit recorder.
func auditedRouter(handler *eliteacore.Handler, principal auth.User) (chi.Router, func() audit.Annotation) {
	var slot *audit.AnnotationSlot
	inner := usersWriteRouterAs(handler, principal)
	router := chi.NewRouter()
	router.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			var ctx context.Context
			ctx, slot = audit.ContextWithAnnotationSlot(r.Context())
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	})
	router.Mount("/", inner)
	return router, func() audit.Annotation {
		annotation, _ := slot.Read()
		return annotation
	}
}

func centralRouter(pool *pgxpool.Pool, id int, central []string) (chi.Router, func() audit.Annotation) {
	handler := eliteacore.NewHandler(pool,
		eliteacore.WithPermissionResolver(centralResolver{userID: int64(id), central: central}))
	return auditedRouter(handler, auth.User{ID: fmt.Sprint(id), UserID: fmt.Sprint(id)})
}

var centralMembersPath = fmt.Sprintf("/admin/users/administration/%d", usersWriteProjectID)

// A holder of the project administrator set adds another person to a project
// it is not a member of, and the audit record says it was a central override.
func TestAProjectAdministratorAddsAnotherPersonToAProjectItIsNotAMemberOf(t *testing.T) {
	pool, id := centralEnvironment(t)
	router, recorded := centralRouter(pool, id, projectAdministratorSet(t))

	recorder := usersWriteDo(t, router, http.MethodPost, centralMembersPath,
		map[string]any{"emails": []string{"e2e-newcomer@autotest.local"}, "roles": []string{"admin"}})
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d body %s, want 200", recorder.Code, recorder.Body.String())
	}
	var roles int
	if err := pool.QueryRow(context.Background(), `
SELECT COUNT(*) FROM public.auth_core__project_user_role assignment
JOIN public.auth_core__user account ON account.id = assignment.user_id
WHERE assignment.project_id = $1 AND account.email = 'e2e-newcomer@autotest.local'`,
		usersWriteProjectID).Scan(&roles); err != nil {
		t.Fatal(err)
	}
	if roles != 1 {
		t.Fatalf("the newcomer holds %d project roles, want 1", roles)
	}
	if !recorded().CentralOverride {
		t.Fatal("the audit record of a write into a project the caller is not a member of has no central_override")
	}
}

// A member of the project who writes through the same route is not a central
// override.
func TestAMemberWritingThroughTheCentralRouteIsNoOverride(t *testing.T) {
	pool, _ := centralEnvironment(t)
	_, adminID := escalationRouter(t, pool, "e2e-admin@autotest.local")
	_, memberID := escalationRouter(t, pool, "e2e-member@autotest.local")
	router, recorded := centralRouter(pool, adminID, []string{adminaccess.ProjectsMembersManage})

	recorder := usersWriteDo(t, router, http.MethodPut, centralMembersPath,
		map[string]any{"userId": fmt.Sprint(memberID), "roles": []string{"viewer"}})
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d body %s, want 200", recorder.Code, recorder.Body.String())
	}
	if recorded().CentralOverride {
		t.Fatal("a member's write is marked central_override")
	}
}

// A standing global administrator adds itself to a project through the member
// route; anyone else gets a refusal that names the two ways in.
func TestASuperAdminAddsItselfAndAnyoneElseIsToldTheWaysIn(t *testing.T) {
	pool, id := centralEnvironment(t)

	super, _ := centralRouter(pool, id, []string{adminaccess.SuperAdminPermission})
	recorder := usersWriteDo(t, super, http.MethodPost, centralMembersPath,
		map[string]any{"emails": []string{centralCallerEmail}, "roles": []string{"admin"}})
	if recorder.Code != http.StatusOK {
		t.Fatalf("super_admin adding itself: status = %d body %s, want 200", recorder.Code, recorder.Body.String())
	}
	if roles := projectRolesOf(t, pool, id); fmt.Sprint(roles) != "[admin]" {
		t.Fatalf("super_admin's roles after adding itself = %v, want [admin]", roles)
	}

	pool, id = centralEnvironment(t)
	router, recorded := centralRouter(pool, id, projectAdministratorSet(t))
	refused := usersWriteDo(t, router, http.MethodPost, centralMembersPath,
		map[string]any{"emails": []string{centralCallerEmail}, "roles": []string{"admin"}})
	if refused.Code != http.StatusForbidden {
		t.Fatalf("status = %d body %s, want 403", refused.Code, refused.Body.String())
	}
	var body struct {
		Code            string   `json:"code"`
		Error           string   `json:"error"`
		WaysIn          []string `json:"ways_in"`
		OperatorCommand string   `json:"operator_command"`
		ProjectID       int      `json:"project_id"`
	}
	if err := json.Unmarshal(refused.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode %q: %v", refused.Body.String(), err)
	}
	if body.Code != adminaccess.CodeSelfEscalation {
		t.Fatalf("code = %q, want %s", body.Code, adminaccess.CodeSelfEscalation)
	}
	if !slices.Equal(body.WaysIn, []string{adminaccess.WayInProjectMember, adminaccess.WayInOperatorCommand}) {
		t.Fatalf("ways_in = %v, want the project member and the operator command", body.WaysIn)
	}
	if body.OperatorCommand != "elitea-main admin-role" || body.ProjectID != usersWriteProjectID {
		t.Fatalf("operator_command = %q, project_id = %d", body.OperatorCommand, body.ProjectID)
	}
	if roles := projectRolesOf(t, pool, id); len(roles) != 0 {
		t.Fatalf("the refused caller holds %v", roles)
	}
	if recorded().CentralOverride {
		t.Fatal("a refused write is marked central_override")
	}
}

// A holder of only projects.members.manage removes another member through the
// central DELETE. Every project role of the member goes.
func TestAMembersManageHolderRemovesAMemberCentrally(t *testing.T) {
	pool, id := centralEnvironment(t)
	_, memberID := escalationRouter(t, pool, "e2e-member@autotest.local")
	router, recorded := centralRouter(pool, id, []string{adminaccess.ProjectsMembersManage})

	recorder := usersWriteDo(t, router, http.MethodDelete,
		fmt.Sprintf("%s?id[]=%d", centralMembersPath, memberID), nil)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d body %s, want 200", recorder.Code, recorder.Body.String())
	}
	if roles := projectRolesOf(t, pool, memberID); len(roles) != 0 {
		t.Fatalf("the removed member still holds %v", roles)
	}
	if !recorded().CentralOverride {
		t.Fatal("a central removal by a non-member has no central_override")
	}
}
