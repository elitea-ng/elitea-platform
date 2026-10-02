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

// With both planes usable, /auth/login is the sign-in page, and so is its
// deprecated alias /forward-auth/login.
func TestTheSSOPlaneServesTheSignInPage(t *testing.T) {
	router := ssoRouter(t, gitHubAndEntra())
	for _, path := range []string{"/auth/login", "/forward-auth/login"} {
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

// Every canonical single sign-on route has its deprecated alias, with the
// same methods, and the alias is the same handler rather than a redirect.
func TestEveryCanonicalAuthRouteKeepsItsForwardAuthAlias(t *testing.T) {
	routes := map[string]map[string]bool{}
	if err := chi.Walk(ssoRouter(t, gitHubAndEntra()).(chi.Routes), func(method, route string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
		if routes[route] == nil {
			routes[route] = map[string]bool{}
		}
		routes[route][method] = true
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	canonical := reflectPaths(canonicalSSOPaths)
	legacy := reflectPaths(legacySSOPaths)
	if len(canonical) != len(legacy) || len(canonical) < 12 {
		t.Fatalf("canonical %v, legacy %v", canonical, legacy)
	}
	for index, path := range canonical {
		if !strings.HasPrefix(path, "/auth/") || !strings.HasPrefix(legacy[index], "/forward-auth/") {
			t.Fatalf("path pair %q / %q has the wrong prefixes", path, legacy[index])
		}
		if len(routes[path]) == 0 || len(routes[legacy[index]]) == 0 {
			t.Fatalf("pair %q / %q: canonical %v, alias %v", path, legacy[index], routes[path], routes[legacy[index]])
		}
		for method := range routes[path] {
			if !routes[legacy[index]][method] {
				t.Errorf("%s %s has no %s alias at %s", method, path, method, legacy[index])
			}
		}
	}
	// The edge's forward-auth check at /auth itself is unchanged.
	if !routes["/auth"][http.MethodGet] {
		t.Fatal("GET /auth is no longer the forward-auth check")
	}
}

func reflectPaths(paths ssoBrowserPaths) []string {
	return []string{
		paths.Login, paths.Continue, paths.Logout, paths.Info, paths.FormLogout,
		paths.OIDCLogin, paths.OIDCCallback, paths.OIDCLogout,
		paths.SAMLMetadata, paths.SAMLLogin, paths.SAMLACS, paths.SAMLLogout,
	}
}

// An identity provider registered with the old ACS URL still POSTs there.
// Both URLs reach the same assertion consumer service; without a login
// request cookie both refuse the same way.
func TestTheSAMLAssertionConsumerAnswersUnderBothPrefixes(t *testing.T) {
	router := ssoRouter(t, gitHubAndEntra())
	codes := map[string]int{}
	for _, path := range []string{"/auth/saml/acs", "/forward-auth/auth_saml/acs"} {
		request := httptest.NewRequest(http.MethodPost, path, strings.NewReader("SAMLResponse=x"))
		request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		recorder := httptest.NewRecorder()
		router.ServeHTTP(recorder, request)
		codes[path] = recorder.Code
	}
	if codes["/auth/saml/acs"] == http.StatusNotFound || codes["/auth/saml/acs"] == http.StatusMethodNotAllowed ||
		codes["/auth/saml/acs"] != codes["/forward-auth/auth_saml/acs"] {
		t.Fatalf("codes = %v", codes)
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
