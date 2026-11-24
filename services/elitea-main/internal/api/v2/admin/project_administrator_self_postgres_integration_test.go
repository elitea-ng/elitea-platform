package admin_test

// A holder of the built-in project_administrator role cannot widen its own set
// through the bulk invite or the project matrix (ADR-0059 R0.6a, issue 2495).
// The member create and update paths are in
// internal/api/v2/eliteacore/users_central_self_postgres_integration_test.go.
//
// The caller is resolved by the REAL legacyrbac resolver. The project matrix
// route is gated on configuration.roles.user_project_permissions.edit, which
// the role does not hold, so its route gate already refuses it
// (internal/api/central_project_admin_gates_test.go); the test here drives the
// handler to prove the #2555 rule behind that gate holds for the role too.

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/adminaccess"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/audit"
)

const escalationProjectAdministrator = "esc-project-administrator@autotest.local"

// withProjectAdministrator adds the built-in project_administrator role, with
// the registry's set, and one account that holds it.
func withProjectAdministrator(t *testing.T, env escalationEnvironment) int {
	t.Helper()
	role, _ := adminaccess.LookupBuiltInRole(adminaccess.RoleProjectAdministrator)
	for _, statement := range []string{
		`INSERT INTO public.auth_core__role (name, mode) VALUES ('project_administrator', 'administration')
			ON CONFLICT DO NOTHING`,
		fmt.Sprintf(`INSERT INTO public.auth_core__role_permission (role_id, permission)
			SELECT role.id, permission FROM public.auth_core__role role,
			unnest(ARRAY['%s']::text[]) AS permission
			WHERE role.name = 'project_administrator' AND role.mode = 'administration'
			ON CONFLICT DO NOTHING`, strings.Join(role.Set(), "','")),
		fmt.Sprintf(`INSERT INTO public.auth_core__user (email, name) VALUES ('%s', 'Project administrator')`,
			escalationProjectAdministrator),
		fmt.Sprintf(`INSERT INTO public.auth_core__user_role (user_id, role_id)
			SELECT account.id, role.id FROM public.auth_core__user account, public.auth_core__role role
			WHERE account.email = '%s' AND role.name = 'project_administrator' AND role.mode = 'administration'`,
			escalationProjectAdministrator),
	} {
		if _, err := env.pool.Exec(context.Background(), statement); err != nil {
			t.Fatalf("seed %q: %v", statement, err)
		}
	}
	id := userID(t, env.pool, escalationProjectAdministrator)
	env.ids[escalationProjectAdministrator] = id
	return id
}

// audited wraps a router with the audit middleware's annotation slot.
func audited(inner chi.Router) (chi.Router, func() audit.Annotation) {
	var slot *audit.AnnotationSlot
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

func TestAProjectAdministratorCannotAddItselfThroughTheBulkInvite(t *testing.T) {
	env := newEscalationEnvironment(t)
	self := withProjectAdministrator(t, env)
	router, recorded := audited(env.as(escalationProjectAdministrator))
	const path = "/b/admin/invites_bulk/administration"

	expectRefusal(t, adminDo(t, router, http.MethodPost, path, map[string]any{
		"users": []int{self}, "projects": []int{2}, "role": "admin",
	}), adminaccess.CodeSelfEscalation)
	var members int
	if err := env.pool.QueryRow(context.Background(),
		`SELECT COUNT(*) FROM public.auth_core__project_user_role WHERE project_id = 2`).Scan(&members); err != nil {
		t.Fatal(err)
	}
	if members != 0 {
		t.Fatalf("a refused batch wrote %d memberships, want none", members)
	}

	// Another account goes in, and the write is a central override: the
	// caller is not a member of project 2.
	expectStatus(t, adminDo(t, router, http.MethodPost, path, map[string]any{
		"users": []int{env.ids[escalationPlainTarget]}, "projects": []int{2}, "role": "admin",
	}), http.StatusOK)
	if !recorded().CentralOverride {
		t.Fatal("a bulk invite into a project the caller is not a member of has no central_override")
	}
}

func TestAProjectAdministratorCannotWidenItselfThroughTheProjectMatrix(t *testing.T) {
	env := newEscalationEnvironment(t)
	self := withProjectAdministrator(t, env)
	// A viewer of the personal project 3, whose viewer has its own grant.
	if _, err := env.pool.Exec(context.Background(), `
INSERT INTO public.auth_core__project_user_role (project_id, user_id, role_id)
SELECT 3, $1, id FROM public.auth_core__project_role WHERE project_id = 3 AND name = 'viewer'`, self); err != nil {
		t.Fatal(err)
	}
	router := env.as(escalationProjectAdministrator)
	const path = "/p/admin/user_project_permissions/administration"

	expectRefusal(t, adminDo(t, router, http.MethodPut, path,
		map[string]any{"viewer": []string{"models.gamma.delete", "models.beta.edit"}}), adminaccess.CodeSelfEscalation)
	if contains(projectGrantsSQL(t, env.pool, 3, "viewer"), "models.beta.edit") {
		t.Fatal("the refused grant was written")
	}
}
