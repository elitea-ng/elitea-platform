package api

import (
	"context"
	"net/http"
	"testing"

	"github.com/go-chi/chi/v5"

	v2credentialgrants "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/v2/credentialgrants"
	v2skills "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/v2/skills"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/credentialgrant"
)

// unusedCredentialGrantService is never reached: the routes are probed for
// registration and for the permission gate only.
type unusedCredentialGrantService struct{}

func (unusedCredentialGrantService) ProposeGrant(context.Context, credentialgrant.Proposal, string) (credentialgrant.CredentialUseGrant, error) {
	panic("unreachable")
}
func (unusedCredentialGrantService) ListGrants(context.Context, credentialgrant.ListFilter) ([]credentialgrant.CredentialUseGrant, error) {
	panic("unreachable")
}
func (unusedCredentialGrantService) TransitionGrant(context.Context, int64, string, credentialgrant.Action, string) (credentialgrant.CredentialUseGrant, error) {
	panic("unreachable")
}

var credentialGrantRoutes = []string{
	"GET /api/v2/projects/{projectID}/credential_grants",
	"POST /api/v2/projects/{projectID}/credential_grants",
	"POST /api/v2/projects/{projectID}/credential_grants/{id}:approve",
	"POST /api/v2/projects/{projectID}/credential_grants/{id}:suspend",
	"POST /api/v2/projects/{projectID}/credential_grants/{id}:revoke",
}

func newCredentialGrantRouter(t *testing.T, admin *v2credentialgrants.Handler) chi.Router {
	t.Helper()
	return NewRouter(RouterConfig{
		SkillsRepo:            struct{ v2skills.Repository }{},
		AuthValidator:         testTokenValidator{user: authenticatedTestUser()},
		PrincipalValidator:    testPrincipalValidator{},
		CredentialGrantsAdmin: admin,
	})
}

// Flag off (nil field): none of the routes exists, and a probe answers
// exactly what an unknown project path answers.
func TestCredentialGrantRoutesAreUnmountedWhenTheDependencyIsNil(t *testing.T) {
	router := newCredentialGrantRouter(t, nil)
	routes := walkRoutes(t, router)
	for _, route := range credentialGrantRoutes {
		if _, ok := routes[route]; ok {
			t.Errorf("%s is mounted with CredentialGrantsAdmin nil", route)
		}
	}
	unknown := serveStatus(t, router, http.MethodGet, "/api/v2/projects/7/no_such_surface_1116")
	for _, probe := range []struct{ method, path string }{
		{http.MethodGet, "/api/v2/projects/7/credential_grants"},
		{http.MethodPost, "/api/v2/projects/7/credential_grants"},
		{http.MethodPost, "/api/v2/projects/7/credential_grants/0192f5e2-7c3a-7b1e-8a9d-2b4c6d8e0f11:approve"},
		{http.MethodPost, "/api/v2/projects/7/credential_grants/0192f5e2-7c3a-7b1e-8a9d-2b4c6d8e0f11:revoke"},
	} {
		if got := serveStatus(t, router, probe.method, probe.path); got != unknown || got != http.StatusNotFound {
			t.Errorf("flag off: %s %s = %d, an unknown route answers %d (want 404)", probe.method, probe.path, got, unknown)
		}
	}
}

// Flag on: every route is mounted behind its gate, and the /projects mount
// beside it still routes its own paths.
func TestCredentialGrantRoutesAreMountedAndGatedWhenTheDependencyIsSet(t *testing.T) {
	admin, err := v2credentialgrants.New(unusedCredentialGrantService{})
	if err != nil {
		t.Fatal(err)
	}
	router := newCredentialGrantRouter(t, admin)
	routes := walkRoutes(t, router)
	for _, route := range credentialGrantRoutes {
		if _, ok := routes[route]; !ok {
			t.Errorf("%s is not mounted", route)
		}
	}
	got := serveStatus(t, router, http.MethodGet, "/api/v2/projects/7/credential_grants")
	if got == http.StatusNotFound || got == http.StatusOK || got == http.StatusInternalServerError {
		t.Errorf("GET credential_grants without a grant = %d, want a gate refusal", got)
	}
	// A path of the /projects mount is not swallowed by the param routes.
	off := newCredentialGrantRouter(t, nil)
	for _, path := range []string{"/api/v2/projects/quota/7", "/api/v2/projects/statistics/7"} {
		if on, before := serveStatus(t, router, http.MethodGet, path), serveStatus(t, off, http.MethodGet, path); on != before {
			t.Errorf("GET %s = %d with grants mounted, %d without", path, on, before)
		}
	}
}
