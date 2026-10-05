package api

import (
	"net/http"
	"testing"

	"github.com/go-chi/chi/v5"

	v2skills "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/skills"
)

// The platform default model surface (#6826) is an administration-mode
// surface. It sits in the `configuration.governance` group with the platform
// models it chooses from. These tests pin that on the ROUTER: the handler
// tests mount a router of their own and cannot see the gate.
//
// The caller is authenticated and this router has no pool, so legacyrbac
// resolves no permission. A gated route answers 403. An ungated route would
// reach the handler, which answers 503 because no service is composed.
func newPlatformDefaultModelTestRouter(t *testing.T) chi.Router {
	t.Helper()
	return NewRouter(RouterConfig{
		SkillsRepo:         struct{ v2skills.Repository }{},
		AuthValidator:      testTokenValidator{user: authenticatedTestUser()},
		PrincipalValidator: testPrincipalValidator{},
	})
}

func TestRouterRegistersThePlatformDefaultModelRoutes(t *testing.T) {
	got := walkRoutes(t, newPlatformDefaultModelTestRouter(t))
	for _, required := range []string{
		"GET /api/v2/admin/gateway/default_model/",
		"PUT /api/v2/admin/gateway/default_model/",
		"DELETE /api/v2/admin/gateway/default_model/",
		"GET /api/v2/admin/gateway/platform_models/{configID}/default_usage",
	} {
		if _, ok := got[required]; !ok {
			t.Errorf("%s is not registered", required)
		}
	}
}

func TestPlatformDefaultModelRoutesAreGatedInAdministrationMode(t *testing.T) {
	router := newPlatformDefaultModelTestRouter(t)
	for _, probe := range []struct {
		method string
		path   string
	}{
		{http.MethodGet, "/api/v2/admin/gateway/default_model"},
		{http.MethodPut, "/api/v2/admin/gateway/default_model"},
		{http.MethodDelete, "/api/v2/admin/gateway/default_model"},
		{http.MethodGet, "/api/v2/admin/gateway/platform_models/5/default_usage"},
	} {
		status := serveStatus(t, router, probe.method, probe.path)
		switch status {
		case http.StatusForbidden:
		case http.StatusServiceUnavailable:
			t.Errorf("%s %s reached the handler for a caller with no permission; "+
				"the route lost its configuration.governance gate", probe.method, probe.path)
		case http.StatusNotFound:
			t.Errorf("%s %s is not routed", probe.method, probe.path)
		default:
			t.Errorf("%s %s status = %d, want 403", probe.method, probe.path, status)
		}
	}
}

// TestPlatformDefaultModelGateIsNotVacuous is the control: the same router and
// the same caller reach a deliberately ungated read, so "everything answers
// 403" cannot pass the test above for the wrong reason.
func TestPlatformDefaultModelGateIsNotVacuous(t *testing.T) {
	router := newPlatformDefaultModelTestRouter(t)
	if status := serveStatus(t, router, http.MethodGet, "/api/v2/admin/system_info/prompt_lib"); status == http.StatusForbidden {
		t.Fatal("the ungated help-center read also answered 403; the gate test cannot tell a gate from a dead router")
	}
}
