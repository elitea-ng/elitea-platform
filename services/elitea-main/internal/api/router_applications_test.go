package api

import (
	"net/http"
	"net/http/httptest"
	"testing"

	v2skills "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/v2/skills"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/applications"
)

// applicationRoutes is the route table internal/api/router.go registers behind
// `if cfg.AppsRepo != nil`.
var applicationRoutes = []struct {
	method string
	path   string
}{
	{http.MethodGet, "/api/v2/elitea_core/applications/prompt_lib/1"},
	{http.MethodPost, "/api/v2/elitea_core/applications/prompt_lib/1"},
	{http.MethodGet, "/api/v2/elitea_core/application/prompt_lib/1/2"},
	{http.MethodPut, "/api/v2/elitea_core/application/prompt_lib/1/2"},
	{http.MethodDelete, "/api/v2/elitea_core/application/prompt_lib/1/2"},
	{http.MethodGet, "/api/v2/elitea_core/versions/prompt_lib/1/2"},
	{http.MethodPost, "/api/v2/elitea_core/versions/prompt_lib/1/2"},
	{http.MethodGet, "/api/v2/elitea_core/version/prompt_lib/1/2/3"},
	{http.MethodPut, "/api/v2/elitea_core/version/prompt_lib/1/2/3"},
	{http.MethodDelete, "/api/v2/elitea_core/version/prompt_lib/1/2/3"},
	// PATCH on this path is the expanded READ the SDK calls, not an update (#336).
	{http.MethodPatch, "/api/v2/elitea_core/version/prompt_lib/1/2/3"},
	{http.MethodGet, "/api/v2/elitea_core/default_version/prompt_lib/1/2"},
	{http.MethodPatch, "/api/v2/elitea_core/default_version/prompt_lib/1/2/3"},
	{http.MethodPost, "/api/v2/elitea_core/batch_replace_version/prompt_lib/1/2/3"},
}

// TestRouterRegistersEveryApplicationRouteWhenAppsRepoIsComposed pins that
// route table. The gate is silent by design — a nil repository unregisters the
// whole group rather than failing startup — so dropping a line here has no
// symptom other than a production 404 (#115).
// cmd/elitea-main/main_router_wiring_test.go covers the other half: that
// main.go actually sets the field.
//
// Requests carry a real credential (testAuthHeader) validated by an injected
// testTokenValidator, so they reach the routing table instead of being
// answered by the auth middleware — which returns 401 for every path under
// the group, registered or not, and would make a "not 404" assertion pass for
// any string at all.
func TestRouterRegistersEveryApplicationRouteWhenAppsRepoIsComposed(t *testing.T) {
	router := NewRouter(RouterConfig{
		AppsRepo:           struct{ applications.Repository }{},
		AuthValidator:      testTokenValidator{user: authenticatedTestUser()},
		PrincipalValidator: testPrincipalValidator{},
	})

	// Control: an unregistered path under the same mounted group still 404s,
	// so "not 404" below is a real signal.
	response := httptest.NewRecorder()
	router.ServeHTTP(response, testAuthHeader(httptest.NewRequest(http.MethodGet, "/api/v2/elitea_core/not_a_real_route/prompt_lib/1", nil)))
	if response.Code != http.StatusNotFound {
		t.Fatalf("an unregistered path answers %d, so this test cannot detect a missing route", response.Code)
	}

	for _, route := range applicationRoutes {
		t.Run(route.method+" "+route.path, func(t *testing.T) {
			response := httptest.NewRecorder()
			router.ServeHTTP(response, testAuthHeader(httptest.NewRequest(route.method, route.path, nil)))
			// 405 matters as much as 404: several of these paths carry more
			// than one method, so dropping just one verb leaves chi matching
			// the path and answering Method Not Allowed.
			if response.Code == http.StatusNotFound || response.Code == http.StatusMethodNotAllowed {
				t.Errorf("route is not registered even though AppsRepo is composed (status %d)", response.Code)
			}
		})
	}
}

// TestRouterDropsApplicationRoutesWithoutAppsRepo documents the gate's current
// shape: the routes vanish rather than failing loudly. The prototype group is
// still mounted here (via SkillsRepo), so the 404s below are the AppsRepo gate
// and not an unmounted /api/v2.
func TestRouterDropsApplicationRoutesWithoutAppsRepo(t *testing.T) {
	router := NewRouter(RouterConfig{
		SkillsRepo:         struct{ v2skills.Repository }{},
		AuthValidator:      testTokenValidator{user: authenticatedTestUser()},
		PrincipalValidator: testPrincipalValidator{},
	})

	// The group really is mounted: a sibling route registered by SkillsRepo
	// answers.
	response := httptest.NewRecorder()
	router.ServeHTTP(response, testAuthHeader(httptest.NewRequest(http.MethodGet, "/api/v2/elitea_core/skills/prompt_lib/1", nil)))
	if response.Code == http.StatusNotFound {
		t.Fatalf("the prototype group is not mounted, so this test proves nothing")
	}

	for _, route := range applicationRoutes {
		t.Run(route.method+" "+route.path, func(t *testing.T) {
			response := httptest.NewRecorder()
			router.ServeHTTP(response, testAuthHeader(httptest.NewRequest(route.method, route.path, nil)))
			if response.Code != http.StatusNotFound {
				t.Errorf("status = %d, want 404: the route answers without a repository behind it", response.Code)
			}
		})
	}
}
