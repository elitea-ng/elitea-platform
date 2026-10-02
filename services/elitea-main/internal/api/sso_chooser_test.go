package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	v2auth "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/auth"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/identityproviders"
)

type chooserProviderStore map[identityproviders.Kind]identityproviders.Provider

func (s chooserProviderStore) Enabled(_ context.Context, kind identityproviders.Kind) (identityproviders.Provider, error) {
	provider, ok := s[kind]
	if !ok {
		return identityproviders.Provider{}, identityproviders.ErrNotFound
	}
	return provider, nil
}

type chooserVault struct{}

func (chooserVault) LookupAdminHiddenSecret(context.Context, string) (string, error) { return "", nil }

// ssoRouter composes the production router with both single sign-on planes
// reading the given providers.
func ssoRouter(t *testing.T, providers chooserProviderStore) http.Handler {
	t.Helper()
	oidc, err := v2auth.NewOIDCHandler(context.Background(), nil, nil, "0123456789abcdef0123456789abcdef")
	if err != nil {
		t.Fatal(err)
	}
	oidc = oidc.WithProviderStore(providers, chooserVault{})
	saml := v2auth.NewSAMLHandler(nil, "0123456789abcdef0123456789abcdef", providers, chooserVault{}, true)
	return NewRouter(RouterConfig{
		AuthValidator:      testTokenValidator{user: authenticatedTestUser()},
		PrincipalValidator: testPrincipalValidator{},
		SessionHandler:     v2auth.NewSessionHandler(nil, "0123456789abcdef0123456789abcdef"),
		OIDCHandler:        oidc,
		SAMLHandler:        saml,
	})
}

func gitHubAndEntra() chooserProviderStore {
	return chooserProviderStore{
		identityproviders.KindOIDC: {
			Key: "github", Kind: identityproviders.KindOIDC, DisplayName: "GitHub", Enabled: true,
			OIDC: &identityproviders.OIDCDocument{Issuer: "https://dex.example.com"},
		},
		identityproviders.KindSAML: {
			Key: "entra", Kind: identityproviders.KindSAML, DisplayName: "Microsoft", Enabled: true,
			SAML: &identityproviders.SAMLDocument{LoginDomains: []string{"contoso.com"}},
		},
	}
}

// With both planes usable, /forward-auth/login is the sign-in page.
func TestTheSSOPlaneServesTheSignInPage(t *testing.T) {
	router := ssoRouter(t, gitHubAndEntra())
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/forward-auth/login?target_to=%2Fapp", nil))

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", recorder.Code)
	}
	body := recorder.Body.String()
	for _, want := range []string{"Continue with GitHub", "Continue with Microsoft", "Work email"} {
		if !strings.Contains(body, want) {
			t.Errorf("page lacks %q", want)
		}
	}
	if csp := recorder.Header().Get("Content-Security-Policy"); !strings.Contains(csp, "default-src 'none'") {
		t.Fatalf("CSP = %q", csp)
	}
}

// The email form routes through the mounted continue route to the SAML login.
func TestTheSignInPageRoutesAWorkEmailToSAML(t *testing.T) {
	router := ssoRouter(t, gitHubAndEntra())
	form := url.Values{"email": {"alice@contoso.com"}, "target_to": {"/app/chat"}}
	request := httptest.NewRequest(http.MethodPost, "/forward-auth/login/continue", strings.NewReader(form.Encode()))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303", recorder.Code)
	}
	location, err := url.Parse(recorder.Header().Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	if location.Path != "/forward-auth/auth_saml/login" ||
		location.Query().Get("login_hint") != "alice@contoso.com" ||
		location.Query().Get("target_to") != "/app/chat" {
		t.Fatalf("Location = %s", location)
	}
}

// A SAML-only deployment now has a /forward-auth/login. Before, the route was
// mounted only with an OIDC handler, so the SAML login was reachable only by
// typing its path.
func TestASAMLOnlyDeploymentRedirectsToTheSAMLLogin(t *testing.T) {
	providers := gitHubAndEntra()
	delete(providers, identityproviders.KindOIDC)
	router := ssoRouter(t, providers)
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/forward-auth/login?target_to=%2Fapp", nil))

	if recorder.Code != http.StatusFound ||
		recorder.Header().Get("Location") != "/forward-auth/auth_saml/login?target_to=%2Fapp" {
		t.Fatalf("status = %d, Location = %q", recorder.Code, recorder.Header().Get("Location"))
	}
}
