package api

import (
	"net/http"
	"testing"

	"github.com/go-chi/chi/v5"

	v2skills "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/skills"
)

// The admin Service Descriptors surface (unit A14, issue #200) — asserted on the
// ROUTER, because the two defects this port fixes are both route-shaped and the
// handler tests in internal/api/v2/eliteacore mount a router of their own.
//
//  1. The listing was `GET /api/v2/elitea_core/admin/{mode}`, so a caller could
//     name any mode and get the same three invented rows back. Legacy registers
//     `mode_handlers = {'administration': AdminAPI}` and nothing else, and a
//     static segment is also what keeps the handler from having to sniff a
//     `{mode}` param that a static segment does not bind — the trap #207 caught.
//  2. The two registration verbs had NO route at all. `RegisterDescriptor` was a
//     handler answering `{"ok": true}` to a discarded body, reachable from
//     nowhere. Registering them is what makes the refusal explicit and pinned; a
//     404 leaves the next person free to wire the stub back up.
//
// The route set is compared as a SET, in both directions — what must be present
// and what must be absent. A conflict resolution that added the `administration`
// registration while leaving `{mode}` behind would pass a presence-only check
// and still restore defect 1.
func newServiceDescriptorTestRouter(t *testing.T) chi.Router {
	t.Helper()
	return NewRouter(RouterConfig{
		SkillsRepo:         struct{ v2skills.Repository }{},
		AuthValidator:      testTokenValidator{user: authenticatedTestUser()},
		PrincipalValidator: testPrincipalValidator{},
	})
}

func TestRouterRegistersServiceDescriptorRoutesUnderAdministrationOnly(t *testing.T) {
	got := walkRoutes(t, newServiceDescriptorTestRouter(t))

	for _, required := range []string{
		"GET /api/v2/elitea_core/admin/administration",
		"POST /api/v2/elitea_core/register_descriptor/{projectID}",
		"DELETE /api/v2/elitea_core/register_descriptor/{projectID}",
	} {
		if _, ok := got[required]; !ok {
			t.Errorf("%s is not registered", required)
		}
	}

	// The wildcard the listing used to be. Its presence is the defect, not its
	// absence: it answered every mode with the same fabricated rows.
	if _, ok := got["GET /api/v2/elitea_core/admin/{mode}"]; ok {
		t.Error("GET /api/v2/elitea_core/admin/{mode} is registered again; " +
			"the legacy contract serves administration and no other mode on this path")
	}
}

// TestServiceDescriptorRoutesAreGatedOnTheRouter — the middleware, not the
// handler, refuses an unprivileged caller here, and `chi.Walk` cannot see
// whether a route carries one.
//
// The caller must be AUTHENTICATED for this to discriminate. Without a session
// the outer auth middleware answers 401 whether or not the route has its own
// gate, so the first version of this test passed against a router with every
// `.With(central(…))` removed. An injected testTokenValidator plus
// testAuthHeader supplies an authenticated principal, and
// this router has no pool — so `legacyrbac` resolves no permissions and a gated
// route answers 403 while an ungated one reaches the handler and answers its
// unconditional 501. That is the whole discrimination:
//
//	gated   → 403
//	ungated → 501
//
// It is the assertion that fails if a conflict resolution re-adds these routes
// without their gate, which is exactly how an admin surface silently opens.
func TestServiceDescriptorRoutesAreGatedOnTheRouter(t *testing.T) {
	router := newServiceDescriptorTestRouter(t)

	for _, probe := range []struct {
		method string
		path   string
	}{
		{http.MethodGet, "/api/v2/elitea_core/admin/administration"},
		{http.MethodPost, "/api/v2/elitea_core/register_descriptor/1"},
		{http.MethodDelete, "/api/v2/elitea_core/register_descriptor/1"},
	} {
		status := serveStatus(t, router, probe.method, probe.path)
		switch status {
		case http.StatusForbidden:
			// The gate ran and refused, which is what a caller holding no
			// permission must get.
		case http.StatusNotImplemented:
			t.Errorf("%s %s reached the handler for a caller with no permissions; "+
				"the route lost its permission gate", probe.method, probe.path)
		case http.StatusNotFound:
			t.Errorf("%s %s is not routed at all", probe.method, probe.path)
		default:
			t.Errorf("%s %s status = %d, want 403", probe.method, probe.path, status)
		}
	}
}

// TestServiceDescriptorGateIsNotVacuous is the control for the test above: with
// the same router and the same absence of permissions, a route that is
// deliberately NOT gated does reach its handler. Without this, "everything
// answers 403" would satisfy the gate assertions for the wrong reason — a router
// that had stopped serving altogether would look perfectly gated.
func TestServiceDescriptorGateIsNotVacuous(t *testing.T) {
	router := newServiceDescriptorTestRouter(t)

	// `GET /admin/system_info/prompt_lib` is the help-center read that legacy
	// leaves ungated (`PromptLibAPI.get` carries no `check_api` decorator), and
	// router.go keeps it that way on purpose.
	if status := serveStatus(t, router, http.MethodGet, "/api/v2/admin/system_info/prompt_lib"); status == http.StatusForbidden {
		t.Fatal("the deliberately ungated help-center read also answered 403; " +
			"the gate assertions above cannot distinguish a gate from a dead router")
	}
}
