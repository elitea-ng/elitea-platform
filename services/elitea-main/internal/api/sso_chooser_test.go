package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

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

// With both planes usable, /auth/login is the sign-in page.
func TestTheSSOPlaneServesTheSignInPage(t *testing.T) {
	router := ssoRouter(t, gitHubAndEntra())
	for _, path := range []string{"/auth/login"} {
		recorder := httptest.NewRecorder()
		router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, path+"?target_to=%2Fapp", nil))

		if recorder.Code != http.StatusOK {
			t.Fatalf("%s: status = %d, want 200", path, recorder.Code)
		}
		body := recorder.Body.String()
		for _, want := range []string{
			"Continue with GitHub", "Continue with Microsoft", "Work email",
			// The page links to the canonical routes under either prefix.
			`action="/auth/login/continue"`, `href="/auth/login/continue?provider=oidc`,
		} {
			if !strings.Contains(body, want) {
				t.Errorf("%s: page lacks %q", path, want)
			}
		}
		if csp := recorder.Header().Get("Content-Security-Policy"); !strings.Contains(csp, "default-src 'none'") {
			t.Fatalf("%s: CSP = %q", path, csp)
		}
	}
}

// The single sign-on routes are all under /auth/, and the old prefix
// is gone: it answers 404, not a redirect.
func TestTheSSORoutesAreOnlyUnderAuth(t *testing.T) {
	routes := map[string]bool{}
	if err := chi.Walk(ssoRouter(t, gitHubAndEntra()).(chi.Routes), func(_, route string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
		routes[route] = true
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{
		ssoPaths.Login, ssoPaths.Continue, ssoPaths.Logout, ssoPaths.Info, ssoPaths.FormLogout,
		ssoPaths.OIDCLogin, ssoPaths.OIDCCallback, ssoPaths.OIDCLogout,
		ssoPaths.SAMLMetadata, ssoPaths.SAMLLogin, ssoPaths.SAMLACS, ssoPaths.SAMLLogout,
	} {
		if !strings.HasPrefix(path, "/auth/") || !routes[path] {
			t.Errorf("route %q is not registered under /auth/", path)
		}
	}
	for route := range routes {
		if strings.Contains(route, "forward") {
			t.Errorf("route %q still names the old prefix", route)
		}
	}
	// The edge auth check at /auth itself is unchanged.
	if !routes["/auth"] {
		t.Fatal("/auth is no longer registered")
	}
	router := ssoRouter(t, gitHubAndEntra())
	for _, old := range []string{"/forward-" + "auth/login", "/forward-" + "auth/auth_oidc/callback"} {
		recorder := httptest.NewRecorder()
		router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, old, nil))
		if recorder.Code != http.StatusNotFound {
			t.Errorf("%s answered %d, want 404", old, recorder.Code)
		}
	}
}

// The assertion consumer service is mounted as a POST route.
func TestTheSAMLAssertionConsumerIsMounted(t *testing.T) {
	router := ssoRouter(t, gitHubAndEntra())
	request := httptest.NewRequest(http.MethodPost, "/auth/saml/acs", strings.NewReader("SAMLResponse=x"))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	if recorder.Code == http.StatusNotFound || recorder.Code == http.StatusMethodNotAllowed {
		t.Fatalf("status = %d", recorder.Code)
	}
}

// The email form routes through the mounted continue route to the SAML login.
func TestTheSignInPageRoutesAWorkEmailToSAML(t *testing.T) {
	router := ssoRouter(t, gitHubAndEntra())
	form := url.Values{"email": {"alice@contoso.com"}, "target_to": {"/app/chat"}}
	request := httptest.NewRequest(http.MethodPost, "/auth/login/continue", strings.NewReader(form.Encode()))
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
	if location.Path != "/auth/saml/login" ||
		location.Query().Get("login_hint") != "alice@contoso.com" ||
		location.Query().Get("target_to") != "/app/chat" {
		t.Fatalf("Location = %s", location)
	}
}

// A SAML-only deployment now has a sign-in route. Before, the route was
// mounted only with an OIDC handler, so the SAML login was reachable only by
// typing its path.
func TestASAMLOnlyDeploymentRedirectsToTheSAMLLogin(t *testing.T) {
	providers := gitHubAndEntra()
	delete(providers, identityproviders.KindOIDC)
	router := ssoRouter(t, providers)
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/auth/login?target_to=%2Fapp", nil))

	if recorder.Code != http.StatusFound ||
		recorder.Header().Get("Location") != "/auth/saml/login?target_to=%2Fapp" {
		t.Fatalf("status = %d, Location = %q", recorder.Code, recorder.Header().Get("Location"))
	}
}
