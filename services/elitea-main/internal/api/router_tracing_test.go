package api

import (
	"net/http"
	"testing"

	"github.com/go-chi/chi/v5"

	v2skills "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/v2/skills"
)

// The tracing plugin's admin status read (issue #250) is wired up like this
// in router.go:
//
//	requireTracingAdminStatus := apimw.RequireCentralPermissions(
//	    permissionResolver, platformauth.PermissionModeAdministration,
//	    "runtime.plugins",
//	)
//	r.Mount("/tracing", tracingHandler.Routes(requireTracingAdminStatus))
//
// internal/api/v2/tracing's own handler tests inject a stub middleware
// directly into Routes() and never touch this construction, so nothing
// previously asserted that router.go actually resolves "runtime.plugins" in
// PermissionModeAdministration before GET /api/v2/tracing/status/administration
// reaches its handler. A future edit could change the permission string, flip
// the mode, or drop the .Group/requireTracingAdminStatus wrapping entirely —
// every existing test would still pass.
func newTracingTestRouter(t *testing.T) chi.Router {
	t.Helper()
	return NewRouter(RouterConfig{
		SkillsRepo:         struct{ v2skills.Repository }{},
		AuthValidator:      testTokenValidator{user: authenticatedTestUser()},
		PrincipalValidator: testPrincipalValidator{},
	})
}

func TestRouterRegistersTracingRoutes(t *testing.T) {
	got := walkRoutes(t, newTracingTestRouter(t))

	for _, required := range []string{
		"POST /api/v2/tracing/collect/prompt_lib",
		"POST /api/v2/tracing/collect/prompt_lib/{projectID}",
		"POST /api/v2/tracing/otlp/prompt_lib",
		"POST /api/v2/tracing/otlp/prompt_lib/{projectID}",
		"GET /api/v2/tracing/status/prompt_lib/{projectID}",
		"GET /api/v2/tracing/status/administration",
	} {
		if _, ok := got[required]; !ok {
			t.Errorf("%s is not registered", required)
		}
	}
}

// TestTracingAdminStatusIsGatedOnTheRouter proves router.go's
// requireTracingAdminStatus actually runs: an authenticated caller holding no
// permissions (this router has no pool, so legacyrbac resolves nothing) must
// be refused, not answered the real status payload.
func TestTracingAdminStatusIsGatedOnTheRouter(t *testing.T) {
	router := newTracingTestRouter(t)

	status := serveStatus(t, router, http.MethodGet, "/api/v2/tracing/status/administration")
	switch status {
	case http.StatusForbidden:
		// The gate ran and refused, which is what a caller holding no
		// "runtime.plugins" grant must get.
	case http.StatusOK:
		t.Error("GET /api/v2/tracing/status/administration reached the handler for a caller with no permissions; " +
			"the route lost its runtime.plugins gate")
	case http.StatusNotFound:
		t.Error("GET /api/v2/tracing/status/administration is not routed at all")
	default:
		t.Errorf("GET /api/v2/tracing/status/administration status = %d, want 403", status)
	}
}

// TestTracingAdminStatusGateIsNotVacuous is the control for the test above:
// with the same router and the same absence of permissions, a deliberately
// ungated route still reaches its handler. Without this, "everything answers
// 403" would satisfy the assertion above for the wrong reason.
func TestTracingAdminStatusGateIsNotVacuous(t *testing.T) {
	router := newTracingTestRouter(t)

	// The project-scoped tracing status/collect/otlp routes are gated by
	// apimw.RequireProjectAccess, not a central permission, and this router
	// has no pool — RequireProjectAccess with a nil pool answers 503, not
	// 403, so it cannot be used as the "ungated" control here. Use the
	// deliberately ungated help-center read the service-descriptors test
	// above also relies on.
	if status := serveStatus(t, router, http.MethodGet, "/api/v2/admin/system_info/prompt_lib"); status == http.StatusForbidden {
		t.Fatal("the deliberately ungated help-center read also answered 403; " +
			"the gate assertion above cannot distinguish a gate from a dead router")
	}
}
