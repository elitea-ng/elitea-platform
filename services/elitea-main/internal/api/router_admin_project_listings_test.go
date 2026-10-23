package api

import (
	"net/http"
	"testing"

	"github.com/go-chi/chi/v5"

	v2skills "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/v2/skills"
)

// The project MEMBER listing and the project ROLE listing under /admin (#313).
//
// Both shipped with NO gate of any kind while their own writes beside them were
// gated. `coreHandler.Users` joins auth_core__project_user_role for whatever
// project the {projectID} segment names and answers with every member's EMAIL,
// name and roles, so any authenticated principal — a PAT holder included —
// could enumerate any project's membership by editing one path segment.
//
// EACH LISTING IS REGISTERED TWICE, and this file is about the pair. The two
// registrations run the same handler and differ only in the gate:
//
//	GET /api/v2/admin/users/{mode}/{projectID}           DEFAULT mode
//	GET /api/v2/admin/users/administration/{projectID}   ADMINISTRATION mode
//	GET /api/v2/admin/roles/{mode}/{projectID}           DEFAULT mode
//	GET /api/v2/admin/roles/administration/{projectID}   ADMINISTRATION mode
//
// The default-mode pair serves the project settings page, whose caller is a
// member. The administration pair serves the admin panel's project member
// dialog and activity drawer, whose caller is an operator who is a member of
// nothing — legacyrbac resolves a default-mode gate purely from membership of
// the named project, so the default-mode gate alone refuses every legitimate
// caller of that dialog.
//
// WHY THE STATIC REGISTRATION IS LOAD-BEARING, not decorative. chi falls
// through to the `{mode}` route for a method the static node does not carry.
// Before this change the static `administration` node held POST and PUT only,
// so a GET to `/admin/users/administration/41` was served by the `{mode}` route.
// Registering the default-mode gate there WITHOUT adding a static GET would
// therefore have moved the admin panel's read onto the membership-shaped
// resolver and broken the dialog. `TestAdminProjectListingsRegisterBothModes`
// is what fails if the static registration is dropped in a rebase.
//
// The three project-scoped claims for the DEFAULT-mode pair — refuse another
// project, refuse an under-privileged member, admit an entitled one — are made
// by router_elitea_core_project_scope_test.go, whose table carries both rows.
// This file covers what that table cannot express: the administration pair,
// which resolves centrally rather than against {projectID}.
func newAdminProjectListingRouter(t *testing.T) chi.Router {
	t.Helper()
	return NewRouter(RouterConfig{
		SkillsRepo:         struct{ v2skills.Repository }{},
		AuthValidator:      testTokenValidator{user: authenticatedTestUser()},
		PrincipalValidator: testPrincipalValidator{},
	})
}

// The route SET, in both directions. A presence-only check passes against a
// composition that added the static GET and dropped the `{mode}` one, which
// would leave the project settings page unrouted.
func TestAdminProjectListingsRegisterBothModes(t *testing.T) {
	got := walkRoutes(t, newAdminProjectListingRouter(t))

	for _, required := range []string{
		"GET /api/v2/admin/users/{mode}/{projectID}",
		"GET /api/v2/admin/users/administration/{projectID}",
		"GET /api/v2/admin/roles/{mode}/{projectID}",
		"GET /api/v2/admin/roles/administration/{projectID}",
	} {
		if _, ok := got[required]; !ok {
			t.Errorf("%s is not registered.\n"+
				"  The two modes need two registrations: chi falls through to the {mode} route for a\n"+
				"  method the static node does not carry, so dropping the static GET moves the admin\n"+
				"  panel's read onto the default-mode gate, which refuses every operator.", required)
		}
	}
}

// The administration pair REFUSES a caller who resolves nothing.
//
// The caller must be AUTHENTICATED for this to discriminate: without a session
// the outer auth middleware answers 401 whether or not the route carries a gate.
// This router has no pool, so legacyrbac resolves no permission and a gated
// route answers 403 while an ungated one reaches the handler and answers 200
// with an empty listing. That difference is the whole measurement:
//
//	gated   → 403
//	ungated → 200
func TestAdminProjectListingsRefuseACallerWithNoCentralRole(t *testing.T) {
	router := newAdminProjectListingRouter(t)

	for _, path := range []string{
		"/api/v2/admin/users/administration/41",
		"/api/v2/admin/roles/administration/41",
	} {
		status := serveStatus(t, router, http.MethodGet, path)
		switch status {
		case http.StatusForbidden:
			// The gate ran and refused, which is correct for a caller holding
			// no administration role.
		case http.StatusOK:
			t.Errorf("GET %s reached the handler for a caller with no permissions, so it "+
				"answered with the project's member or role listing ungated", path)
		case http.StatusNotFound:
			t.Errorf("GET %s is not routed at all", path)
		default:
			t.Errorf("GET %s status = %d, want 403", path, status)
		}
	}
}

// The control. Without it, "every route answers 403" satisfies the test above
// for the wrong reason — a router that had stopped serving altogether would
// read as perfectly gated.
//
// `GET /admin/system_info/prompt_lib` is the help-center read that legacy
// leaves ungated, and router.go keeps it that way on purpose.
func TestAdminProjectListingGateIsNotVacuous(t *testing.T) {
	router := newAdminProjectListingRouter(t)

	if status := serveStatus(t, router, http.MethodGet, "/api/v2/admin/system_info/prompt_lib"); status == http.StatusForbidden {
		t.Fatal("the deliberately ungated help-center read also answered 403; the gate " +
			"assertions above cannot distinguish a gate from a dead router")
	}
}
