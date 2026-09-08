package api

// The three role-definition writes under /admin/roles (gap G9).
//
// WHY THIS FILE EXISTS AT ALL. The writes take `/admin/roles/{scope}/{mode}`
// while the two listings beside them take `/admin/roles/{mode}/{projectID}`,
// and one of the listings is registered with a STATIC first segment,
// `/admin/roles/administration/{projectID}`. Whether a POST to
// `/admin/roles/administration/default` reaches the write handler or 404s is a
// property of chi's trie: two param nodes with different names are siblings,
// and a static node is preferred over both — but only for the METHODS it
// carries. The static node here carries GET alone, so the three writes fall
// through to `{scope}`.
//
// That is a fact about a dependency, not about this repository's code, so it is
// measured rather than assumed. If a later change registers a write on the
// static `administration` node, or renames a parameter, this file says so.

import (
	"net/http"
	"testing"

	"github.com/go-chi/chi/v5"

	v2skills "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/skills"
)

func newAdminRoleWriteRouter(t *testing.T) chi.Router {
	t.Helper()
	return NewRouter(RouterConfig{
		SkillsRepo:         struct{ v2skills.Repository }{},
		AuthValidator:      testTokenValidator{user: authenticatedTestUser()},
		PrincipalValidator: testPrincipalValidator{},
	})
}

// The route SET, in both directions: the three writes exist AND the two
// listings they sit beside are untouched.
func TestAdminRoleWriteRoutesAreRegisteredBesideTheListings(t *testing.T) {
	got := walkRoutes(t, newAdminRoleWriteRouter(t))

	for _, required := range []string{
		"POST /api/v2/admin/roles/{scope}/{mode}",
		"PUT /api/v2/admin/roles/{scope}/{mode}",
		"DELETE /api/v2/admin/roles/{scope}/{mode}",
		// Unchanged by this unit. A write registered on the static
		// `administration` node would have had to move one of these.
		"GET /api/v2/admin/roles/{mode}/{projectID}",
		"GET /api/v2/admin/roles/administration/{projectID}",
	} {
		if _, ok := got[required]; !ok {
			t.Errorf("%s is not registered", required)
		}
	}
}

// TestAdminRoleWriteRoutesResolve is the name roles_crud.go and router.go both
// cite: it proves the three writes RESOLVE on every scope segment, including
// `administration`, where the static GET node sits.
//
// The discriminator is 404 versus anything else. This router has no pool, so a
// resolved write is refused by its permission gate (403) — never 404, which is
// what an unresolved path gives. The listing's own 403 in the same table is the
// control that says the router is serving at all.
func TestAdminRoleWriteRoutesResolve(t *testing.T) {
	router := newAdminRoleWriteRouter(t)

	for _, probe := range []struct{ method, path string }{
		{http.MethodPost, "/api/v2/admin/roles/administration/default"},
		{http.MethodPut, "/api/v2/admin/roles/administration/default"},
		{http.MethodDelete, "/api/v2/admin/roles/administration/default"},
		{http.MethodPost, "/api/v2/admin/roles/public/default"},
		{http.MethodPut, "/api/v2/admin/roles/support/default"},
		{http.MethodGet, "/api/v2/admin/roles/administration/41"},
	} {
		status := serveStatus(t, router, probe.method, probe.path)
		if status == http.StatusNotFound {
			t.Errorf("%s %s = 404: the path does not resolve to any handler",
				probe.method, probe.path)
		}
		if status == http.StatusMethodNotAllowed {
			t.Errorf("%s %s = 405: chi matched a node that does not carry this method",
				probe.method, probe.path)
		}
	}
}

// The three writes are GATED. A caller who resolves no central permission is
// refused, which for this pool-less router is the only status a gated route can
// produce — an ungated one would reach the handler and answer 503.
//
// The distinction matters here more than usual: 503 is exactly what
// `beginRoleWrite` answers without a pool, so an UNGATED route in this router
// is not a 500 or a panic. It is a clean, plausible-looking response.
func TestAdminRoleWritesRefuseACallerWithNoCentralRole(t *testing.T) {
	router := newAdminRoleWriteRouter(t)

	for _, probe := range []struct{ method, path string }{
		{http.MethodPost, "/api/v2/admin/roles/administration/default"},
		{http.MethodPut, "/api/v2/admin/roles/administration/default"},
		{http.MethodDelete, "/api/v2/admin/roles/administration/default"},
	} {
		switch status := serveStatus(t, router, probe.method, probe.path); status {
		case http.StatusForbidden:
			// The gate ran and refused, which is correct for a caller holding
			// no administration role.
		case http.StatusServiceUnavailable:
			t.Errorf("%s %s reached the handler ungated (it answered its own no-pool 503)",
				probe.method, probe.path)
		default:
			t.Errorf("%s %s status = %d, want 403", probe.method, probe.path, status)
		}
	}
}
