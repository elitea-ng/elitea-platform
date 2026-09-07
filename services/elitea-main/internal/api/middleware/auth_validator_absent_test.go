package middleware_test

// A deployment that composed no PrincipalValidator must authenticate nobody,
// on EVERY credential path.
//
// validatePrincipal used to return `(user, nil)` when the validator was nil.
// That was safe only by accident: the forwarded path guarded nil a few lines
// before calling it, so the open default was unreachable *there*. The three
// paths below had no such guard.
//
// The session path is the one that mattered. verifySessionCookie is an HMAC
// check with no database read, so on a deployment with a session secret and no
// validator, an unexpired cookie authenticated a user this service never
// looked up — including one since suspended or deleted.
//
// Each test asserts BOTH halves: the status is 401, and the protected handler
// was never entered. Status alone would pass against a handler that ran and
// then had its response overwritten.

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	apimw "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/middleware"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth"
)

const absentValidatorSessionSecret = "session-secret-for-this-test"

// sessionCookieFor mints the cookie shape verifySessionCookie accepts:
// base64url(payload) + "." + hex(HMAC-SHA256(payload)).
func sessionCookieFor(t *testing.T, claims map[string]any) string {
	t.Helper()
	payload, err := json.Marshal(claims)
	if err != nil {
		t.Fatal(err)
	}
	encoded := base64.RawURLEncoding.EncodeToString(payload)
	mac := hmac.New(sha256.New, []byte(absentValidatorSessionSecret))
	mac.Write([]byte(encoded))
	return encoded + "." + hex.EncodeToString(mac.Sum(nil))
}

// acceptEveryToken stands in for a deployment whose TOKEN validator works —
// which is the whole point. The credential is genuinely valid; what is missing
// is the reload that would notice the principal is no longer allowed in.
type acceptEveryToken struct{ user auth.User }

func (a acceptEveryToken) ValidateToken(context.Context, string) (auth.User, error) {
	return a.user, nil
}

func absentValidatorConfig() apimw.AuthConfig {
	return apimw.AuthConfig{
		Validator:     acceptEveryToken{user: auth.User{ID: "7", UserID: "7", AuthType: "user"}},
		SessionSecret: absentValidatorSessionSecret,
		// PrincipalValidator is deliberately absent. That is the deployment
		// shape under test.
		ForwardedIdentityVerifier: forwardedIdentityVerifierFunc(allowForwardedIdentity),
	}
}

// refusedWithoutReachingHandler runs one request and reports whether the
// protected handler was entered.
func refusedWithoutReachingHandler(t *testing.T, decorate func(*http.Request)) (int, bool) {
	t.Helper()
	reached := false
	handler := apimw.Auth(absentValidatorConfig())(
		http.HandlerFunc(func(http.ResponseWriter, *http.Request) { reached = true }))

	request := httptest.NewRequest(http.MethodGet, "/api/v2/anything", nil)
	decorate(request)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response.Code, reached
}

func TestAnAbsentPrincipalValidatorRefusesASessionCookie(t *testing.T) {
	// The sharpest case: an HMAC-valid cookie with no database read behind it.
	cookie := sessionCookieFor(t, map[string]any{
		"uid": 7,
		"exp": float64(time.Now().Add(time.Hour).Unix()),
	})

	status, reached := refusedWithoutReachingHandler(t, func(r *http.Request) {
		r.AddCookie(&http.Cookie{Name: "elitea_session", Value: cookie})
	})

	if reached {
		t.Fatal("a session cookie authenticated a principal this deployment never validated")
	}
	if status != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", status)
	}
}

func TestAnAbsentPrincipalValidatorRefusesAnAPIKey(t *testing.T) {
	status, reached := refusedWithoutReachingHandler(t, func(r *http.Request) {
		r.Header.Set("X-API-Key", "a-valid-key")
	})

	if reached {
		t.Fatal("an X-API-Key authenticated a principal this deployment never validated")
	}
	if status != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", status)
	}
}

func TestAnAbsentPrincipalValidatorRefusesABearerToken(t *testing.T) {
	status, reached := refusedWithoutReachingHandler(t, func(r *http.Request) {
		r.Header.Set("Authorization", "Bearer a-valid-token")
	})

	if reached {
		t.Fatal("a bearer token authenticated a principal this deployment never validated")
	}
	if status != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", status)
	}
}

func TestAnAbsentPrincipalValidatorRefusesAForwardedIdentity(t *testing.T) {
	// This path already refused before the change, via its own inline nil
	// check. The check is gone and the shared helper covers it now, so this
	// asserts the behaviour did not move when the duplicate was removed.
	status, reached := refusedWithoutReachingHandler(t, func(r *http.Request) {
		r.Header.Set("X-Auth-Type", "token")
		r.Header.Set("X-Auth-Id", "42")
		r.Header.Set("X-Auth-User-ID", "7")
	})

	if reached {
		t.Fatal("a forwarded identity authenticated a principal this deployment never validated")
	}
	if status != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", status)
	}
}

// The counterpart: with a validator composed, each path still authenticates.
// Without this, "refuse everything" would pass all four tests above.
func TestEveryPathStillAuthenticatesWhenTheValidatorIsPresent(t *testing.T) {
	cookie := sessionCookieFor(t, map[string]any{
		"uid": 7,
		"exp": float64(time.Now().Add(time.Hour).Unix()),
	})

	cases := map[string]func(*http.Request){
		"session cookie": func(r *http.Request) {
			r.AddCookie(&http.Cookie{Name: "elitea_session", Value: cookie})
		},
		"api key": func(r *http.Request) { r.Header.Set("X-API-Key", "a-valid-key") },
		"bearer":  func(r *http.Request) { r.Header.Set("Authorization", "Bearer a-valid-token") },
		"forwarded identity": func(r *http.Request) {
			r.Header.Set("X-Auth-Type", "token")
			r.Header.Set("X-Auth-Id", "42")
			r.Header.Set("X-Auth-User-ID", "7")
		},
	}

	for name, decorate := range cases {
		cfg := absentValidatorConfig()
		cfg.PrincipalValidator = principalValidatorFunc(
			func(_ context.Context, user auth.User) (auth.User, error) { return user, nil })

		reached := false
		handler := apimw.Auth(cfg)(
			http.HandlerFunc(func(http.ResponseWriter, *http.Request) { reached = true }))

		request := httptest.NewRequest(http.MethodGet, "/api/v2/anything", nil)
		decorate(request)
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)

		if !reached {
			t.Fatalf("%s: a valid credential was refused with a validator present (status %d)",
				name, response.Code)
		}
	}
}
