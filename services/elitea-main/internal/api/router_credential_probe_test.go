package api

import (
	"context"
	"net/http"
	"testing"

	"github.com/go-chi/chi/v5"

	v2credentialprobe "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/v2/credentialprobe"
	v2skills "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/v2/skills"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/credentialadapters"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/credentialgrant"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/securityaudit"
)

// unusedProbeDeps is never reached: the route is probed for registration
// and for the permission gate only.
type unusedProbeDeps struct{}

func (unusedProbeDeps) CredentialReference(context.Context, int64, string) (credentialgrant.CredentialReference, error) {
	panic("unreachable")
}
func (unusedProbeDeps) ActiveGrantsWithCapability(context.Context, int64, string, credentialgrant.Capability) ([]credentialgrant.CredentialUseGrant, error) {
	panic("unreachable")
}
func (unusedProbeDeps) RecordProbe(context.Context, securityaudit.SecurityEvent) error {
	panic("unreachable")
}
func (unusedProbeDeps) Descriptor(string) (credentialadapters.AdapterDescriptor, bool) {
	panic("unreachable")
}

const credentialProbeRoute = "POST /api/v2/projects/{projectID}/credential_references/{id}:probe"

func newCredentialProbeRouter(t *testing.T, probe *v2credentialprobe.Handler) chi.Router {
	t.Helper()
	return NewRouter(RouterConfig{
		SkillsRepo:         struct{ v2skills.Repository }{},
		AuthValidator:      testTokenValidator{user: authenticatedTestUser()},
		PrincipalValidator: testPrincipalValidator{},
		CredentialProbe:    probe,
	})
}

// Flag off (nil field): the route does not exist and answers like an
// unknown project path.
func TestCredentialProbeRouteIsUnmountedWhenTheDependencyIsNil(t *testing.T) {
	router := newCredentialProbeRouter(t, nil)
	if _, ok := walkRoutes(t, router)[credentialProbeRoute]; ok {
		t.Fatal("the probe route is mounted with CredentialProbe nil")
	}
	unknown := serveStatus(t, router, http.MethodPost, "/api/v2/projects/7/no_such_surface_1120")
	path := "/api/v2/projects/7/credential_references/0192f5e2-7c3a-7b1e-8a9d-2b4c6d8e0f11:probe"
	if got := serveStatus(t, router, http.MethodPost, path); got != unknown || got != http.StatusNotFound {
		t.Fatalf("flag off: POST %s = %d, an unknown route answers %d (want 404)", path, got, unknown)
	}
}

// Flag on: the route is mounted behind the probe permission gate.
func TestCredentialProbeRouteIsMountedAndGatedWhenTheDependencyIsSet(t *testing.T) {
	deps := unusedProbeDeps{}
	probe, err := v2credentialprobe.New(v2credentialprobe.Config{
		References: deps, Grants: deps, Audit: deps, Descriptors: deps,
	})
	if err != nil {
		t.Fatal(err)
	}
	router := newCredentialProbeRouter(t, probe)
	if _, ok := walkRoutes(t, router)[credentialProbeRoute]; !ok {
		t.Fatal("the probe route is not mounted")
	}
	got := serveStatus(t, router, http.MethodPost, "/api/v2/projects/7/credential_references/0192f5e2-7c3a-7b1e-8a9d-2b4c6d8e0f11:probe")
	if got == http.StatusNotFound || got == http.StatusOK || got == http.StatusInternalServerError {
		t.Fatalf("probe without the permission = %d, want a gate refusal", got)
	}
}
