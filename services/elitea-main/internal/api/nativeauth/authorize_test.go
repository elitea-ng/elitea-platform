package nativeauth_test

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/browserauth"
	nativeapi "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/nativeauth"
	domain "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/nativeauth"
)

// The /authorize validation order (RFC 6749 §4.1.2.1): every refusal that
// could be an open redirect is a PAGE; everything after the client and
// redirect are trusted is a REDIRECT carrying state and iss. None of these
// cases reaches the store, so no database is needed.
func TestAuthorizeRefusalsArePagesOrRedirects(t *testing.T) {
	handler := nativeapi.New(nativeapi.Config{
		Registry:     domain.NewRegistry([]domain.Client{fileClient()}, nil),
		PublicOrigin: testOrigin,
		Pages:        browserauth.NewNativePages(nil),
	})
	router := chi.NewRouter()
	handler.Mount(router)

	pages := map[string]string{
		"unknown client":       authorizeQuery(map[string]string{"client_id": "dev.elitea.unknown"}),
		"missing redirect":     authorizeQuery(map[string]string{"redirect_uri": ""}),
		"unregistered":         authorizeQuery(map[string]string{"redirect_uri": "dev.elitea.conformance:/other"}),
		"open redirect":        authorizeQuery(map[string]string{"redirect_uri": "https://evil.example/cb"}),
		"missing state":        authorizeQuery(map[string]string{"state": ""}),
		"control in state":     authorizeQuery(map[string]string{"state": "a\nb"}),
		"repeated client":      authorizeQuery(nil) + "&client_id=dev.elitea.conformance",
		"repeated redirect":    authorizeQuery(nil) + "&redirect_uri=" + url.QueryEscape(testRedirect),
		"oversized state":      authorizeQuery(map[string]string{"state": strings.Repeat("s", 513)}),
		"disabled-like client": authorizeQuery(map[string]string{"client_id": ""}),
	}
	for name, target := range pages {
		recorder := httptest.NewRecorder()
		router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, target, nil))
		if recorder.Code != http.StatusBadRequest || recorder.Header().Get("Location") != "" ||
			!strings.HasPrefix(recorder.Header().Get("Content-Type"), "text/html") {
			t.Fatalf("%s: %d location=%q; want an HTML page and never a redirect", name, recorder.Code, recorder.Header().Get("Location"))
		}
	}

	redirects := map[string]struct{ target, code string }{
		"response type":      {authorizeQuery(map[string]string{"response_type": "token"}), "unsupported_response_type"},
		"plain pkce":         {authorizeQuery(map[string]string{"code_challenge_method": "plain"}), "invalid_request"},
		"missing pkce":       {authorizeQuery(map[string]string{"code_challenge_method": ""}), "invalid_request"},
		"short challenge":    {authorizeQuery(map[string]string{"code_challenge": "abc"}), "invalid_request"},
		"device name":        {authorizeQuery(map[string]string{"device_name": strings.Repeat("d", 65)}), "invalid_request"},
		"platform":           {authorizeQuery(map[string]string{"platform": "beos"}), "invalid_request"},
		"client version":     {authorizeQuery(map[string]string{"client_version": "1.0 beta"}), "invalid_request"},
		"repeated parameter": {authorizeQuery(nil) + "&platform=ios", "invalid_request"},
	}
	for name, c := range redirects {
		recorder := httptest.NewRecorder()
		router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, c.target, nil))
		location, err := url.Parse(recorder.Header().Get("Location"))
		if recorder.Code != http.StatusFound || err != nil ||
			!strings.HasPrefix(location.String(), testRedirect+"?") {
			t.Fatalf("%s: %d %q; want a redirect to the registered URI", name, recorder.Code, recorder.Header().Get("Location"))
		}
		query := location.Query()
		if query.Get("error") != c.code || query.Get("state") != "state-123" || query.Get("iss") != testOrigin {
			t.Fatalf("%s: redirect query = %v", name, query)
		}
	}
}

func TestAuthorizeRefusesWithoutAPublicOrigin(t *testing.T) {
	handler := nativeapi.New(nativeapi.Config{
		Registry: domain.NewRegistry([]domain.Client{fileClient()}, nil),
		Pages:    browserauth.NewNativePages(nil),
	})
	router := chi.NewRouter()
	handler.Mount(router)
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, authorizeQuery(nil), nil))
	if recorder.Code != http.StatusServiceUnavailable || recorder.Header().Get("Location") != "" {
		t.Fatalf("authorize without DEPLOYMENT_URL = %d %q", recorder.Code, recorder.Header().Get("Location"))
	}
}

func TestTokenEndpointRefusesMalformedRequestsBeforeTheStore(t *testing.T) {
	handler := nativeapi.New(nativeapi.Config{
		Registry: domain.NewRegistry([]domain.Client{fileClient()}, nil), PublicOrigin: testOrigin,
	})
	router := chi.NewRouter()
	handler.Mount(router)
	cases := map[string]struct {
		body, contentType, authorization, code string
		status                                 int
	}{
		"json body":       {`{"grant_type":"refresh_token"}`, "application/json", "", "invalid_request", 400},
		"repeated":        {"grant_type=refresh_token&grant_type=refresh_token", "application/x-www-form-urlencoded", "", "invalid_request", 400},
		"password grant":  {"grant_type=password&client_id=" + testClientID, "application/x-www-form-urlencoded", "", "unsupported_grant_type", 400},
		"missing grant":   {"client_id=" + testClientID, "application/x-www-form-urlencoded", "", "invalid_request", 400},
		"client secret":   {"grant_type=refresh_token&client_id=" + testClientID, "application/x-www-form-urlencoded", "Basic eDp5", "invalid_request", 400},
		"unknown client":  {"grant_type=authorization_code&client_id=dev.elitea.nobody&code=x&redirect_uri=x&code_verifier=" + testVerifier, "application/x-www-form-urlencoded", "", "invalid_client", 401},
		"missing refresh": {"grant_type=refresh_token&client_id=" + testClientID, "application/x-www-form-urlencoded", "", "invalid_request", 400},
	}
	for name, c := range cases {
		request := httptest.NewRequest(http.MethodPost, nativeapi.TokenPath, strings.NewReader(c.body))
		request.Header.Set("Content-Type", c.contentType)
		if c.authorization != "" {
			request.Header.Set("Authorization", c.authorization)
		}
		recorder := httptest.NewRecorder()
		router.ServeHTTP(recorder, request)
		if recorder.Code != c.status || !strings.Contains(recorder.Body.String(), `"error":"`+c.code+`"`) ||
			recorder.Header().Get("Cache-Control") != "no-store" {
			t.Fatalf("%s = %d %s", name, recorder.Code, recorder.Body.String())
		}
	}
}

func TestConsentPageEscapesAndPinsFormAction(t *testing.T) {
	pages := browserauth.NewNativePages(nil)
	recorder := httptest.NewRecorder()
	pages.RenderConsent(recorder, httptest.NewRequest(http.MethodGet, "/", nil), browserauth.NativeConsent{
		ClientName: "Agent", ClientID: testClientID, Email: "a@example.test",
		DeviceName: `<script>alert(1)</script>`, Platform: "ios", Origin: testOrigin,
		Action: nativeapi.DecisionPath, Request: "handle", UserID: "7",
		SwitchAccountURL: "/auth/logout?target_to=%2Fx", FormAction: domain.FormActionSource(testRedirect),
	})
	body := recorder.Body.String()
	if strings.Contains(body, "<script>alert(1)</script>") || !strings.Contains(body, "&lt;script&gt;") {
		t.Fatal("the device name claim must be escaped")
	}
	csp := recorder.Header().Get("Content-Security-Policy")
	if !strings.Contains(csp, "form-action 'self' dev.elitea.conformance:;") || !strings.Contains(csp, "frame-ancestors 'none'") {
		t.Fatalf("CSP = %q", csp)
	}
	if recorder.Header().Get("X-Frame-Options") != "DENY" || recorder.Header().Get("Referrer-Policy") != "no-referrer" ||
		recorder.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("headers = %v", recorder.Header())
	}
	for _, hook := range []string{`id="native-consent-form"`, `data-testid="native-consent-allow"`, `data-testid="native-consent-deny"`} {
		if !strings.Contains(body, hook) {
			t.Fatalf("missing test hook %s", hook)
		}
	}
	loopback := httptest.NewRecorder()
	pages.RenderConsent(loopback, httptest.NewRequest(http.MethodGet, "/", nil), browserauth.NativeConsent{
		FormAction: domain.FormActionSource("http://127.0.0.1:4000/callback"),
	})
	if !strings.Contains(loopback.Header().Get("Content-Security-Policy"), "form-action 'self' http://127.0.0.1:* http://[::1]:*;") {
		t.Fatalf("loopback CSP = %q", loopback.Header().Get("Content-Security-Policy"))
	}
}

type fixedAddress string

func (a fixedAddress) Resolve(*http.Request) (string, bool) { return string(a), true }

// A known address is blocked BEFORE any lookup once it failed too often.
func TestTokenFailuresAreRateLimitedPerClientAndAddress(t *testing.T) {
	handler := nativeapi.New(nativeapi.Config{
		Registry:     domain.NewRegistry([]domain.Client{fileClient()}, nil),
		PublicOrigin: testOrigin,
		Addresses:    fixedAddress("198.51.100.7"),
	})
	router := chi.NewRouter()
	handler.Mount(router)
	body := "grant_type=authorization_code&client_id=dev.elitea.nobody&code=x&redirect_uri=x&code_verifier=" + testVerifier
	var last *httptest.ResponseRecorder
	for attempt := 1; attempt <= 21; attempt++ {
		request := httptest.NewRequest(http.MethodPost, nativeapi.TokenPath, strings.NewReader(body))
		request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		last = httptest.NewRecorder()
		router.ServeHTTP(last, request)
		if attempt < 20 && last.Code != http.StatusUnauthorized {
			t.Fatalf("attempt %d = %d, want 401 invalid_client", attempt, last.Code)
		}
	}
	if last.Code != http.StatusTooManyRequests || last.Header().Get("Retry-After") == "" {
		t.Fatalf("21st failure = %d Retry-After=%q, want 429", last.Code, last.Header().Get("Retry-After"))
	}
}

// Discovery's native_auth: the endpoints, absolute against the configured
// issuer, while at least one client is enabled; null otherwise.
func TestDiscoverySeamPublishesEndpointsOnlyWhileAClientIsEnabled(t *testing.T) {
	enabled := nativeapi.New(nativeapi.Config{
		Registry: domain.NewRegistry([]domain.Client{fileClient()}, nil), PublicOrigin: testOrigin,
	})
	got, err := enabled.NativeAuth(t.Context(), "https://ignored.example")
	if err != nil || got == nil || got.Issuer != testOrigin ||
		got.TokenEndpoint != testOrigin+nativeapi.TokenPath ||
		got.AuthorizationEndpoint != testOrigin+nativeapi.AuthorizePath ||
		got.RevocationEndpoint != testOrigin+nativeapi.RevokePath ||
		len(got.CodeChallengeMethodsSupported) != 1 || got.CodeChallengeMethodsSupported[0] != "S256" {
		t.Fatalf("native_auth = %+v, %v", got, err)
	}
	off := fileClient()
	off.Enabled = false
	disabled := nativeapi.New(nativeapi.Config{
		Registry: domain.NewRegistry([]domain.Client{off}, nil), PublicOrigin: testOrigin,
	})
	if got, err := disabled.NativeAuth(t.Context(), testOrigin); err != nil || got != nil {
		t.Fatalf("native_auth with only a disabled client = %+v, %v; want null", got, err)
	}
}
