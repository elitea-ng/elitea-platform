package auth_test

import (
	"context"
	"encoding/base64"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

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

type forwardedIdentityVerifierFunc func(*http.Request) error

func (f forwardedIdentityVerifierFunc) VerifyForwardedIdentityPeer(request *http.Request) error {
	return f(request)
}

func TestForwardAuthRequiresCurrentBaselineTraefikHeaders(t *testing.T) {
	for _, missing := range currentBaselineTraefikHeaders() {
		t.Run(missing.name, func(t *testing.T) {
			validated := false
			forward := v2auth.NewForwardAuthHandler(tokenValidatorFunc(func(context.Context, string) (identity.User, error) {
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

func TestForwardAuthTraefikHeadersRequirePresenceNotContent(t *testing.T) {
	forward := v2auth.NewForwardAuthHandler(tokenValidatorFunc(func(context.Context, string) (identity.User, error) {
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

func TestForwardAuthAuthorizationPrecedesAdditionalCredentialHeaders(t *testing.T) {
	var validatedToken string
	forward := v2auth.NewForwardAuthHandler(
		tokenValidatorFunc(func(_ context.Context, token string) (identity.User, error) {
			validatedToken = token
			return validatedTokenUser(), nil
		}),
		v2auth.WithForwardAuthCredentialHeaders(v2auth.ForwardAuthCredentialHeader{
			Name:           "X-API-Key",
			CredentialType: "bearer",
		}),
	)
	req := newForwardAuthRequest("/auth")
	req.Header.Set("Authorization", "bEaReR authorization-token")
	req.Header.Set("X-API-Key", "additional-header-token")
	rec := httptest.NewRecorder()

	forward.ServeHTTP(rec, req)

	requireOK(t, rec)
	if validatedToken != "authorization-token" {
		t.Fatalf("validated token = %q, want Authorization credential", validatedToken)
	}
}

func TestForwardAuthMalformedAuthorizationDoesNotTraverseToAdditionalHeader(t *testing.T) {
	validated := false
	forward := v2auth.NewForwardAuthHandler(
		tokenValidatorFunc(func(context.Context, string) (identity.User, error) {
			validated = true
			return validatedTokenUser(), nil
		}),
		v2auth.WithForwardAuthCredentialHeaders(v2auth.ForwardAuthCredentialHeader{
			Name:           "X-API-Key",
			CredentialType: "bearer",
		}),
	)
	req := newForwardAuthRequest("/auth")
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

func TestForwardAuthCredentialHandlers(t *testing.T) {
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
			forward := v2auth.NewForwardAuthHandler(tokenValidatorFunc(func(_ context.Context, token string) (identity.User, error) {
				gotToken = token
				return validatedTokenUser(), nil
			}))
			req := newForwardAuthRequest("/auth")
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

func TestForwardAuthAdditionalCredentialHeadersAreExplicitAndOrdered(t *testing.T) {
	t.Run("not trusted by default", func(t *testing.T) {
		validated := false
		forward := v2auth.NewForwardAuthHandler(tokenValidatorFunc(func(context.Context, string) (identity.User, error) {
			validated = true
			return validatedTokenUser(), nil
		}))
		req := newForwardAuthRequest("/auth")
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
		forward := v2auth.NewForwardAuthHandler(
			tokenValidatorFunc(func(_ context.Context, token string) (identity.User, error) {
				gotToken = token
				return validatedTokenUser(), nil
			}),
			v2auth.WithForwardAuthCredentialHeaders(
				v2auth.ForwardAuthCredentialHeader{Name: "X-First-Key", CredentialType: "bearer"},
				v2auth.ForwardAuthCredentialHeader{Name: "X-Second-Key", CredentialType: "bearer"},
			),
		)
		req := newForwardAuthRequest("/auth")
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

func TestForwardAuthSuccessTargetContract(t *testing.T) {
	tests := []struct {
		name       string
		path       string
		wantStatus int
		wantRPC    bool
	}{
		{name: "target omitted uses no-op mapper", path: "/auth", wantStatus: http.StatusOK},
		{name: "rpc target emits auth headers", path: "/auth?target=rpc", wantStatus: http.StatusOK, wantRPC: true},
		{name: "empty target is not registered", path: "/auth?target=", wantStatus: http.StatusForbidden},
		{name: "unknown target is not registered", path: "/auth?target=unknown", wantStatus: http.StatusForbidden},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			forward := v2auth.NewForwardAuthHandler(tokenValidatorFunc(func(context.Context, string) (identity.User, error) {
				return validatedTokenUser(), nil
			}))
			req := newForwardAuthRequest(test.path)
			req.Header.Set("Authorization", "Bearer signed-token")
			rec := httptest.NewRecorder()

			forward.ServeHTTP(rec, req)

			if test.wantStatus == http.StatusOK {
				requireOK(t, rec)
			} else {
				requireAccessDenied(t, rec)
			}
			if got := rec.Header().Get("X-Auth-Type"); test.wantRPC && got != "token" {
				t.Fatalf("X-Auth-Type = %q, want token", got)
			} else if !test.wantRPC && got != "" {
				t.Fatalf("X-Auth-Type = %q, want no auth headers", got)
			}
		})
	}
}

func TestForwardAuthPreservesTokenRowAndOwningUserAcrossHeaders(t *testing.T) {
	forward := v2auth.NewForwardAuthHandler(tokenValidatorFunc(func(_ context.Context, token string) (identity.User, error) {
		if token != "signed-token" {
			t.Fatalf("validated token = %q", token)
		}
		// Token row 42 and user row 42 both exist in this scenario, but token
		// row 42 deliberately belongs to user 7.
		return identity.User{
			ID:       "7",
			TokenID:  "42",
			UserID:   "7",
			Email:    "owner@example.test",
			AuthType: "token",
		}, nil
	}))
	forwardReq := newForwardAuthRequest("/auth?target=rpc")
	forwardReq.Header.Set("Authorization", "Bearer signed-token")
	forwardRec := httptest.NewRecorder()
	forward.ServeHTTP(forwardRec, forwardReq)
	requireOK(t, forwardRec)
	if got := forwardRec.Header().Get("X-Auth-ID"); got != "42" {
		t.Fatalf("X-Auth-ID = %q, want token row 42", got)
	}
	if got := forwardRec.Header().Get("X-Auth-User-ID"); got != "7" {
		t.Fatalf("X-Auth-User-ID = %q, want owner 7", got)
	}
	if got := forwardRec.Header().Get("X-Auth-Reference"); got != "-" {
		t.Fatalf("X-Auth-Reference = %q, want current-baseline token reference", got)
	}

	var downstream identity.User
	authMiddleware := middleware.Auth(middleware.AuthConfig{
		ForwardedIdentityVerifier: forwardedIdentityVerifierFunc(func(*http.Request) error { return nil }),
		PrincipalValidator: principalValidatorFunc(func(_ context.Context, user identity.User) (identity.User, error) {
			if user.ID != "42" || user.TokenID != "42" || user.UserID != "7" {
				t.Fatalf("principal validator received %+v", user)
			}
			return user, nil
		}),
	})(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var ok bool
		downstream, ok = identity.UserFromContext(r.Context())
		if !ok {
			t.Fatal("missing downstream identity")
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	downstreamReq := httptest.NewRequest(http.MethodGet, "/api/v2/configurations/7", nil)
	for _, header := range []string{"X-Auth-Type", "X-Auth-ID", "X-Auth-User-ID", "X-Auth-Reference"} {
		downstreamReq.Header.Set(header, forwardRec.Header().Get(header))
	}
	downstreamRec := httptest.NewRecorder()
	authMiddleware.ServeHTTP(downstreamRec, downstreamReq)
	if downstreamRec.Code != http.StatusNoContent {
		t.Fatalf("downstream status = %d, body=%s", downstreamRec.Code, downstreamRec.Body.String())
	}
	if downstream.TokenID != "42" || downstream.UserID != "7" {
		t.Fatalf("downstream identity = %+v", downstream)
	}
}

func TestForwardAuthFailsClosedWhenValidatorOmitsTypedTokenIdentity(t *testing.T) {
	for _, user := range []identity.User{
		{ID: "7", UserID: "7", AuthType: "token"},
		{ID: "42", TokenID: "42", AuthType: "token"},
	} {
		forward := v2auth.NewForwardAuthHandler(tokenValidatorFunc(func(context.Context, string) (identity.User, error) {
			return user, nil
		}))
		req := newForwardAuthRequest("/auth?target=rpc")
		req.Header.Set("Authorization", "Bearer signed-token")
		rec := httptest.NewRecorder()

		forward.ServeHTTP(rec, req)

		requireAccessDenied(t, rec)
	}
}

func TestForwardAuthFailsClosedWithoutAWorkingValidator(t *testing.T) {
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
		forward := v2auth.NewForwardAuthHandler(test.validator)
		req := newForwardAuthRequest("/auth?target=rpc")
		req.Header.Set("Authorization", "Bearer signed-token")
		rec := httptest.NewRecorder()

		forward.ServeHTTP(rec, req)

		requireAccessDenied(t, rec)
		if rec.Body.String() == "test-only validator detail" {
			t.Fatal("validator detail was exposed to the client")
		}
	}
}

func newForwardAuthRequest(path string) *http.Request {
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
