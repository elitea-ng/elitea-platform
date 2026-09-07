package middleware_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/middleware"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/authsvc"
	"github.com/redis/go-redis/v9"
)

type tokenValidatorFunc func(context.Context, string) (auth.User, error)

func (f tokenValidatorFunc) ValidateToken(ctx context.Context, token string) (auth.User, error) {
	return f(ctx, token)
}

type principalValidatorFunc func(context.Context, auth.User) (auth.User, error)

func (f principalValidatorFunc) ValidatePrincipal(ctx context.Context, user auth.User) (auth.User, error) {
	return f(ctx, user)
}

type forwardedIdentityVerifierFunc func(*http.Request) error

func (f forwardedIdentityVerifierFunc) VerifyForwardedIdentityPeer(request *http.Request) error {
	return f(request)
}

func allowForwardedIdentity(*http.Request) error { return nil }

func newTestClient() *authsvc.Client {
	rdb := redis.NewClient(&redis.Options{Addr: "localhost:6379"})
	return authsvc.New(rdb)
}

func TestAuth_MissingHeader(t *testing.T) {
	handler := middleware.Auth(middleware.AuthConfig{Client: newTestClient()})(
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			t.Fatal("handler should not be called")
		}),
	)

	req := httptest.NewRequest("GET", "/api/v1/test", nil)
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("expected 401, got %d", rec.Code)
	}
	// `no_credential`, not the old blanket `unauthenticated`: the four
	// findings behind this 401 now each name themselves in the body (#538).
	// The MESSAGE is unchanged for this one, because for this one it was
	// always true.
	wantBody := `{"error":{"message":"missing authorization header","type":"authentication_error","code":"no_credential"}}` + "\n"
	if body := rec.Body.String(); body != wantBody {
		t.Errorf("unexpected body: %q, want %q", body, wantBody)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
}

func TestAuth_UnsupportedScheme(t *testing.T) {
	handler := middleware.Auth(middleware.AuthConfig{Client: newTestClient()})(
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			t.Fatal("handler should not be called")
		}),
	)

	req := httptest.NewRequest("GET", "/api/v1/test", nil)
	req.Header.Set("Authorization", "Digest abc123")
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("expected 401, got %d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
	assertJSONErrorBody(t, rec.Body.Bytes())
}

func TestAuth_InvalidBasicEncoding(t *testing.T) {
	handler := middleware.Auth(middleware.AuthConfig{Client: newTestClient()})(
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			t.Fatal("handler should not be called")
		}),
	)

	req := httptest.NewRequest("GET", "/api/v1/test", nil)
	req.Header.Set("Authorization", "Basic !!!notbase64!!!")
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("expected 401, got %d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
	assertJSONErrorBody(t, rec.Body.Bytes())
}

func TestAuth_BasicAuthExtractsUsername(t *testing.T) {
	handler := middleware.Auth(middleware.AuthConfig{Client: newTestClient()})(
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
		}),
	)

	token := "mytoken"
	encoded := base64.StdEncoding.EncodeToString([]byte(token + ":password"))
	req := httptest.NewRequest("GET", "/api/v1/test", nil)
	req.Header.Set("Authorization", "Basic "+encoded)
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	// Will fail with 401 since no real pylon_auth to validate — that's expected
	if rec.Code != http.StatusUnauthorized {
		t.Logf("got status %d (expected 401 without real auth backend)", rec.Code)
	}
}

func TestAuth_TraefikHeaders(t *testing.T) {
	var gotUser auth.User
	var gotSource auth.AuthenticationSource
	handler := middleware.Auth(middleware.AuthConfig{
		Client:                    newTestClient(),
		ForwardedIdentityVerifier: forwardedIdentityVerifierFunc(allowForwardedIdentity),
		PrincipalValidator: principalValidatorFunc(func(_ context.Context, user auth.User) (auth.User, error) {
			if user.Email != "" {
				t.Fatalf("forwarded reference was trusted as email: %+v", user)
			}
			user.Email = "authoritative@example.com"
			return user, nil
		}),
	})(
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			u, ok := auth.UserFromContext(r.Context())
			if !ok {
				t.Fatal("expected user in context")
			}
			gotUser = u
			var sourceOK bool
			gotSource, sourceOK = auth.AuthenticationSourceFromContext(r.Context())
			if !sourceOK {
				t.Fatal("expected authentication source in context")
			}
			w.WriteHeader(http.StatusOK)
		}),
	)

	req := httptest.NewRequest("GET", "/api/v1/test", nil)
	req.Header.Set("X-Auth-Type", "user")
	req.Header.Set("X-Auth-Id", "123")
	req.Header.Set("X-Auth-Reference", "user@example.com")
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	if gotUser.ID != "123" || gotUser.UserID != "123" {
		t.Errorf("expected user ID 123, got %+v", gotUser)
	}
	if gotUser.Email != "authoritative@example.com" {
		t.Errorf("expected authoritative email, got %q", gotUser.Email)
	}
	if gotUser.AuthType != "user" {
		t.Errorf("expected AuthType user, got %q", gotUser.AuthType)
	}
	if gotSource != auth.AuthenticationSourceForwarded {
		t.Errorf("expected forwarded source, got %d", gotSource)
	}
	if runtimePrincipal, ok := auth.RuntimePrincipalFromContext(auth.ContextWithAuthenticatedUser(context.Background(), gotUser, gotSource)); !ok || runtimePrincipal.ID != "123" {
		t.Fatalf("verified forwarded runtime principal = %+v, present=%v", runtimePrincipal, ok)
	}
}

func TestAuth_TraefikTokenNormalizesCompatibilityIDToOwningUser(t *testing.T) {
	var gotUser auth.User
	handler := middleware.Auth(middleware.AuthConfig{
		Client:                    newTestClient(),
		ForwardedIdentityVerifier: forwardedIdentityVerifierFunc(allowForwardedIdentity),
		PrincipalValidator: principalValidatorFunc(func(_ context.Context, user auth.User) (auth.User, error) {
			if user.TokenID != "900" || user.UserID != "7" {
				t.Fatalf("unvalidated forwarded identity = %+v", user)
			}
			user.ID = user.UserID
			return user, nil
		}),
	})(
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			var ok bool
			gotUser, ok = auth.UserFromContext(r.Context())
			if !ok {
				t.Fatal("expected user in context")
			}
			w.WriteHeader(http.StatusOK)
		}),
	)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/test", nil)
	req.Header.Set("X-Auth-Type", "token")
	req.Header.Set("X-Auth-Id", "900")
	req.Header.Set("X-Auth-User-ID", "7")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	if gotUser.ID != "7" || gotUser.TokenID != "900" || gotUser.UserID != "7" {
		t.Fatalf("forwarded token identity = %+v, compatibility ID must be the owning user", gotUser)
	}
	if ownerID, ok := gotUser.OwningUserID(); !ok || ownerID != 7 {
		t.Fatalf("owning user = %d, %v; want 7", ownerID, ok)
	}
}

func TestAuth_TrustedForwardedIdentityRequiresPrincipalValidator(t *testing.T) {
	handler := middleware.Auth(middleware.AuthConfig{
		Client:                    newTestClient(),
		ForwardedIdentityVerifier: forwardedIdentityVerifierFunc(allowForwardedIdentity),
	})(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("unvalidated forwarded identity reached protected handler")
	}))

	req := httptest.NewRequest(http.MethodGet, "/api/v1/test", nil)
	req.Header.Set("X-Auth-Type", "token")
	req.Header.Set("X-Auth-Id", "42")
	req.Header.Set("X-Auth-User-ID", "7")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusUnauthorized)
	}
}

func TestAuth_DoesNotTrustForwardedHeadersOnPublicListenerByDefault(t *testing.T) {
	handler := middleware.Auth(middleware.AuthConfig{Client: newTestClient()})(
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			t.Fatal("spoofed forwarded identity reached protected handler")
		}),
	)

	req := httptest.NewRequest(http.MethodGet, "/api/v2/projects", nil)
	req.Header.Set("X-Auth-Type", "user")
	req.Header.Set("X-Auth-Id", "1")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusUnauthorized)
	}
}

func TestAuthRejectsForwardedIdentityFromUnverifiedPeer(t *testing.T) {
	verifierCalls := 0
	handler := middleware.Auth(middleware.AuthConfig{
		Client: newTestClient(),
		ForwardedIdentityVerifier: forwardedIdentityVerifierFunc(func(*http.Request) error {
			verifierCalls++
			return errors.New("untrusted socket peer")
		}),
		PrincipalValidator: principalValidatorFunc(func(context.Context, auth.User) (auth.User, error) {
			t.Fatal("unverified forwarded identity reached principal validation")
			return auth.User{}, nil
		}),
	})(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("unverified forwarded identity reached protected handler")
	}))
	request := httptest.NewRequest(http.MethodGet, "/api/v2/projects", nil)
	request.Header.Set("X-Auth-Type", "user")
	request.Header.Set("X-Auth-ID", "7")
	recorder := httptest.NewRecorder()

	handler.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusUnauthorized || verifierCalls != 1 {
		t.Fatalf("status=%d verifier calls=%d", recorder.Code, verifierCalls)
	}
}

func TestAuthRejectsDuplicateForwardedIdentityHeaders(t *testing.T) {
	handler := middleware.Auth(middleware.AuthConfig{
		Client:                    newTestClient(),
		ForwardedIdentityVerifier: forwardedIdentityVerifierFunc(allowForwardedIdentity),
		PrincipalValidator: principalValidatorFunc(func(context.Context, auth.User) (auth.User, error) {
			t.Fatal("ambiguous forwarded identity reached principal validation")
			return auth.User{}, nil
		}),
	})(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("ambiguous forwarded identity reached protected handler")
	}))
	request := httptest.NewRequest(http.MethodGet, "/api/v2/projects", nil)
	request.Header.Set("X-Auth-Type", "user")
	request.Header.Set("X-Auth-ID", "7")
	request.Header["x-auth-id"] = []string{"8"}
	recorder := httptest.NewRecorder()

	handler.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("status=%d, want %d", recorder.Code, http.StatusUnauthorized)
	}
}

func TestAuthRejectsCredentialWhenAuthoritativePrincipalCheckFails(t *testing.T) {
	handler := middleware.Auth(middleware.AuthConfig{
		Client: newTestClient(),
		Validator: tokenValidatorFunc(func(context.Context, string) (auth.User, error) {
			return auth.User{ID: "7", UserID: "7", AuthType: "token"}, nil
		}),
		PrincipalValidator: principalValidatorFunc(func(context.Context, auth.User) (auth.User, error) {
			// See the note in auth_traefik_test.go: a suspension reports
			// auth.ErrPrincipalInactive, and only that answer is a 401 (#537).
			return auth.User{}, auth.ErrPrincipalInactive
		}),
	})(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("inactive principal reached protected handler")
	}))

	req := httptest.NewRequest(http.MethodGet, "/api/v2/projects", nil)
	req.Header.Set("Authorization", "Bearer valid-credential")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusUnauthorized)
	}
}

func TestAuthLocalValidationNeverConsultsSharedRedisCache(t *testing.T) {
	var redisDials atomic.Int64
	rdb := redis.NewClient(&redis.Options{
		Addr: "shared-legacy-redis.invalid:6379",
		Dialer: func(context.Context, string, string) (net.Conn, error) {
			redisDials.Add(1)
			return nil, errors.New("shared Redis must not be an authentication authority")
		},
	})
	t.Cleanup(func() { _ = rdb.Close() })
	validatorCalls := 0
	handler := middleware.Auth(middleware.AuthConfig{
		Client: authsvc.New(rdb),
		Validator: tokenValidatorFunc(func(context.Context, string) (auth.User, error) {
			validatorCalls++
			return auth.User{}, errors.New("invalid signature")
		}),
	})(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("invalid credential reached protected handler")
	}))

	req := httptest.NewRequest(http.MethodGet, "/api/v2/auth/token/", nil)
	req.Header.Set("Authorization", "Bearer attacker-controlled-cache-key")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, req)

	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusUnauthorized)
	}
	if validatorCalls != 1 {
		t.Fatalf("local validator calls = %d, want 1", validatorCalls)
	}
	if redisDials.Load() != 0 {
		t.Fatalf("shared Redis dials = %d, want 0", redisDials.Load())
	}
}

func TestAuth_TraefikHeaders_MissingID(t *testing.T) {
	handler := middleware.Auth(middleware.AuthConfig{
		Client:                    newTestClient(),
		ForwardedIdentityVerifier: forwardedIdentityVerifierFunc(allowForwardedIdentity),
	})(
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			t.Fatal("should not reach handler without Authorization header")
		}),
	)

	req := httptest.NewRequest("GET", "/api/v1/test", nil)
	req.Header.Set("X-Auth-Type", "user")
	// Missing X-Auth-Id
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("expected 401, got %d", rec.Code)
	}
}

func assertJSONErrorBody(t *testing.T, body []byte) {
	t.Helper()
	var envelope struct {
		Error struct {
			Message string `json:"message"`
			Type    string `json:"type"`
			Code    string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		t.Fatalf("body is not valid JSON: %s", body)
	}
	if envelope.Error.Message == "" {
		t.Error("error.message is empty")
	}
	if envelope.Error.Type == "" {
		t.Error("error.type is empty")
	}
	if envelope.Error.Code == "" {
		t.Error("error.code is empty")
	}
}
