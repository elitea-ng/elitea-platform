package browserauth

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	browserapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/browserauth"
	forwardapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/edgeauth"
)

func TestMainHandlerTraversesRejectedCredentialIntoBrowserSession(t *testing.T) {
	sessionID := canonicalSessionID(21)
	handler := newMainTestHandler(t,
		coreCredentialFunc(func(context.Context, forwardapp.Source, forwardapp.CredentialInput) (forwardapp.CredentialResult, error) {
			return forwardapp.CredentialResult{Resolution: forwardapp.CredentialRejected}, nil
		}),
		coreSessionFunc(func(_ context.Context, got string) (browserapp.Authorization, error) {
			if got != sessionID {
				t.Fatalf("session ID = %q, want %q", got, sessionID)
			}
			authorization := validCoreBrowserAuthorization()
			authorization.ProviderAttributes = []byte(
				`{"attributes":{"picture":"https://images.example.test/current-avatar.png"}}`,
			)
			return authorization, nil
		}),
		nil,
	)
	request := mainRequest("/api/v2/social/author")
	// A configured credential header, not an Authorization bearer: a
	// bearer deny is a 401 (TestMainHandlerAnswersBearerDenialsWith401).
	request.Header.Set("X-API-Key", "rejected")
	request.Header.Set("X-Auth-Type", "user")
	request.Header.Set("X-Auth-ID", "999999")
	request.Header.Set(MainAvatarStateHeader, mainAvatarValue)
	request.Header.Set(MainAvatarHeader, "https://attacker.example/forged.png")
	request.AddCookie(&http.Cookie{Name: "centry_auth_session", Value: CookieValuePrefix + sessionID})
	recorder := httptest.NewRecorder()

	handler.ServeHTTP(recorder, request)

	requireCoreOK(t, recorder)
	requireHeaders(t, recorder.Header(), map[string]string{
		"X-Auth-Type":         "user",
		"X-Auth-ID":           "7",
		"X-Auth-User-ID":      "7",
		"X-Auth-Reference":    "-",
		MainAvatarStateHeader: mainAvatarValue,
		MainAvatarHeader:      "https://images.example.test/current-avatar.png",
	})
}

func TestMainHandlerProjectsAcceptedPATAndBoundsDecisionTime(t *testing.T) {
	handler := newMainTestHandler(t,
		coreCredentialFunc(func(ctx context.Context, _ forwardapp.Source, _ forwardapp.CredentialInput) (forwardapp.CredentialResult, error) {
			deadline, ok := ctx.Deadline()
			remaining := time.Until(deadline)
			if !ok || remaining <= 0 || remaining > mainDecisionTimeout {
				t.Fatalf("authorization deadline ok=%v remaining=%s", ok, remaining)
			}
			return acceptedCoreToken(), nil
		}),
		panicCoreSession(t),
		nil,
	)
	request := mainRequest("/api/v2/private")
	request.Header.Set("Authorization", "Bearer accepted")
	recorder := httptest.NewRecorder()

	handler.ServeHTTP(recorder, request)

	requireCoreOK(t, recorder)
	requireHeaders(t, recorder.Header(), map[string]string{
		"X-Auth-Type":         "token",
		"X-Auth-ID":           "42",
		"X-Auth-User-ID":      "7",
		"X-Auth-Reference":    "-",
		MainAvatarStateHeader: mainAvatarNone,
		MainAvatarHeader:      "-",
	})
}

// An EdgeAuth response is consumed by the EDGE, not by the browser, and an
// edge resolves a relative Location against the address it called — this
// service's internal one. Traefik handed browsers
// http://elitea-main.elitea.svc.cluster.local:8080/auth/login?... and
// they answered ERR_NAME_NOT_RESOLVED, because that name exists only inside
// the cluster.
//
// So both redirects this handler emits must be absolute against the origin a
// browser actually reaches, whenever the deployment names one.
func TestMainHandlerRedirectsAreAbsoluteAgainstThePublicOrigin(t *testing.T) {
	handler := newMainTestHandlerWithOrigin(t,
		coreCredentialFunc(func(context.Context, forwardapp.Source, forwardapp.CredentialInput) (forwardapp.CredentialResult, error) {
			return forwardapp.CredentialResult{Resolution: forwardapp.CredentialRejected}, nil
		}),
		panicCoreSession(t),
		nil,
		"https://elitea.example.test",
	)
	request := mainRequest("/api/v2/private")
	// A configured credential header, not an Authorization bearer: a
	// bearer deny is a 401 (TestMainHandlerAnswersBearerDenialsWith401).
	request.Header.Set("X-API-Key", "rejected")
	recorder := httptest.NewRecorder()

	handler.ServeHTTP(recorder, request)

	location := recorder.Header().Get("Location")
	if recorder.Code != http.StatusFound || location != "https://elitea.example.test/app/access_denied" {
		t.Fatalf("response = %d location=%q", recorder.Code, location)
	}
}

func TestMainHandlerLoginRedirectIsAbsoluteAgainstThePublicOrigin(t *testing.T) {
	// Same shape as TestMainHandlerRedirectsPrivateAnonymousRequestToFormLogin:
	// an anonymous request to a private route produces the login decision.
	handler := newMainTestHandlerWithOrigin(t, panicCoreCredential(t), panicCoreSession(t), nil,
		"https://elitea.example.test")
	recorder := httptest.NewRecorder()

	handler.ServeHTTP(recorder, mainRequest("/api/v2/social/author"))

	location, err := url.Parse(recorder.Header().Get("Location"))
	if err != nil {
		t.Fatalf("location parse error = %v", err)
	}
	if location.Scheme != "https" || location.Host != "elitea.example.test" {
		t.Fatalf("location = %q, want an absolute URL on the public origin", location)
	}
	if location.Path != BasePath+LoginPath {
		t.Fatalf("location path = %q, want %q", location.Path, BasePath+LoginPath)
	}
	// The caller must come back to where it was, not to the origin root.
	if got := location.Query().Get("target_to"); got != "/api/v2/social/author" {
		t.Fatalf("target_to = %q", got)
	}
}

// An empty public origin keeps the previous relative form, for a caller that
// reaches this handler directly rather than through an edge.
func TestMainHandlerRedirectStaysRelativeWithoutAPublicOrigin(t *testing.T) {
	handler := newMainTestHandler(t,
		coreCredentialFunc(func(context.Context, forwardapp.Source, forwardapp.CredentialInput) (forwardapp.CredentialResult, error) {
			return forwardapp.CredentialResult{Resolution: forwardapp.CredentialRejected}, nil
		}),
		panicCoreSession(t),
		nil,
	)
	request := mainRequest("/api/v2/private")
	// A configured credential header, not an Authorization bearer: a
	// bearer deny is a 401 (TestMainHandlerAnswersBearerDenialsWith401).
	request.Header.Set("X-API-Key", "rejected")
	recorder := httptest.NewRecorder()

	handler.ServeHTTP(recorder, request)

	if got := recorder.Header().Get("Location"); got != "/app/access_denied" {
		t.Fatalf("location = %q, want the relative target", got)
	}
}

func TestMainHandlerRedirectsRejectedCredentialToAccessDenied(t *testing.T) {
	handler := newMainTestHandler(t,
		coreCredentialFunc(func(context.Context, forwardapp.Source, forwardapp.CredentialInput) (forwardapp.CredentialResult, error) {
			return forwardapp.CredentialResult{Resolution: forwardapp.CredentialRejected}, nil
		}),
		panicCoreSession(t),
		nil,
	)
	// A browser-shaped caller (a credential in a configured header, no
	// Authorization bearer) still gets the access-denied page.
	request := mainRequest("/api/v2/private")
	request.Header.Set("X-API-Key", "rejected")
	recorder := httptest.NewRecorder()

	handler.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusFound || recorder.Header().Get("Location") != "/app/access_denied" {
		t.Fatalf("response = %d location=%q", recorder.Code, recorder.Header().Get("Location"))
	}
}

// ADR-0025 WP3 (section 4.6): a refused BEARER caller is a program, so the
// deny is a 401 it can read, and a revoked native device session is the ADR's
// flat device_revoked body rather than a redirect to a page.
func TestMainHandlerAnswersBearerDenialsWith401(t *testing.T) {
	for name, c := range map[string]struct {
		resolution forwardapp.CredentialResolution
		wantBody   string
	}{
		"rejected": {forwardapp.CredentialRejected, `"code":"token_rejected"`},
		"revoked":  {forwardapp.CredentialRevoked, `"error":"device_revoked"`},
	} {
		resolution := c.resolution
		handler := newMainTestHandler(t,
			coreCredentialFunc(func(context.Context, forwardapp.Source, forwardapp.CredentialInput) (forwardapp.CredentialResult, error) {
				return forwardapp.CredentialResult{Resolution: resolution}, nil
			}),
			panicCoreSession(t),
			nil,
		)
		request := mainRequest("/api/v2/private")
		request.Header.Set("Authorization", "Bearer elnat_x")
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, request)
		if recorder.Code != http.StatusUnauthorized || recorder.Header().Get("Location") != "" ||
			!strings.Contains(recorder.Body.String(), c.wantBody) ||
			!strings.HasPrefix(recorder.Header().Get("WWW-Authenticate"), "Bearer") {
			t.Fatalf("%s: %d %q %s", name, recorder.Code, recorder.Header().Get("Location"), recorder.Body.String())
		}
	}
}

func TestMainHandlerEmitsCompletePublicIdentityFromExactPolicy(t *testing.T) {
	handler := newMainTestHandler(t, panicCoreCredential(t), panicCoreSession(t), []forwardapp.PublicRule{{
		Name:       "public health",
		Conditions: []forwardapp.RuleCondition{{Field: forwardapp.SourceURI, Pattern: `/healthz`}},
	}})
	request := mainRequest("/healthz")
	request.Header.Set("X-Auth-Type", "user")
	request.Header.Set("X-Auth-ID", "999999")
	recorder := httptest.NewRecorder()

	handler.ServeHTTP(recorder, request)

	requireCoreOK(t, recorder)
	requireHeaders(t, recorder.Header(), map[string]string{
		"X-Auth-Type":         "public",
		"X-Auth-ID":           "-",
		"X-Auth-User-ID":      "-",
		"X-Auth-Reference":    "-",
		MainAvatarStateHeader: mainAvatarNone,
		MainAvatarHeader:      "-",
	})
}

func TestMainHandlerProjectsAuthoritativeNullAvatar(t *testing.T) {
	sessionID := canonicalSessionID(23)
	handler := newMainTestHandler(t,
		panicCoreCredential(t),
		coreSessionFunc(func(context.Context, string) (browserapp.Authorization, error) {
			authorization := validCoreBrowserAuthorization()
			authorization.ProviderAttributes = []byte(`{"attributes":{}}`)
			return authorization, nil
		}),
		nil,
	)
	request := mainRequest("/api/v2/social/author")
	request.Header.Set(MainAvatarStateHeader, mainAvatarValue)
	request.Header.Set(MainAvatarHeader, "https://attacker.example/forged.png")
	request.AddCookie(&http.Cookie{Name: "centry_auth_session", Value: CookieValuePrefix + sessionID})
	recorder := httptest.NewRecorder()

	handler.ServeHTTP(recorder, request)

	requireCoreOK(t, recorder)
	requireHeaders(t, recorder.Header(), map[string]string{
		MainAvatarStateHeader: mainAvatarNone,
		MainAvatarHeader:      "-",
	})
}

func TestMainHandlerMarksUntransportableAvatarProjectionUnavailable(t *testing.T) {
	tests := map[string]string{
		"non scalar":     `{"attributes":{"picture":["one","two"]}}`,
		"control":        `{"attributes":{"picture":"https://example.test/a\nb"}}`,
		"tab":            `{"attributes":{"picture":"https://example.test/a\tb"}}`,
		"leading space":  `{"attributes":{"picture":" https://example.test/a"}}`,
		"trailing space": `{"attributes":{"picture":"https://example.test/a "}}`,
		"non ASCII":      `{"attributes":{"picture":"https://example.test/аватар"}}`,
		"sentinel":       `{"attributes":{"picture":"-"}}`,
		"oversized":      `{"attributes":{"picture":"` + strings.Repeat("x", maxMainAvatarBytes+1) + `"}}`,
	}
	for name, attributes := range tests {
		t.Run(name, func(t *testing.T) {
			sessionID := canonicalSessionID(byte(len(name) + 40))
			handler := newMainTestHandler(t,
				panicCoreCredential(t),
				coreSessionFunc(func(context.Context, string) (browserapp.Authorization, error) {
					authorization := validCoreBrowserAuthorization()
					authorization.ProviderAttributes = []byte(attributes)
					return authorization, nil
				}),
				nil,
			)
			request := mainRequest("/api/v2/social/author")
			request.AddCookie(&http.Cookie{Name: "centry_auth_session", Value: CookieValuePrefix + sessionID})
			recorder := httptest.NewRecorder()

			handler.ServeHTTP(recorder, request)

			if recorder.Code != http.StatusOK ||
				recorder.Header().Get(MainAvatarStateHeader) != mainAvatarUnavailable ||
				recorder.Header().Get(MainAvatarHeader) != "-" {
				t.Fatalf("response = %d headers=%v", recorder.Code, recorder.Header())
			}
		})
	}
}

var (
	benchmarkAvatarState string
	benchmarkAvatar      string
	benchmarkAvatarOK    bool
)

func BenchmarkMainAvatarProjection(b *testing.B) {
	tests := map[string]string{
		"common": `{"attributes":{"picture":"https://images.example.test/avatar.png"}}`,
		"30KiB attributes": `{"padding":"` + strings.Repeat("x", 30<<10) +
			`","attributes":{"picture":"https://images.example.test/avatar.png"}}`,
	}
	for name, providerAttributes := range tests {
		b.Run(name, func(b *testing.B) {
			decision := validSuccessMapperDecision(providerAttributes)
			b.SetBytes(int64(len(providerAttributes)))
			b.ReportAllocs()
			b.ResetTimer()
			for range b.N {
				benchmarkAvatarState, benchmarkAvatar, benchmarkAvatarOK =
					mainAvatarProjection(decision)
			}
		})
	}
}

func TestMainHandlerFixesRPCProjectionAndRejectsCallerQuery(t *testing.T) {
	handler := newMainTestHandler(t, panicCoreCredential(t), panicCoreSession(t), nil)
	for _, rawQuery := range []string{"target=header", "target=rpc", "scope=admin"} {
		t.Run(rawQuery, func(t *testing.T) {
			request := mainRequest("/private")
			request.URL.RawQuery = rawQuery
			recorder := httptest.NewRecorder()
			handler.ServeHTTP(recorder, request)
			if recorder.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want %d", recorder.Code, http.StatusBadRequest)
			}
		})
	}
}

func TestMainHandlerFailsClosedAtGatewayAndDependencies(t *testing.T) {
	wantDependency := errors.New("credential database unavailable")
	handler := newMainTestHandler(t,
		coreCredentialFunc(func(context.Context, forwardapp.Source, forwardapp.CredentialInput) (forwardapp.CredentialResult, error) {
			return forwardapp.CredentialResult{}, wantDependency
		}),
		panicCoreSession(t),
		nil,
	)
	dependency := mainRequest("/private")
	dependency.Header.Set("Authorization", "Bearer unavailable")
	dependencyRecorder := httptest.NewRecorder()
	handler.ServeHTTP(dependencyRecorder, dependency)
	if dependencyRecorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("dependency status = %d, want %d", dependencyRecorder.Code, http.StatusServiceUnavailable)
	}

	untrusted := mainRequest("/private")
	untrusted.RemoteAddr = "203.0.113.8:43210"
	untrusted.Header.Set("X-Auth-Type", "user")
	untrusted.Header.Set("X-Auth-ID", "7")
	untrustedRecorder := httptest.NewRecorder()
	handler.ServeHTTP(untrustedRecorder, untrusted)
	if untrustedRecorder.Code != http.StatusForbidden || untrustedRecorder.Header().Get("X-Auth-Type") != "" {
		t.Fatalf("untrusted response = %d headers=%v", untrustedRecorder.Code, untrustedRecorder.Header())
	}
}

func TestMainHandlerNeverDowngradesSessionDependencyFailureToPublic(t *testing.T) {
	wantDependency := errors.New("session store unavailable")
	handler := newMainTestHandler(t,
		panicCoreCredential(t),
		coreSessionFunc(func(context.Context, string) (browserapp.Authorization, error) {
			return browserapp.Authorization{}, wantDependency
		}),
		[]forwardapp.PublicRule{{
			Name:       "public health",
			Conditions: []forwardapp.RuleCondition{{Field: forwardapp.SourceURI, Pattern: `/healthz`}},
		}},
	)
	request := mainRequest("/healthz")
	request.AddCookie(&http.Cookie{
		Name:  "centry_auth_session",
		Value: CookieValuePrefix + canonicalSessionID(22),
	})
	recorder := httptest.NewRecorder()

	handler.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusServiceUnavailable || recorder.Header().Get("X-Auth-Type") != "" {
		t.Fatalf("dependency response = %d headers=%v", recorder.Code, recorder.Header())
	}
}

func TestMainHandlerRedirectsPrivateAnonymousRequestToFormLogin(t *testing.T) {
	handler := newMainTestHandler(t, panicCoreCredential(t), panicCoreSession(t), nil)
	request := mainRequest("/api/v2/projects/7?view=full")
	request.Header.Set("X-Auth-Type", "user")
	request.Header.Set("X-Auth-ID", "999999")
	request.Header.Set("X-Auth-Reference", "forged")
	recorder := httptest.NewRecorder()

	handler.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusFound {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusFound)
	}
	location, err := url.Parse(recorder.Header().Get("Location"))
	if err != nil || location.Path != "/auth/login" ||
		location.Query().Get("target_to") != "/api/v2/projects/7?view=full" {
		t.Fatalf("location = %q, parse error=%v", recorder.Header().Get("Location"), err)
	}
	if recorder.Header().Get("X-Auth-Type") != "" || recorder.Header().Get("X-Auth-ID") != "" ||
		recorder.Header().Get("X-Auth-Reference") != "" {
		t.Fatalf("forged inbound identity escaped into response: %v", recorder.Header())
	}
}

func newMainTestHandlerWithOrigin(
	t *testing.T,
	credentialAuthenticator forwardapp.CredentialAuthenticator,
	sessionAuthorizer forwardapp.SessionAuthorizer,
	rules []forwardapp.PublicRule,
	publicOrigin string,
) *MainHandler {
	t.Helper()
	handler := newMainTestHandler(t, credentialAuthenticator, sessionAuthorizer, rules)
	handler.publicOrigin = strings.TrimSuffix(publicOrigin, "/")
	return handler
}

func newMainTestHandler(
	t *testing.T,
	credentialAuthenticator forwardapp.CredentialAuthenticator,
	sessionAuthorizer forwardapp.SessionAuthorizer,
	rules []forwardapp.PublicRule,
) *MainHandler {
	t.Helper()
	policy, err := forwardapp.NewPublicPolicy(rules)
	if err != nil {
		t.Fatal(err)
	}
	kernel, err := forwardapp.NewKernel(credentialAuthenticator, sessionAuthorizer, policy)
	if err != nil {
		t.Fatal(err)
	}
	resolver, err := NewTrustedProxyResolver(TrustedProxyConfig{
		TrustedProxyCIDRs: []string{"10.0.0.0/8"},
		PublicOrigin:      "https://elitea.example.test",
	})
	if err != nil {
		t.Fatal(err)
	}
	cookies, err := NewCookiePolicy(CookieConfig{
		Name:     "centry_auth_session",
		Secure:   true,
		SameSite: http.SameSiteLaxMode,
		Lifetime: 7 * 24 * time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	handler, err := NewMainHandler(kernel, resolver, cookies, MainConfig{
		CredentialHeaders:  []CredentialHeader{{Name: "X-API-Key", Type: "bearer"}},
		AccessDeniedTarget: "/app/access_denied",
	})
	if err != nil {
		t.Fatal(err)
	}
	return handler
}

func mainRequest(uri string) *http.Request {
	request := httptest.NewRequest(http.MethodGet, MainEdgeAuthPath, nil)
	request.RemoteAddr = "10.1.2.3:43120"
	request.Header.Set("X-Forwarded-Method", http.MethodGet)
	request.Header.Set("X-Forwarded-Proto", "https")
	request.Header.Set("X-Forwarded-Host", "elitea.example.test")
	request.Header.Set("X-Forwarded-Uri", uri)
	request.Header.Set("X-Forwarded-For", "198.51.100.8")
	return request
}
