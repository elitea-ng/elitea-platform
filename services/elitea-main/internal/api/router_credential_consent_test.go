package api

import (
	"context"
	"net/http"
	"testing"

	"github.com/go-chi/chi/v5"

	v2credentialconsent "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/v2/credentialconsent"
	v2skills "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/v2/skills"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/provideroauth"
)

// unusedConsentFlow is never reached: the routes are probed for
// registration and for the permission gate only.
type unusedConsentFlow struct{}

func (unusedConsentFlow) Start(context.Context, provideroauth.StartRequest) (provideroauth.StartResult, error) {
	panic("unreachable")
}
func (unusedConsentFlow) Callback(context.Context, provideroauth.CallbackRequest, provideroauth.Authorize) (provideroauth.CallbackResult, error) {
	panic("unreachable")
}
func (unusedConsentFlow) RecordRefusal(context.Context, int64, string, string) error {
	panic("unreachable")
}

const (
	credentialConsentStartRoute    = "POST /api/v2/credential_consent/{provider}:start"
	credentialConsentCallbackRoute = "GET /api/v2/credential_consent/{provider}:callback"
)

func newCredentialConsentRouter(t *testing.T, consent *v2credentialconsent.Handler) chi.Router {
	t.Helper()
	return NewRouter(RouterConfig{
		SkillsRepo:         struct{ v2skills.Repository }{},
		AuthValidator:      testTokenValidator{user: authenticatedTestUser()},
		PrincipalValidator: testPrincipalValidator{},
		CredentialConsent:  consent,
	})
}

// Flag off (nil field): neither route exists.
func TestCredentialConsentRoutesAreUnmountedWhenTheDependencyIsNil(t *testing.T) {
	router := newCredentialConsentRouter(t, nil)
	routes := walkRoutes(t, router)
	for _, route := range []string{credentialConsentStartRoute, credentialConsentCallbackRoute} {
		if _, ok := routes[route]; ok {
			t.Fatalf("%s is mounted with CredentialConsent nil", route)
		}
	}
	if got := serveStatus(t, router, http.MethodPost, "/api/v2/credential_consent/jira:start"); got != http.StatusNotFound {
		t.Fatalf("flag off: start = %d, want 404", got)
	}
	if got := serveStatus(t, router, http.MethodGet, "/api/v2/credential_consent/jira:callback?state=x&code=y"); got != http.StatusNotFound {
		t.Fatalf("flag off: callback = %d, want 404", got)
	}
}

// Flag on: both routes are mounted; start is behind the use permission
// gate (an empty body is refused before the flow is reached).
func TestCredentialConsentRoutesAreMountedWhenTheDependencyIsSet(t *testing.T) {
	consent, err := v2credentialconsent.New(unusedConsentFlow{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	router := newCredentialConsentRouter(t, consent)
	routes := walkRoutes(t, router)
	for _, route := range []string{credentialConsentStartRoute, credentialConsentCallbackRoute} {
		if _, ok := routes[route]; !ok {
			t.Fatalf("%s is not mounted", route)
		}
	}
	got := serveStatus(t, router, http.MethodPost, "/api/v2/credential_consent/jira:start")
	if got == http.StatusNotFound || got == http.StatusOK || got == http.StatusInternalServerError {
		t.Fatalf("start without a body or permission = %d, want a refusal", got)
	}
}
