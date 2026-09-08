package middleware_test

// Regression tests for #390. The trusted-proxy header path is deleted.
// X-Auth-Type / X-Auth-Id are never a credential on their own, whatever the
// source address is, and whatever TRUSTED_PROXY_CIDRS holds. Only the
// ForwardedIdentityPeerVerifier path accepts a forwarded identity, and that
// path always applies the authoritative principal check.
//
// These tests replace three earlier tests that asserted the opposite contract:
// that the same two headers authenticate a caller whose RemoteAddr falls inside
// a configured CIDR. That contract admitted an anonymous caller as any user.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/middleware"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth"
)

// TestAuth_ForwardedHeadersAreNotACredential proves that the two headers get
// 401 from every source address. httptest.NewRequest sets RemoteAddr to
// 192.0.2.1:1234, so the first case names the range that used to trust it.
// Before #390 that case served the request as database user 1.
func TestAuth_ForwardedHeadersAreNotACredential(t *testing.T) {
	for _, testCase := range []struct {
		name       string
		remoteAddr string
	}{
		{name: "formerly trusted range", remoteAddr: "192.0.2.1:1234"},
		{name: "loopback", remoteAddr: "127.0.0.1:5678"},
		{name: "private range", remoteAddr: "10.1.2.3:9999"},
		{name: "public address", remoteAddr: "203.0.113.9:443"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			// The variable named the whole production switch before #390.
			t.Setenv("TRUSTED_PROXY_CIDRS", "0.0.0.0/0,192.0.2.0/24,10.0.0.0/8")

			validatorCalls := 0
			handler := middleware.Auth(middleware.AuthConfig{
				Validator: unusedTokenValidator(),
				PrincipalValidator: principalValidatorFunc(func(context.Context, auth.User) (auth.User, error) {
					validatorCalls++
					return auth.User{}, nil
				}),
			})(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
				t.Fatal("forwarded headers alone reached protected handler")
			}))

			req := httptest.NewRequest(http.MethodGet, "/api/v2/projects", nil)
			req.RemoteAddr = testCase.remoteAddr
			req.Header.Set("X-Auth-Type", "user")
			req.Header.Set("X-Auth-Id", "1")
			req.Header.Set("X-Auth-Reference", "attacker@example.com")
			rec := httptest.NewRecorder()

			handler.ServeHTTP(rec, req)

			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("status = %d, want %d", rec.Code, http.StatusUnauthorized)
			}
			if validatorCalls != 0 {
				t.Fatalf("principal validator calls = %d, want 0; headers must not reach principal validation", validatorCalls)
			}
		})
	}
}

// TestAuth_VerifiedForwardedIdentityStillAuthenticates is the control. A change
// that refuses every caller would pass the test above on its own. The supported
// forwarded-identity path must still admit a verified peer, and must still
// resolve the principal through the validator.
func TestAuth_VerifiedForwardedIdentityStillAuthenticates(t *testing.T) {
	t.Setenv("TRUSTED_PROXY_CIDRS", "192.0.2.0/24")

	var gotUser auth.User
	var gotSource auth.AuthenticationSource
	handler := middleware.Auth(middleware.AuthConfig{
		Validator:                 unusedTokenValidator(),
		ForwardedIdentityVerifier: forwardedIdentityVerifierFunc(allowForwardedIdentity),
		PrincipalValidator: principalValidatorFunc(func(_ context.Context, user auth.User) (auth.User, error) {
			user.Email = "authoritative@example.com"
			return user, nil
		}),
	})(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, ok := auth.UserFromContext(r.Context())
		if !ok {
			t.Fatal("no user in context")
		}
		gotUser = user
		gotSource, _ = auth.AuthenticationSourceFromContext(r.Context())
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodGet, "/api/v2/projects", nil)
	req.Header.Set("X-Auth-Type", "user")
	req.Header.Set("X-Auth-ID", "7")
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	if gotUser.UserID != "7" {
		t.Errorf("user ID = %q, want 7", gotUser.UserID)
	}
	if gotUser.Email != "authoritative@example.com" {
		t.Errorf("email = %q, want the validator's value", gotUser.Email)
	}
	if gotSource != auth.AuthenticationSourceForwarded {
		t.Errorf("source = %v, want %v", gotSource, auth.AuthenticationSourceForwarded)
	}
}

// TestAuth_ForwardedHeadersRefusedWhenPrincipalIsRefused proves the second half
// of #390: a refused principal never reaches the handler on any forwarded path.
// This test failed before the change, because the trusted-proxy path called
// serveAuthenticated directly and never consulted the validator.
func TestAuth_ForwardedHeadersRefusedWhenPrincipalIsRefused(t *testing.T) {
	t.Setenv("TRUSTED_PROXY_CIDRS", "192.0.2.0/24")

	handler := middleware.Auth(middleware.AuthConfig{
		Validator:                 unusedTokenValidator(),
		ForwardedIdentityVerifier: forwardedIdentityVerifierFunc(allowForwardedIdentity),
		PrincipalValidator: principalValidatorFunc(func(context.Context, auth.User) (auth.User, error) {
			// The sentinel, and not a bare error: a deactivated principal is
			// what auth.ErrPrincipalInactive means, and the middleware now
			// reads that difference to keep a database fault out of the 401
			// (#537). A bare error here would assert the collapsed behaviour.
			return auth.User{}, auth.ErrPrincipalInactive
		}),
	})(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("deactivated principal reached protected handler")
	}))

	req := httptest.NewRequest(http.MethodGet, "/api/v2/projects", nil)
	req.Header.Set("X-Auth-Type", "user")
	req.Header.Set("X-Auth-ID", "1")
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusUnauthorized)
	}
}
