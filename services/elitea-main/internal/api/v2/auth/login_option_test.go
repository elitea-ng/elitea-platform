package auth

// The sign-in page's view of each plane (login_option.go), and the login hint
// both login routes forward to the identity provider.

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"golang.org/x/oauth2"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/identityproviders"
)

// stubProviderSource answers Enabled from a map, or with one error.
type stubProviderSource struct {
	providers map[identityproviders.Kind]identityproviders.Provider
	err       error
}

func (s stubProviderSource) Enabled(_ context.Context, kind identityproviders.Kind) (identityproviders.Provider, error) {
	if s.err != nil {
		return identityproviders.Provider{}, s.err
	}
	provider, ok := s.providers[kind]
	if !ok {
		return identityproviders.Provider{}, identityproviders.ErrNotFound
	}
	return provider, nil
}

func envOnlyOIDCHandler() *OIDCHandler {
	return &OIDCHandler{
		secretKey:    "test-secret",
		runtimeCache: map[string]*oidcRuntime{},
		envRuntime: &oidcRuntime{
			oauth2Cfg: &oauth2.Config{
				ClientID:    "elitea",
				RedirectURL: "https://elitea.example.com/auth/oidc/callback",
				Endpoint:    oauth2.Endpoint{AuthURL: "https://dex.example.com/auth", TokenURL: "https://dex.example.com/token"},
				Scopes:      []string{"openid", "email"},
			},
			origin: "environment",
		},
		envOption: LoginOption{DisplayName: DefaultOIDCDisplayName},
	}
}

func TestAStoredOIDCProviderWinsOverTheEnvironmentOnThePage(t *testing.T) {
	handler := envOnlyOIDCHandler()
	handler.providers = stubProviderSource{providers: map[identityproviders.Kind]identityproviders.Provider{
		identityproviders.KindOIDC: {
			Key: "github", Kind: identityproviders.KindOIDC, DisplayName: "GitHub", Enabled: true,
			OIDC: &identityproviders.OIDCDocument{LoginDomains: []string{"example.com"}},
		},
	}}

	option, ok := handler.LoginOption(context.Background())
	if !ok || option.DisplayName != "GitHub" || len(option.LoginDomains) != 1 || option.LoginDomains[0] != "example.com" {
		t.Fatalf("option = %+v, ok = %v", option, ok)
	}
}

func TestTheEnvironmentOIDCProviderHasADefaultName(t *testing.T) {
	handler := envOnlyOIDCHandler()
	handler.providers = stubProviderSource{}

	option, ok := handler.LoginOption(context.Background())
	if !ok || option.DisplayName != DefaultOIDCDisplayName {
		t.Fatalf("option = %+v, ok = %v", option, ok)
	}
}

func TestTheEnvironmentOptionReadsItsOverrides(t *testing.T) {
	t.Setenv("OIDC_DISPLAY_NAME", "GitHub")
	t.Setenv("OIDC_LOGIN_DOMAINS", "Example.com, example.org")
	option := oidcEnvironmentOption()
	if option.DisplayName != "GitHub" || strings.Join(option.LoginDomains, ",") != "example.com,example.org" {
		t.Fatalf("option = %+v", option)
	}

	t.Setenv("OIDC_LOGIN_DOMAINS", "not a domain!")
	if option := oidcEnvironmentOption(); option.LoginDomains != nil {
		t.Fatalf("an invalid domain list was used: %+v", option)
	}
}

func TestNoOIDCProviderIsNotOffered(t *testing.T) {
	handler := &OIDCHandler{providers: stubProviderSource{}}
	if _, ok := handler.LoginOption(context.Background()); ok {
		t.Fatal("an OIDC plane with no provider was offered")
	}
	var nilHandler *OIDCHandler
	if _, ok := nilHandler.LoginOption(context.Background()); ok {
		t.Fatal("a nil OIDC handler was offered")
	}
}

// An unreadable table hides the plane on the page. It does not fall through
// to the environment, for the reason oidc_providers.go gives.
func TestAnUnreadableTableHidesTheOIDCPlane(t *testing.T) {
	handler := envOnlyOIDCHandler()
	handler.providers = stubProviderSource{err: errors.New("connection refused")}
	if _, ok := handler.LoginOption(context.Background()); ok {
		t.Fatal("an unreadable table fell through to the environment")
	}
}

func TestTheSAMLPlaneIsOfferedOnlyWithAnEnabledProvider(t *testing.T) {
	handler := &SAMLHandler{providers: stubProviderSource{}}
	if _, ok := handler.LoginOption(context.Background()); ok {
		t.Fatal("a SAML plane with no provider was offered")
	}

	handler.providers = stubProviderSource{providers: map[identityproviders.Kind]identityproviders.Provider{
		identityproviders.KindSAML: {
			Key: "entra", Kind: identityproviders.KindSAML, DisplayName: "Microsoft", Enabled: true,
			SAML: &identityproviders.SAMLDocument{LoginDomains: []string{"contoso.com"}},
		},
	}}
	option, ok := handler.LoginOption(context.Background())
	if !ok || option.DisplayName != "Microsoft" || option.LoginDomains[0] != "contoso.com" {
		t.Fatalf("option = %+v, ok = %v", option, ok)
	}
}

func TestOnlyAPlainAddressIsALoginHint(t *testing.T) {
	for raw, want := range map[string]string{
		"alice@contoso.com":                 "alice@contoso.com",
		" alice@contoso.com ":               "alice@contoso.com",
		"":                                  "",
		"alice":                             "",
		"Alice <alice@contoso.com>":         "",
		"alice@contoso.com\r\nX: y":         "",
		"a@b@contoso.com":                   "",
		strings.Repeat("a", 250) + "@x.com": "",
	} {
		if got := loginHint(raw); got != want {
			t.Errorf("loginHint(%q) = %q, want %q", raw, got, want)
		}
	}
}

// OIDC: the sign-in page's address reaches the identity provider as the
// `login_hint` authorization parameter, and the return target stays in state.
func TestTheOIDCLoginForwardsTheLoginHint(t *testing.T) {
	recorder := httptest.NewRecorder()
	envOnlyOIDCHandler().Login(recorder, httptest.NewRequest(http.MethodGet,
		"/auth/oidc/login?target_to=%2Fapp%2Fchat&login_hint=alice%40example.com", nil))

	if recorder.Code != http.StatusFound {
		t.Fatalf("status = %d", recorder.Code)
	}
	location, err := url.Parse(recorder.Header().Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	if location.Host != "dex.example.com" || location.Query().Get("login_hint") != "alice@example.com" {
		t.Fatalf("Location = %s", location)
	}
	if !strings.HasSuffix(location.Query().Get("state"), "|/app/chat") {
		t.Fatalf("state = %q, want the return target", location.Query().Get("state"))
	}
	// Every login cookie is scoped to "/", so the callback route reads it.
	for _, cookie := range recorder.Result().Cookies() {
		if cookie.Path != "/" {
			t.Fatalf("cookie %s has Path %q, want /", cookie.Name, cookie.Path)
		}
	}

	// A value that is not an address is dropped, not forwarded.
	recorder = httptest.NewRecorder()
	envOnlyOIDCHandler().Login(recorder, httptest.NewRequest(http.MethodGet,
		"/auth/oidc/login?login_hint=%3Cscript%3E", nil))
	if strings.Contains(recorder.Header().Get("Location"), "login_hint") {
		t.Fatalf("an invalid hint was forwarded: %s", recorder.Header().Get("Location"))
	}
}

// The identity provider ended the login (the person cancelled). The browser
// goes back to the sign-in page with the error banner and its return target,
// not to a bare 400.
func TestAnIdentityProviderErrorReturnsToTheSignInPage(t *testing.T) {
	handler := envOnlyOIDCHandler()
	state := "nonce-1|/app/chat"
	request := httptest.NewRequest(http.MethodGet,
		"/auth/oidc/callback?error=access_denied&state="+url.QueryEscape(state), nil)
	request.AddCookie(&http.Cookie{Name: oidcStateCookie, Value: signBrowserValue(handler.secretKey, state)})
	recorder := httptest.NewRecorder()

	handler.Callback(recorder, request)

	if recorder.Code != http.StatusFound {
		t.Fatalf("status = %d, want 302", recorder.Code)
	}
	location, err := url.Parse(recorder.Header().Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	if location.Path != SignInPath || location.Query().Get("error") != "sso_failed" ||
		location.Query().Get("target_to") != "/app/chat" {
		t.Fatalf("Location = %s", location)
	}
}

// A callback with an error but no valid state is still refused: only a login
// this browser started is sent back to the page.
func TestAnIdentityProviderErrorWithoutStateIsStillRefused(t *testing.T) {
	recorder := httptest.NewRecorder()
	envOnlyOIDCHandler().Callback(recorder, httptest.NewRequest(http.MethodGet,
		"/auth/oidc/callback?error=access_denied", nil))
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", recorder.Code)
	}
}

// SAML: Microsoft Entra ID reads `login_hint` on its SAML endpoint. The hint
// is appended after the encoded request, which stays byte-for-byte intact.
func TestTheSAMLLoginForwardsTheLoginHint(t *testing.T) {
	handler := NewSAMLHandler(nil, "test-secret", stubProviderSource{providers: map[identityproviders.Kind]identityproviders.Provider{
		identityproviders.KindSAML: {
			Key: "entra", Kind: identityproviders.KindSAML, DisplayName: "Microsoft", Enabled: true, Revision: 1,
			SAML: &identityproviders.SAMLDocument{
				IDPEntityID:     "https://sts.windows.net/tenant/",
				IDPSSOURL:       "https://login.microsoftonline.com/tenant/saml2",
				IDPCertificates: []string{selfSignedCertificate(t)},
				SPEntityID:      "https://elitea.example.com",
				ACSURL:          "https://elitea.example.com/auth/saml/acs",
			},
		},
	}}, nil, true)

	recorder := httptest.NewRecorder()
	handler.Login(recorder, httptest.NewRequest(http.MethodGet,
		"/auth/saml/login?target_to=%2Fapp&login_hint=Alice%40contoso.com", nil))

	if recorder.Code != http.StatusFound {
		t.Fatalf("status = %d: %s", recorder.Code, recorder.Body.String())
	}
	raw := recorder.Header().Get("Location")
	location, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	if location.Host != "login.microsoftonline.com" || location.Query().Get("SAMLRequest") == "" {
		t.Fatalf("Location = %s", raw)
	}
	if location.Query().Get("login_hint") != "Alice@contoso.com" || !strings.HasSuffix(raw, "&login_hint=Alice%40contoso.com") {
		t.Fatalf("login_hint not appended: %s", raw)
	}
}

func selfSignedCertificate(t *testing.T) string {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "idp"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
}
