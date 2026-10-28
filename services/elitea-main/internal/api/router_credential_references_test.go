package api

import (
	"net/http"
	"testing"

	"github.com/go-chi/chi/v5"

	v2credentialreferences "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/v2/credentialreferences"
	v2skills "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/v2/skills"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/db/repos"
)

var credentialReferenceRoutes = []string{
	"GET /api/v2/admin/credential_references",
	"GET /api/v2/admin/credential_references/{id}",
	"POST /api/v2/admin/credential_references/{id}:activate",
	"POST /api/v2/admin/credential_references/{id}:retire",
	"POST /api/v2/admin/credential_references/{id}:attach",
	"POST /api/v2/admin/credential_references/{id}:detach",
	"PUT /api/v2/admin/agent_identities/{id}/toolkits/{toolkitID}",
	"DELETE /api/v2/admin/agent_identities/{id}/toolkits/{toolkitID}",
}

func newCredentialReferenceRouter(t *testing.T, admin *v2credentialreferences.Handler) chi.Router {
	t.Helper()
	return NewRouter(RouterConfig{
		SkillsRepo:                struct{ v2skills.Repository }{},
		AuthValidator:             testTokenValidator{user: authenticatedTestUser()},
		PrincipalValidator:        testPrincipalValidator{},
		CredentialReferencesAdmin: admin,
	})
}

// Flag off (nil field): none of the routes exists, and each answers what
// an unknown admin path answers, 404.
func TestCredentialReferenceRoutesAreUnmountedWhenTheDependencyIsNil(t *testing.T) {
	router := newCredentialReferenceRouter(t, nil)
	routes := walkRoutes(t, router)
	for _, route := range credentialReferenceRoutes {
		if _, ok := routes[route]; ok {
			t.Errorf("%s is mounted with CredentialReferencesAdmin nil", route)
		}
	}
	unknown := serveStatus(t, router, http.MethodGet, "/api/v2/admin/no_such_surface_1124")
	const id = "0192f5e2-7c3a-7b1e-8a9d-2b4c6d8e0f11"
	for _, probe := range []struct{ method, path string }{
		{http.MethodGet, "/api/v2/admin/credential_references?project_id=7"},
		{http.MethodGet, "/api/v2/admin/credential_references/" + id},
		{http.MethodPost, "/api/v2/admin/credential_references/" + id + ":activate"},
		{http.MethodPost, "/api/v2/admin/credential_references/" + id + ":retire"},
		{http.MethodPost, "/api/v2/admin/credential_references/" + id + ":attach"},
		{http.MethodPut, "/api/v2/admin/agent_identities/" + id + "/toolkits/11"},
	} {
		if got := serveStatus(t, router, probe.method, probe.path); got != unknown || got != http.StatusNotFound {
			t.Errorf("flag off: %s %s = %d, an unknown route answers %d (want 404)", probe.method, probe.path, got, unknown)
		}
	}
}

// Flag on: every route is mounted behind its central permission gate.
func TestCredentialReferenceRoutesAreMountedAndGatedWhenTheDependencyIsSet(t *testing.T) {
	admin, err := v2credentialreferences.New(repos.NewCredentialReferences(nil, nil))
	if err != nil {
		t.Fatal(err)
	}
	router := newCredentialReferenceRouter(t, admin)
	routes := walkRoutes(t, router)
	for _, route := range credentialReferenceRoutes {
		if _, ok := routes[route]; !ok {
			t.Errorf("%s is not mounted", route)
		}
	}
	got := serveStatus(t, router, http.MethodPost, "/api/v2/admin/credential_references/0192f5e2-7c3a-7b1e-8a9d-2b4c6d8e0f11:activate")
	if got == http.StatusNotFound || got == http.StatusOK || got == http.StatusInternalServerError {
		t.Errorf("activate without the permission = %d, want a gate refusal", got)
	}
}
