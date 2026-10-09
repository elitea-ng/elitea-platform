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

	apimw "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/middleware"
	browserapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/browserauth"
	forwardapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/edgeauth"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth/browserflow"
)

type coreCredentialFunc func(
	context.Context,
	forwardapp.Source,
	forwardapp.CredentialInput,
) (forwardapp.CredentialResult, error)

func (f coreCredentialFunc) AuthenticateCredential(
	ctx context.Context,
	source forwardapp.Source,
	credential forwardapp.CredentialInput,
) (forwardapp.CredentialResult, error) {
	return f(ctx, source, credential)
}

type coreSessionFunc func(context.Context, string) (browserapp.Authorization, error)

func (f coreSessionFunc) Authorize(ctx context.Context, sessionID string) (browserapp.Authorization, error) {
	return f(ctx, sessionID)
}

func TestCoreHandlerPreservesCredentialPrecedenceAndRPCHeaders(t *testing.T) {
	var credentials []forwardapp.CredentialInput
	handler := newCoreTestHandler(t,
		coreCredentialFunc(func(_ context.Context, _ forwardapp.Source, credential forwardapp.CredentialInput) (forwardapp.CredentialResult, error) {
			credentials = append(credentials, credential)
			if credential.Data == "authorization-token" {
				return acceptedCoreToken(), nil
			}
			return forwardapp.CredentialResult{Resolution: forwardapp.CredentialRejected}, nil
		}),
		panicCoreSession(t),
		nil,
	)
	request := coreRequest("/auth/check?target=rpc")
	request.Header.Set("Authorization", "bEaReR authorization-token")
	request.Header.Set("X-API-Key", "configured-token")
	request.AddCookie(&http.Cookie{Name: "centry_auth_session", Value: CookieValuePrefix + canonicalSessionID(4)})
	recorder := httptest.NewRecorder()

	handler.ServeHTTP(recorder, request)

	requireCoreOK(t, recorder)
	if len(credentials) != 1 || credentials[0].Type != "bearer" || credentials[0].Data != "authorization-token" {
		t.Fatalf("credentials = %+v", credentials)
	}
	requireHeaders(t, recorder.Header(), map[string]string{
		"X-Auth-Type":      "token",
		"X-Auth-ID":        "42",
		"X-Auth-User-ID":   "7",
		"X-Auth-Reference": "-",
	})
}

func TestCoreHandlerRejectedCredentialNeverTraversesBrowserSession(t *testing.T) {
	tests := []struct {
		name       string
		public     []forwardapp.PublicRule
		wantStatus int
		wantType   string
	}{
		{name: "private denial", wantStatus: http.StatusFound},
		{
			name: "current public override",
			public: []forwardapp.PublicRule{{
				Name:       "public API",
				Conditions: []forwardapp.RuleCondition{{Field: forwardapp.SourceURI, Pattern: `/api/public`}},
			}},
			wantStatus: http.StatusOK,
			wantType:   "public",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			handler := newCoreTestHandler(t,
				coreCredentialFunc(func(context.Context, forwardapp.Source, forwardapp.CredentialInput) (forwardapp.CredentialResult, error) {
					return forwardapp.CredentialResult{Resolution: forwardapp.CredentialRejected}, nil
				}),
				panicCoreSession(t),
				test.public,
			)
			request := coreRequest("/auth/check?target=rpc")
			request.Header.Set("X-Forwarded-Uri", "/api/public")
			request.Header.Set("Authorization", "malformed")
			request.Header.Set("X-API-Key", "would-be-valid")
			request.AddCookie(&http.Cookie{Name: "centry_auth_session", Value: CookieValuePrefix + canonicalSessionID(4)})
			recorder := httptest.NewRecorder()

			handler.ServeHTTP(recorder, request)

			if recorder.Code != test.wantStatus {
				t.Fatalf("status = %d, want %d; body=%q", recorder.Code, test.wantStatus, recorder.Body.String())
			}
			if test.wantStatus == http.StatusFound {
				if location := recorder.Header().Get("Location"); location != "/access_denied" {
					t.Fatalf("Location = %q", location)
				}
				return
			}
			if got := recorder.Header().Get("X-Auth-Type"); got != test.wantType {
				t.Fatalf("X-Auth-Type = %q, want %q", got, test.wantType)
			}
		})
	}
}

func TestCoreHandlerAuthorizesServerSideBrowserSessionWithoutForwardingBearerReference(t *testing.T) {
	sessionID := canonicalSessionID(5)
	handler := newCoreTestHandler(t,
		panicCoreCredential(t),
		coreSessionFunc(func(_ context.Context, gotID string) (browserapp.Authorization, error) {
			if gotID != sessionID {
				t.Fatalf("session ID = %q, want %q", gotID, sessionID)
			}
			return validCoreBrowserAuthorization(), nil
		}),
		nil,
	)
	request := coreRequest("/auth/check?target=rpc")
	request.AddCookie(&http.Cookie{Name: "centry_auth_session", Value: CookieValuePrefix + sessionID})
	recorder := httptest.NewRecorder()

	handler.ServeHTTP(recorder, request)

	requireCoreOK(t, recorder)
	requireHeaders(t, recorder.Header(), map[string]string{
		"X-Auth-Type":      "user",
		"X-Auth-ID":        "7",
		"X-Auth-User-ID":   "7",
		"X-Auth-Reference": "-",
	})
	if strings.Contains(recorder.Header().Get("X-Auth-Reference"), sessionID) {
		t.Fatal("browser bearer reference was forwarded")
	}
}

func TestCoreHandlerRPCOutputRoundTripsThroughTrustedForwardedMiddleware(t *testing.T) {
	handler := newCoreTestHandler(t,
		panicCoreCredential(t),
		coreSessionFunc(func(context.Context, string) (browserapp.Authorization, error) {
			return validCoreBrowserAuthorization(), nil
		}),
		nil,
	)
	forwardRequest := coreRequest("/auth/check?target=rpc")
	forwardRequest.AddCookie(&http.Cookie{
		Name:  "centry_auth_session",
		Value: CookieValuePrefix + canonicalSessionID(5),
	})
	forwardResponse := httptest.NewRecorder()
	handler.ServeHTTP(forwardResponse, forwardRequest)
	requireCoreOK(t, forwardResponse)

	var downstreamPrincipal auth.User
	downstream := apimw.Auth(apimw.AuthConfig{
		ForwardedIdentityVerifier: coreForwardedIdentityVerifierFunc(func(*http.Request) error { return nil }),
		PrincipalValidator: corePrincipalValidatorFunc(func(_ context.Context, principal auth.User) (auth.User, error) {
			if principal.ID != "7" || principal.UserID != "7" || principal.AuthType != "user" || principal.Email != "" {
				t.Fatalf("unvalidated forwarded principal = %+v", principal)
			}
			principal.Email = "authoritative@example.test"
			return principal, nil
		}),
	})(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		var ok bool
		downstreamPrincipal, ok = auth.UserFromContext(request.Context())
		if !ok {
			t.Fatal("downstream principal missing")
		}
		writer.WriteHeader(http.StatusNoContent)
	}))
	downstreamRequest := httptest.NewRequest(http.MethodGet, "/api/v2/projects/7", nil)
	for _, name := range []string{"X-Auth-Type", "X-Auth-ID", "X-Auth-User-ID", "X-Auth-Reference"} {
		downstreamRequest.Header.Set(name, forwardResponse.Header().Get(name))
	}
	downstreamResponse := httptest.NewRecorder()
	downstream.ServeHTTP(downstreamResponse, downstreamRequest)
	if downstreamResponse.Code != http.StatusNoContent || downstreamPrincipal.Email != "authoritative@example.test" {
		t.Fatalf("status=%d principal=%+v", downstreamResponse.Code, downstreamPrincipal)
	}
}

func TestCoreHandlerNoopMapperEmitsNoIdentityHeaders(t *testing.T) {
	handler := newCoreTestHandler(t,
		coreCredentialFunc(func(context.Context, forwardapp.Source, forwardapp.CredentialInput) (forwardapp.CredentialResult, error) {
			return acceptedCoreToken(), nil
		}),
		panicCoreSession(t),
		nil,
	)
	request := coreRequest("/auth/check")
	request.Header.Set("Authorization", "Bearer valid")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	requireCoreOK(t, recorder)
	for _, name := range []string{"X-Auth-Type", "X-Auth-ID", "X-Auth-User-ID", "X-Auth-Reference"} {
		if value := recorder.Header().Get(name); value != "" {
			t.Fatalf("%s = %q, want absent", name, value)
		}
	}
}

func TestCoreHandlerEmitsTrackedGrafanaProjectionWithoutSessionBearer(t *testing.T) {
	mapper := newTrackedSuccessMapper(t)
	sessionID := canonicalSessionID(8)
	handler := newCoreTestHandlerWithMapper(t,
		panicCoreCredential(t),
		coreSessionFunc(func(context.Context, string) (browserapp.Authorization, error) {
			authorization := validCoreBrowserAuthorization()
			authorization.ProviderAttributes = []byte(
				`{"nameid":"admin","attributes":{},"sessionindex":"sensitive-provider-session"}`,
			)
			return authorization, nil
		}),
		mapper,
	)
	request := coreRequest("/auth/check?target=header&scope=grafana")
	request.Header.Set("X-WEBAUTH-USER", "spoofed")
	request.AddCookie(&http.Cookie{Name: "centry_auth_session", Value: CookieValuePrefix + sessionID})
	recorder := httptest.NewRecorder()

	handler.ServeHTTP(recorder, request)

	requireCoreOK(t, recorder)
	if got := recorder.Header().Get("X-WEBAUTH-USER"); got != "admin" {
		t.Fatalf("X-WEBAUTH-USER = %q", got)
	}
	for _, name := range []string{
		"X-Auth-Reference",
		"X-Auth-Session-Id",
		"X-Auth-Session-Endpoint",
		"X-Auth-Session-Name",
	} {
		if value := recorder.Header().Get(name); value != "" {
			t.Fatalf("%s = %q, want absent", name, value)
		}
	}
	for name, values := range recorder.Header() {
		for _, value := range values {
			if strings.Contains(value, sessionID) || strings.Contains(value, "sensitive-provider-session") {
				t.Fatalf("response %s leaked session material: %q", name, value)
			}
		}
	}
}

func TestCoreHandlerHeaderMapperFailsClosedForUnsafeProviderReference(t *testing.T) {
	tests := map[string]struct {
		attributes string
		wantStatus int
	}{
		"missing":             {attributes: `{"attributes":{}}`, wantStatus: http.StatusServiceUnavailable},
		"non-string":          {attributes: `{"nameid":42}`, wantStatus: http.StatusServiceUnavailable},
		"leading whitespace":  {attributes: `{"nameid":" admin"}`, wantStatus: http.StatusServiceUnavailable},
		"trailing whitespace": {attributes: `{"nameid":"admin "}`, wantStatus: http.StatusServiceUnavailable},
		"control":             {attributes: `{"nameid":"admin\r\nX-Injected: true"}`, wantStatus: http.StatusServiceUnavailable},
		"oversized": {
			attributes: `{"nameid":"` + strings.Repeat("x", browserflow.MaxProviderReferenceBytes+1) + `"}`,
			wantStatus: http.StatusServiceUnavailable,
		},
		"duplicate": {
			attributes: `{"nameid":"first","nameid":"second"}`,
			wantStatus: http.StatusServiceUnavailable,
		},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			handler := newCoreTestHandlerWithMapper(t,
				panicCoreCredential(t),
				coreSessionFunc(func(context.Context, string) (browserapp.Authorization, error) {
					authorization := validCoreBrowserAuthorization()
					authorization.ProviderAttributes = []byte(test.attributes)
					return authorization, nil
				}),
				newTrackedSuccessMapper(t),
			)
			request := coreRequest("/auth/check?target=header&scope=grafana")
			request.AddCookie(&http.Cookie{
				Name: "centry_auth_session", Value: CookieValuePrefix + canonicalSessionID(10),
			})
			recorder := httptest.NewRecorder()
			handler.ServeHTTP(recorder, request)
			if recorder.Code != test.wantStatus || recorder.Header().Get("X-WEBAUTH-USER") != "" {
				t.Fatalf("status=%d headers=%v body=%q", recorder.Code, recorder.Header(), recorder.Body.String())
			}
			for _, header := range []string{
				"X-Auth-Reference", "X-Auth-Session-Id", "X-Auth-Session-Endpoint", "X-Auth-Session-Name",
			} {
				if value := recorder.Header().Get(header); value != "" {
					t.Fatalf("%s = %q", header, value)
				}
			}
		})
	}
}

func TestCoreHandlerOptionalMappersFailClosedOutsideTrackedHeaderScope(t *testing.T) {
	mapper := newTrackedSuccessMapper(t)
	handler := newCoreTestHandlerWithMapper(t,
		panicCoreCredential(t),
		coreSessionFunc(func(context.Context, string) (browserapp.Authorization, error) {
			authorization := validCoreBrowserAuthorization()
			authorization.ProviderAttributes = []byte(`{"nameid":"admin"}`)
			return authorization, nil
		}),
		mapper,
	)
	for _, query := range []string{
		"target=header",
		"target=header&scope=",
		"target=header&scope=unknown",
		"target=header&scope=grafana&scope=unknown",
		"target=json&scope=galloper",
	} {
		t.Run(query, func(t *testing.T) {
			request := coreRequest("/auth/check?" + query)
			request.AddCookie(&http.Cookie{
				Name: "centry_auth_session", Value: CookieValuePrefix + canonicalSessionID(9),
			})
			recorder := httptest.NewRecorder()
			handler.ServeHTTP(recorder, request)
			if recorder.Code != http.StatusFound || recorder.Header().Get("Location") != "/access_denied" ||
				recorder.Header().Get("X-WEBAUTH-USER") != "" || recorder.Header().Get("X-Auth-Session-Id") != "" {
				t.Fatalf("status=%d headers=%v body=%q", recorder.Code, recorder.Header(), recorder.Body.String())
			}
		})
	}
}

func TestCoreHandlerJSONTransportRemainsAbsentForEveryAuthenticationType(t *testing.T) {
	tests := []struct {
		name    string
		handler *CoreHandler
		prepare func(*http.Request)
	}{
		{
			name: "token",
			handler: newCoreTestHandlerWithMapper(t,
				coreCredentialFunc(func(context.Context, forwardapp.Source, forwardapp.CredentialInput) (forwardapp.CredentialResult, error) {
					return acceptedCoreToken(), nil
				}),
				panicCoreSession(t),
				newTrackedSuccessMapper(t),
			),
			prepare: func(request *http.Request) { request.Header.Set("Authorization", "Bearer valid") },
		},
		{
			name: "browser",
			handler: newCoreTestHandlerWithMapper(t,
				panicCoreCredential(t),
				coreSessionFunc(func(context.Context, string) (browserapp.Authorization, error) {
					authorization := validCoreBrowserAuthorization()
					authorization.ProviderAttributes = []byte(`{"nameid":"admin"}`)
					return authorization, nil
				}),
				newTrackedSuccessMapper(t),
			),
			prepare: func(request *http.Request) {
				request.AddCookie(&http.Cookie{
					Name: "centry_auth_session", Value: CookieValuePrefix + canonicalSessionID(11),
				})
			},
		},
		{
			name: "public",
			handler: func() *CoreHandler {
				policy, err := forwardapp.NewPublicPolicy([]forwardapp.PublicRule{{
					Name: "public", Conditions: []forwardapp.RuleCondition{{Field: forwardapp.SourceURI, Pattern: `/api/private.*`}},
				}})
				if err != nil {
					t.Fatal(err)
				}
				kernel, err := forwardapp.NewKernel(panicCoreCredential(t), panicCoreSession(t), policy)
				if err != nil {
					t.Fatal(err)
				}
				handler := newCoreTestHandlerWithKernel(t, kernel)
				handler.mappers = newTrackedSuccessMapper(t)
				return handler
			}(),
			prepare: func(*http.Request) {},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request := coreRequest("/auth/check?target=json&scope=galloper")
			test.prepare(request)
			recorder := httptest.NewRecorder()
			test.handler.ServeHTTP(recorder, request)
			if recorder.Code != http.StatusFound || recorder.Header().Get("Location") != "/access_denied" {
				t.Fatalf("status=%d headers=%v body=%q", recorder.Code, recorder.Header(), recorder.Body.String())
			}
			for _, header := range []string{
				"X-Auth-Reference", "X-Auth-Session-Id", "X-Auth-Session-Endpoint", "X-Auth-Session-Name",
			} {
				if value := recorder.Header().Get(header); value != "" {
					t.Fatalf("%s = %q", header, value)
				}
			}
		})
	}
}

func TestCoreHandlerMissingOrExpiredSessionUsesSafeLoginRedirect(t *testing.T) {
	tests := []struct {
		name    string
		cookie  bool
		session coreSessionFunc
	}{
		{name: "missing cookie", session: panicCoreSession(t)},
		{
			name:   "expired session",
			cookie: true,
			session: func(context.Context, string) (browserapp.Authorization, error) {
				return browserapp.Authorization{}, browserapp.ErrAuthenticationExpired
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			handler := newCoreTestHandler(t, panicCoreCredential(t), test.session, nil)
			request := coreRequest("/auth/check")
			if test.cookie {
				request.AddCookie(&http.Cookie{Name: "centry_auth_session", Value: CookieValuePrefix + canonicalSessionID(6)})
			}
			recorder := httptest.NewRecorder()

			handler.ServeHTTP(recorder, request)

			if recorder.Code != http.StatusFound {
				t.Fatalf("status = %d, want 302", recorder.Code)
			}
			location, err := url.Parse(recorder.Header().Get("Location"))
			if err != nil || location.Path != "/auth/login" ||
				location.Query().Get("target_to") != "/api/private?project=7" {
				t.Fatalf("Location = %q, parsed=%v error=%v", recorder.Header().Get("Location"), location, err)
			}
			if test.cookie && len(recorder.Header().Values("Set-Cookie")) != 1 {
				t.Fatalf("Set-Cookie = %q, want expired cookie", recorder.Header().Values("Set-Cookie"))
			}
		})
	}
}

func TestCoreHandlerDependencyFailureNeverDowngradesToPublic(t *testing.T) {
	handler := newCoreTestHandler(t,
		panicCoreCredential(t),
		coreSessionFunc(func(context.Context, string) (browserapp.Authorization, error) {
			return browserapp.Authorization{}, errors.New("dependency unavailable")
		}),
		[]forwardapp.PublicRule{{
			Name:       "public",
			Conditions: []forwardapp.RuleCondition{{Field: forwardapp.SourceURI, Pattern: `.*`}},
		}},
	)
	request := coreRequest("/auth/check?target=rpc")
	request.AddCookie(&http.Cookie{Name: "centry_auth_session", Value: CookieValuePrefix + canonicalSessionID(7)})
	recorder := httptest.NewRecorder()

	handler.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusServiceUnavailable || recorder.Header().Get("Location") != "" ||
		recorder.Header().Get("X-Auth-Type") != "" {
		t.Fatalf("status=%d headers=%v body=%q", recorder.Code, recorder.Header(), recorder.Body.String())
	}
}

func TestCoreHandlerRejectsUnknownAndExplicitEmptyMapperTargets(t *testing.T) {
	for _, target := range []string{"target=", "target=unknown", "target=rpc&target=json"} {
		t.Run(target, func(t *testing.T) {
			handler := newCoreTestHandler(t,
				coreCredentialFunc(func(context.Context, forwardapp.Source, forwardapp.CredentialInput) (forwardapp.CredentialResult, error) {
					return acceptedCoreToken(), nil
				}),
				panicCoreSession(t),
				nil,
			)
			request := coreRequest("/auth/check?" + target)
			request.Header.Set("Authorization", "Bearer valid")
			recorder := httptest.NewRecorder()
			handler.ServeHTTP(recorder, request)
			if recorder.Code != http.StatusFound || recorder.Header().Get("Location") != "/access_denied" {
				t.Fatalf("status=%d Location=%q", recorder.Code, recorder.Header().Get("Location"))
			}
		})
	}
}

func TestCoreHandlerRejectsUntrustedProxyBeforeAuthorization(t *testing.T) {
	handler := newCoreTestHandler(t, panicCoreCredential(t), panicCoreSession(t), nil)
	request := coreRequest("/auth/check")
	request.RemoteAddr = "192.0.2.99:443"
	recorder := httptest.NewRecorder()

	handler.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusFound || recorder.Header().Get("Location") != "/access_denied" {
		t.Fatalf("status=%d Location=%q", recorder.Code, recorder.Header().Get("Location"))
	}
}

func TestCoreHandlerRoutesPreserveEffectiveMethods(t *testing.T) {
	handler := newCoreTestHandler(t, panicCoreCredential(t), panicCoreSession(t), nil)
	router := handler.Routes()

	for _, test := range []struct {
		method string
		want   int
	}{
		{method: http.MethodGet, want: http.StatusFound},
		{method: http.MethodHead, want: http.StatusFound},
		{method: http.MethodOptions, want: http.StatusOK},
		{method: http.MethodPost, want: http.StatusBadRequest},
	} {
		t.Run(test.method, func(t *testing.T) {
			request := coreRequest(AuthPath)
			request.Method = test.method
			recorder := httptest.NewRecorder()
			router.ServeHTTP(recorder, request)
			if recorder.Code != test.want {
				t.Fatalf("status = %d, want %d", recorder.Code, test.want)
			}
			if test.method == http.MethodHead && recorder.Body.Len() != 0 {
				t.Fatalf("HEAD body = %q", recorder.Body.String())
			}
		})
	}
}

type corePrincipalValidatorFunc func(context.Context, auth.User) (auth.User, error)

func (f corePrincipalValidatorFunc) ValidatePrincipal(ctx context.Context, principal auth.User) (auth.User, error) {
	return f(ctx, principal)
}

type coreForwardedIdentityVerifierFunc func(*http.Request) error

func (f coreForwardedIdentityVerifierFunc) VerifyForwardedIdentityPeer(request *http.Request) error {
	return f(request)
}

func newCoreTestHandler(
	t *testing.T,
	credentials forwardapp.CredentialAuthenticator,
	sessions forwardapp.SessionAuthorizer,
	rules []forwardapp.PublicRule,
) *CoreHandler {
	t.Helper()
	policy, err := forwardapp.NewPublicPolicy(rules)
	if err != nil {
		t.Fatal(err)
	}
	kernel, err := forwardapp.NewKernel(credentials, sessions, policy)
	if err != nil {
		t.Fatal(err)
	}
	return newCoreTestHandlerWithKernel(t, kernel)
}

func newCoreTestHandlerWithKernel(t *testing.T, kernel *forwardapp.Kernel) *CoreHandler {
	t.Helper()
	resolver, err := NewTrustedProxyResolver(TrustedProxyConfig{
		TrustedProxyCIDRs:        []string{"10.0.0.0/8"},
		PublicOrigin:             "https://elitea.example.test",
		IdentityProjectionSecret: []byte("0123456789abcdef0123456789abcdef"),
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
	handler, err := NewCoreHandler(kernel, resolver, cookies, CoreConfig{
		CredentialHeaders: []CredentialHeader{{Name: "X-API-Key", Type: "bearer"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return handler
}

func newCoreTestHandlerWithMapper(
	t *testing.T,
	credentials forwardapp.CredentialAuthenticator,
	sessions forwardapp.SessionAuthorizer,
	mapper *SuccessMapper,
) *CoreHandler {
	t.Helper()
	policy, err := forwardapp.NewPublicPolicy(nil)
	if err != nil {
		t.Fatal(err)
	}
	kernel, err := forwardapp.NewKernel(credentials, sessions, policy)
	if err != nil {
		t.Fatal(err)
	}
	resolver, err := NewTrustedProxyResolver(TrustedProxyConfig{
		TrustedProxyCIDRs:        []string{"10.0.0.0/8"},
		PublicOrigin:             "https://elitea.example.test",
		IdentityProjectionSecret: []byte("0123456789abcdef0123456789abcdef"),
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
	handler, err := NewCoreHandler(kernel, resolver, cookies, CoreConfig{Mappers: mapper})
	if err != nil {
		t.Fatal(err)
	}
	return handler
}

func newTrackedSuccessMapper(t *testing.T) *SuccessMapper {
	t.Helper()
	mapper, err := NewSuccessMapper(MapperContractTrackedV1)
	if err != nil {
		t.Fatal(err)
	}
	return mapper
}

func coreRequest(target string) *http.Request {
	request := httptest.NewRequest(http.MethodGet, target, nil)
	request.RemoteAddr = "10.1.2.3:43120"
	request.Header.Set("X-Forwarded-Method", http.MethodGet)
	request.Header.Set("X-Forwarded-Proto", "https")
	request.Header.Set("X-Forwarded-Host", "elitea.example.test")
	request.Header.Set("X-Forwarded-Uri", "/api/private?project=7")
	request.Header.Set("X-Forwarded-For", "198.51.100.8")
	return request
}

func acceptedCoreToken() forwardapp.CredentialResult {
	return forwardapp.CredentialResult{
		Resolution: forwardapp.CredentialAccepted,
		Principal: auth.User{
			ID:       "7",
			UserID:   "7",
			TokenID:  "42",
			Email:    "owner@example.test",
			AuthType: "token",
		},
	}
}

func validCoreBrowserAuthorization() browserapp.Authorization {
	return browserapp.Authorization{
		Principal: auth.User{
			ID:       "7",
			UserID:   "7",
			Email:    "owner@example.test",
			AuthType: "session",
		},
		Provider:           "form",
		ProviderAttributes: []byte(`{}`),
	}
}

func panicCoreCredential(t *testing.T) coreCredentialFunc {
	t.Helper()
	return func(context.Context, forwardapp.Source, forwardapp.CredentialInput) (forwardapp.CredentialResult, error) {
		t.Fatal("credential authenticator called")
		return forwardapp.CredentialResult{}, nil
	}
}

func panicCoreSession(t *testing.T) coreSessionFunc {
	t.Helper()
	return func(context.Context, string) (browserapp.Authorization, error) {
		t.Fatal("session authorizer called")
		return browserapp.Authorization{}, nil
	}
}

func requireCoreOK(t *testing.T, recorder *httptest.ResponseRecorder) {
	t.Helper()
	if recorder.Code != http.StatusOK || recorder.Body.String() != "OK" {
		t.Fatalf("status=%d body=%q", recorder.Code, recorder.Body.String())
	}
}

func requireHeaders(t *testing.T, headers http.Header, want map[string]string) {
	t.Helper()
	for name, value := range want {
		if got := headers.Get(name); got != value {
			t.Fatalf("%s = %q, want %q", name, got, value)
		}
	}
}
