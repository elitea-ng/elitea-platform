package auth_test

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/browserauth"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/middleware"
	v2auth "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/auth"
	identity "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth"
)

type forwardHeaderFixture struct {
	name  string
	value string
}

type tokenValidatorFunc func(context.Context, string) (identity.User, error)

func (f tokenValidatorFunc) ValidateToken(ctx context.Context, token string) (identity.User, error) {
	return f(ctx, token)
}

type principalValidatorFunc func(context.Context, identity.User) (identity.User, error)

func (f principalValidatorFunc) ValidatePrincipal(ctx context.Context, user identity.User) (identity.User, error) {
	return f(ctx, user)
}

func TestEdgeAuthRequiresCurrentBaselineTraefikHeaders(t *testing.T) {
	for _, missing := range currentBaselineTraefikHeaders() {
		t.Run(missing.name, func(t *testing.T) {
			validated := false
			forward := v2auth.NewEdgeAuthHandler(tokenValidatorFunc(func(context.Context, string) (identity.User, error) {
				validated = true
				return validatedTokenUser(), nil
			}))
			req := httptest.NewRequest(http.MethodGet, "/auth?target=rpc", nil)
			addCurrentBaselineTraefikHeaders(req, missing.name)
			req.Header.Set("Authorization", "Bearer signed-token")
			rec := httptest.NewRecorder()

			forward.ServeHTTP(rec, req)

			requireAccessDenied(t, rec)
			if validated {
				t.Fatal("validator was called before the Traefik boundary was established")
			}
		})
	}
}

func TestEdgeAuthTraefikHeadersRequirePresenceNotContent(t *testing.T) {
	forward := v2auth.NewEdgeAuthHandler(tokenValidatorFunc(func(context.Context, string) (identity.User, error) {
		return validatedTokenUser(), nil
	}))
	req := httptest.NewRequest(http.MethodGet, "/auth", nil)
	for _, header := range currentBaselineTraefikHeaders() {
		req.Header.Set(header.name, "")
	}
	req.Header.Set("Authorization", "Bearer signed-token")
	rec := httptest.NewRecorder()

	forward.ServeHTTP(rec, req)

	requireOK(t, rec)
}

func TestEdgeAuthAuthorizationPrecedesAdditionalCredentialHeaders(t *testing.T) {
	var validatedToken string
	forward := v2auth.NewEdgeAuthHandler(
		tokenValidatorFunc(func(_ context.Context, token string) (identity.User, error) {
			validatedToken = token
			return validatedTokenUser(), nil
		}),
		v2auth.WithEdgeAuthCredentialHeaders(v2auth.EdgeAuthCredentialHeader{
			Name:           "X-API-Key",
			CredentialType: "bearer",
		}),
	)
	req := newEdgeAuthRequest("/auth")
	req.Header.Set("Authorization", "bEaReR authorization-token")
	req.Header.Set("X-API-Key", "additional-header-token")
	rec := httptest.NewRecorder()

	forward.ServeHTTP(rec, req)

	requireOK(t, rec)
	if validatedToken != "authorization-token" {
		t.Fatalf("validated token = %q, want Authorization credential", validatedToken)
	}
}

func TestEdgeAuthMalformedAuthorizationDoesNotTraverseToAdditionalHeader(t *testing.T) {
	validated := false
	forward := v2auth.NewEdgeAuthHandler(
		tokenValidatorFunc(func(context.Context, string) (identity.User, error) {
			validated = true
			return validatedTokenUser(), nil
		}),
		v2auth.WithEdgeAuthCredentialHeaders(v2auth.EdgeAuthCredentialHeader{
			Name:           "X-API-Key",
			CredentialType: "bearer",
		}),
	)
	req := newEdgeAuthRequest("/auth")
	// Header presence, including an empty value, takes precedence in the current
	// baseline and therefore fails instead of falling through.
	req.Header.Set("Authorization", "")
	req.Header.Set("X-API-Key", "additional-header-token")
	rec := httptest.NewRecorder()

	forward.ServeHTTP(rec, req)

	requireAccessDenied(t, rec)
	if validated {
		t.Fatal("validator was called for malformed Authorization")
	}
}

func TestEdgeAuthCredentialHandlers(t *testing.T) {
	basic := base64.StdEncoding.EncodeToString([]byte("basic-token:ignored-password"))
	invalidUTF8 := base64.StdEncoding.EncodeToString([]byte{0xff, ':', 'x'})
	tests := []struct {
		name          string
		authorization string
		wantToken     string
		wantStatus    int
	}{
		{name: "mixed case bearer", authorization: "BeArEr bearer-token", wantToken: "bearer-token", wantStatus: http.StatusOK},
		{name: "mixed case basic", authorization: "bAsIc " + basic, wantToken: "basic-token", wantStatus: http.StatusOK},
		{name: "missing separator", authorization: "Bearer", wantStatus: http.StatusForbidden},
		{name: "unsupported scheme", authorization: "Digest data", wantStatus: http.StatusForbidden},
		{name: "invalid base64", authorization: "Basic !!!", wantStatus: http.StatusForbidden},
		{name: "basic missing colon", authorization: "Basic " + base64.StdEncoding.EncodeToString([]byte("token-only")), wantStatus: http.StatusForbidden},
		{name: "basic invalid utf8", authorization: "Basic " + invalidUTF8, wantStatus: http.StatusForbidden},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var gotToken string
			forward := v2auth.NewEdgeAuthHandler(tokenValidatorFunc(func(_ context.Context, token string) (identity.User, error) {
				gotToken = token
				return validatedTokenUser(), nil
			}))
			req := newEdgeAuthRequest("/auth")
			req.Header.Set("Authorization", test.authorization)
			rec := httptest.NewRecorder()

			forward.ServeHTTP(rec, req)

			if test.wantStatus == http.StatusOK {
				requireOK(t, rec)
			} else {
				requireAccessDenied(t, rec)
			}
			if gotToken != test.wantToken {
				t.Fatalf("validated token = %q, want %q", gotToken, test.wantToken)
			}
		})
	}
}

func TestEdgeAuthAdditionalCredentialHeadersAreExplicitAndOrdered(t *testing.T) {
	t.Run("not trusted by default", func(t *testing.T) {
		validated := false
		forward := v2auth.NewEdgeAuthHandler(tokenValidatorFunc(func(context.Context, string) (identity.User, error) {
			validated = true
			return validatedTokenUser(), nil
		}))
		req := newEdgeAuthRequest("/auth")
		req.Header.Set("X-API-Key", "api-key-token")
		rec := httptest.NewRecorder()

		forward.ServeHTTP(rec, req)

		requireAccessDenied(t, rec)
		if validated {
			t.Fatal("unconfigured X-API-Key was trusted")
		}
	})

	t.Run("configuration order", func(t *testing.T) {
		var gotToken string
		forward := v2auth.NewEdgeAuthHandler(
			tokenValidatorFunc(func(_ context.Context, token string) (identity.User, error) {
				gotToken = token
				return validatedTokenUser(), nil
			}),
			v2auth.WithEdgeAuthCredentialHeaders(
				v2auth.EdgeAuthCredentialHeader{Name: "X-First-Key", CredentialType: "bearer"},
				v2auth.EdgeAuthCredentialHeader{Name: "X-Second-Key", CredentialType: "bearer"},
			),
		)
		req := newEdgeAuthRequest("/auth")
		req.Header.Set("X-First-Key", "first-token")
		req.Header.Set("X-Second-Key", "second-token")
		rec := httptest.NewRecorder()

		forward.ServeHTTP(rec, req)

		requireOK(t, rec)
		if gotToken != "first-token" {
			t.Fatalf("validated token = %q, want first configured header", gotToken)
		}
	})
}

// SEC-12: this route is a credential check only. No mapper target is
// registered, so a request for an identity projection is refused and no
// response, accepted or refused, carries an X-Auth-* header.
func TestEdgeAuthSuccessTargetContract(t *testing.T) {
	tests := []struct {
		name       string
		path       string
		wantStatus int
	}{
		{name: "target omitted answers the credential check", path: "/auth", wantStatus: http.StatusOK},
		{name: "rpc target is refused: no projection is offered", path: "/auth?target=rpc", wantStatus: http.StatusForbidden},
		{name: "header target is refused", path: "/auth?target=header", wantStatus: http.StatusForbidden},
		{name: "empty target is refused", path: "/auth?target=", wantStatus: http.StatusForbidden},
		{name: "unknown target is refused", path: "/auth?target=unknown", wantStatus: http.StatusForbidden},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			forward := v2auth.NewEdgeAuthHandler(tokenValidatorFunc(func(context.Context, string) (identity.User, error) {
				return validatedTokenUser(), nil
			}))
			req := newEdgeAuthRequest(test.path)
			req.Header.Set("Authorization", "Bearer signed-token")
			rec := httptest.NewRecorder()

			forward.ServeHTTP(rec, req)

			if test.wantStatus == http.StatusOK {
				requireOK(t, rec)
			} else {
				requireAccessDenied(t, rec)
			}
			requireNoIdentityProjection(t, rec)
		})
	}
}

// forgedIdentityHeaders is a caller-chosen identity projection, as a client
// or a tenant-configured outbound request could send it.
var forgedIdentityHeaders = map[string]string{
	"X-Auth-Type":      "user",
	"X-Auth-ID":        "1",
	"X-Auth-User-ID":   "1",
	"X-Auth-Reference": "-",
	"X-Auth-Signature": "v1.9999999999.AAAA",
}

func requireNoIdentityProjection(t *testing.T, rec *httptest.ResponseRecorder) {
	t.Helper()
	for name := range rec.Header() {
		if strings.HasPrefix(strings.ToLower(name), "x-auth-") {
			t.Fatalf("response carries %s=%q; this route projects no identity", name, rec.Header().Get(name))
		}
	}
}

// SEC-12: identity headers a caller sends to the legacy route are neither
// trusted nor reflected, with or without a credential and with any target.
func TestEdgeAuthLegacyRouteIgnoresForgedIdentityHeaders(t *testing.T) {
	validated := false
	forward := v2auth.NewEdgeAuthHandler(tokenValidatorFunc(func(_ context.Context, token string) (identity.User, error) {
		validated = true
		if token != "signed-token" {
			return identity.User{}, identity.ErrCredentialRejected
		}
		return validatedTokenUser(), nil
	}))
	for _, tc := range []struct {
		name          string
		path          string
		authorization string
		wantStatus    int
	}{
		{"forged headers alone", "/auth", "", http.StatusForbidden},
		{"forged headers alone, rpc target", "/auth?target=rpc", "", http.StatusForbidden},
		{"forged headers beside a rejected bearer", "/auth?target=rpc", "Bearer someone-else", http.StatusForbidden},
		{"forged headers beside a valid bearer, rpc target", "/auth?target=rpc", "Bearer signed-token", http.StatusForbidden},
		{"forged headers beside a valid bearer", "/auth", "Bearer signed-token", http.StatusOK},
	} {
		t.Run(tc.name, func(t *testing.T) {
			validated = false
			req := newEdgeAuthRequest(tc.path)
			for name, value := range forgedIdentityHeaders {
				req.Header.Set(name, value)
			}
			if tc.authorization != "" {
				req.Header.Set("Authorization", tc.authorization)
			}
			rec := httptest.NewRecorder()
			forward.ServeHTTP(rec, req)
			if tc.wantStatus == http.StatusOK {
				requireOK(t, rec)
			} else {
				requireAccessDenied(t, rec)
			}
			requireNoIdentityProjection(t, rec)
			if tc.authorization == "" && validated {
				t.Fatal("forged identity headers reached the credential validator")
			}
		})
	}
}

// SEC-12: the unsigned projection this route used to emit is refused by
// Main's auth middleware with the real forwarded-identity verifier, even from
// a socket peer inside trusted_proxy_cidrs: the request is unauthenticated.
func TestEdgeAuthLegacyProjectionShapeIsRefusedDownstream(t *testing.T) {
	resolver, err := browserauth.NewTrustedProxyResolver(browserauth.TrustedProxyConfig{
		TrustedProxyCIDRs:        []string{"10.0.0.0/8"},
		PublicOrigin:             "https://elitea.example.test",
		IdentityProjectionSecret: []byte(strings.Repeat("k", 32)),
	})
	if err != nil {
		t.Fatal(err)
	}
	reached := false
	authMiddleware := middleware.Auth(middleware.AuthConfig{
		ForwardedIdentityVerifier: resolver,
		PrincipalValidator: principalValidatorFunc(func(_ context.Context, user identity.User) (identity.User, error) {
			return user, nil
		}),
	})(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		reached = true
		w.WriteHeader(http.StatusNoContent)
	}))
	for _, projection := range []map[string]string{
		// The shape the route answered with for target=rpc.
		{"X-Auth-Type": "token", "X-Auth-ID": "42", "X-Auth-User-ID": "7", "X-Auth-Reference": "-"},
		forgedIdentityHeaders,
	} {
		req := httptest.NewRequest(http.MethodGet, "/api/v2/configurations/7", nil)
		req.RemoteAddr = "10.1.2.3:41000"
		for name, value := range projection {
			req.Header.Set(name, value)
		}
		rec := httptest.NewRecorder()
		authMiddleware.ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized || reached {
			t.Fatalf("unsigned projection %v: status=%d reached=%v, want 401 and no handler", projection, rec.Code, reached)
		}
	}
}

func TestEdgeAuthFailsClosedWhenValidatorOmitsTypedTokenIdentity(t *testing.T) {
	for _, user := range []identity.User{
		{ID: "7", UserID: "7", AuthType: "token"},
		{ID: "42", TokenID: "42", AuthType: "token"},
	} {
		forward := v2auth.NewEdgeAuthHandler(tokenValidatorFunc(func(context.Context, string) (identity.User, error) {
			return user, nil
		}))
		req := newEdgeAuthRequest("/auth?target=rpc")
		req.Header.Set("Authorization", "Bearer signed-token")
		rec := httptest.NewRecorder()

		forward.ServeHTTP(rec, req)

		requireAccessDenied(t, rec)
	}
}

func TestEdgeAuthFailsClosedWithoutAWorkingValidator(t *testing.T) {
	tests := []struct {
		name      string
		validator middleware.TokenValidator
	}{
		{name: "validator not configured"},
		{
			name: "validator rejects token",
			validator: tokenValidatorFunc(func(context.Context, string) (identity.User, error) {
				return identity.User{}, errors.New("test-only validator detail")
			}),
		},
	}

	for _, test := range tests {
		forward := v2auth.NewEdgeAuthHandler(test.validator)
		req := newEdgeAuthRequest("/auth?target=rpc")
		req.Header.Set("Authorization", "Bearer signed-token")
		rec := httptest.NewRecorder()

		forward.ServeHTTP(rec, req)

		requireAccessDenied(t, rec)
		if rec.Body.String() == "test-only validator detail" {
			t.Fatal("validator detail was exposed to the client")
		}
	}
}

func newEdgeAuthRequest(path string) *http.Request {
	req := httptest.NewRequest(http.MethodGet, path, nil)
	addCurrentBaselineTraefikHeaders(req, "")
	return req
}

func addCurrentBaselineTraefikHeaders(req *http.Request, excluded string) {
	for _, header := range currentBaselineTraefikHeaders() {
		if header.name != excluded {
			req.Header.Set(header.name, header.value)
		}
	}
}

func currentBaselineTraefikHeaders() [5]forwardHeaderFixture {
	return [5]forwardHeaderFixture{
		{name: "X-Forwarded-Method", value: http.MethodGet},
		{name: "X-Forwarded-Proto", value: "https"},
		{name: "X-Forwarded-Host", value: "elitea.example.test"},
		{name: "X-Forwarded-Uri", value: "/api/v2/configurations"},
		{name: "X-Forwarded-For", value: "192.0.2.10"},
	}
}

func validatedTokenUser() identity.User {
	return identity.User{
		ID:       "7",
		UserID:   "7",
		TokenID:  "42",
		Email:    "owner@example.test",
		AuthType: "token",
	}
}

func requireOK(t *testing.T, rec *httptest.ResponseRecorder) {
	t.Helper()
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body=%q", rec.Code, http.StatusOK, rec.Body.String())
	}
	if rec.Body.String() != "OK" {
		t.Fatalf("body = %q, want OK", rec.Body.String())
	}
	requireSecurityHeaders(t, rec)
}

func requireAccessDenied(t *testing.T, rec *httptest.ResponseRecorder) {
	t.Helper()
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want %d; body=%q", rec.Code, http.StatusForbidden, rec.Body.String())
	}
	if rec.Body.String() != "Access Denied" {
		t.Fatalf("body = %q, want Access Denied", rec.Body.String())
	}
	requireSecurityHeaders(t, rec)
}

func requireSecurityHeaders(t *testing.T, rec *httptest.ResponseRecorder) {
	t.Helper()
	if got := rec.Header().Get("Cache-Control"); got != "no-store" {
		t.Fatalf("Cache-Control = %q, want no-store", got)
	}
	if got := rec.Header().Get("Pragma"); got != "no-cache" {
		t.Fatalf("Pragma = %q, want no-cache", got)
	}
	if got := rec.Header().Get("Content-Type"); got != "text/html; charset=utf-8" {
		t.Fatalf("Content-Type = %q, want text/html; charset=utf-8", got)
	}
}

// ADR-0025 WP3: Traefik relays this endpoint's refusal to the client, so a
// revoked native device session must read as device_revoked here too.
func TestEdgeAuthAnswersDeviceRevokedForARevokedNativeToken(t *testing.T) {
	forward := v2auth.NewEdgeAuthHandler(tokenValidatorFunc(func(context.Context, string) (identity.User, error) {
		return identity.User{}, fmt.Errorf("wrapped: %w", identity.ErrDeviceRevoked)
	}))
	req := newEdgeAuthRequest("/auth")
	req.Header.Set("Authorization", "Bearer elnat_x")
	rec := httptest.NewRecorder()
	forward.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized || !strings.Contains(rec.Body.String(), `"error":"device_revoked"`) {
		t.Fatalf("revoked = %d %s", rec.Code, rec.Body.String())
	}
	// A plain rejection keeps the existing answer.
	plain := v2auth.NewEdgeAuthHandler(tokenValidatorFunc(func(context.Context, string) (identity.User, error) {
		return identity.User{}, identity.ErrCredentialRejected
	}))
	rec = httptest.NewRecorder()
	plain.ServeHTTP(rec, req)
	requireAccessDenied(t, rec)
}
