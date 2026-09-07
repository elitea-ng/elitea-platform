package middleware_test

// The 401 that says "missing authorization header" is written on FOUR
// different findings and its body says the same thing about all of them.
//
// #538 is what that costs. A browser holding a valid `elitea_session` was
// refused on one route in one webkit run of three, the retry passed, and
// after the fact nobody could say which finding it was: the branch wrote no
// log line, so "the browser sent no cookie" and "this service refused the
// cookie the browser sent" were indistinguishable in the only evidence that
// survived the run.
//
// These tests pin the line that separates them. They assert the LOG, not the
// response: the status and the body are unchanged on purpose, because a
// client that is not authenticated must not be told which check refused it.

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	apimw "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/middleware"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth"
)

const refusalLogSecret = "credential-refusal-log-secret"

// refusalCookie mints the cookie shape verifySessionCookie accepts, signed
// with `secret` so a test can sign with the WRONG one on purpose.
func refusalCookie(t *testing.T, secret string, claims map[string]any) string {
	t.Helper()
	payload, err := json.Marshal(claims)
	if err != nil {
		t.Fatal(err)
	}
	encoded := base64.RawURLEncoding.EncodeToString(payload)
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(encoded))
	return encoded + "." + hex.EncodeToString(mac.Sum(nil))
}

// refusalConfig is the shape every browser-facing route runs under on an
// OIDC-only deployment: a session secret and a principal validator that
// accepts whoever the credential named. Nothing here refuses a principal, so
// every 401 below comes from the credential branch under test.
func refusalConfig() apimw.AuthConfig {
	return apimw.AuthConfig{
		SessionSecret: refusalLogSecret,
		PrincipalValidator: principalValidatorFunc(
			func(_ context.Context, user auth.User) (auth.User, error) { return user, nil },
		),
	}
}

// refuse runs one request through Auth and returns the status, whether the
// protected handler was entered, and what was logged.
func refuse(t *testing.T, decorate func(*http.Request)) (int, bool, string) {
	t.Helper()
	recorded := captureLog(t)
	reached := false
	handler := apimw.Auth(refusalConfig())(
		http.HandlerFunc(func(http.ResponseWriter, *http.Request) { reached = true }))

	request := httptest.NewRequest(http.MethodGet, "/api/v2/admin/users/default/1", nil)
	if decorate != nil {
		decorate(request)
	}
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	return recorder.Code, reached, recorded.String()
}

// mustRefuseWithReason is the whole assertion: the answer is the unchanged
// 401, the handler never ran, and exactly one reason is named.
func mustRefuseWithReason(t *testing.T, logged string, status int, reached bool, reason string) {
	t.Helper()
	if status != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", status)
	}
	if reached {
		t.Fatal("the protected handler ran on a refused request")
	}
	if !strings.Contains(logged, `"reason":"`+reason+`"`) {
		t.Fatalf("log does not name %q:\n%s", reason, logged)
	}
	if !strings.Contains(logged, `"source":"session_cookie"`) {
		t.Fatalf("log does not name the credential source:\n%s", logged)
	}
	if !strings.Contains(logged, `"path":"/api/v2/admin/users/default/1"`) {
		t.Fatalf("log does not name the route:\n%s", logged)
	}
}

// The first half of #538's open question: the browser sent nothing of ours.
//
// `cookie_count` is on the line for the case this test cannot produce and a
// real run can — a browser that sent OTHER cookies and not this one, which is
// a cookie-jar problem rather than a signed-out client.
func TestCredentialRefusalNamesAnAbsentCookie(t *testing.T) {
	status, reached, logged := refuse(t, nil)
	mustRefuseWithReason(t, logged, status, reached, "no_credential")
	if !strings.Contains(logged, `"cookie_count":0`) {
		t.Fatalf("log does not count the request's cookies:\n%s", logged)
	}
}

func TestCredentialRefusalCountsCookiesThatAreNotOurs(t *testing.T) {
	status, reached, logged := refuse(t, func(r *http.Request) {
		r.AddCookie(&http.Cookie{Name: "unrelated", Value: "1"})
		r.AddCookie(&http.Cookie{Name: "also_unrelated", Value: "2"})
	})
	mustRefuseWithReason(t, logged, status, reached, "no_credential")
	if !strings.Contains(logged, `"cookie_count":2`) {
		t.Fatalf("log does not count the request's cookies:\n%s", logged)
	}
}

// The second half: the cookie arrived and THIS service refused it. Each
// reason below is a different operator problem, which is why one reason for
// all four would not have answered #538 either.
func TestCredentialRefusalNamesAMalformedCookie(t *testing.T) {
	status, reached, logged := refuse(t, func(r *http.Request) {
		r.AddCookie(&http.Cookie{Name: "elitea_session", Value: "not-a-signed-token"})
	})
	mustRefuseWithReason(t, logged, status, reached, "session_cookie_malformed")
}

func TestCredentialRefusalNamesAForeignSignature(t *testing.T) {
	cookie := refusalCookie(t, "some-other-deployments-secret", map[string]any{
		"uid": "6", "email": "e2e-member@autotest.local",
		"exp": time.Now().Add(time.Hour).Unix(),
	})
	status, reached, logged := refuse(t, func(r *http.Request) {
		r.AddCookie(&http.Cookie{Name: "elitea_session", Value: cookie})
	})
	mustRefuseWithReason(t, logged, status, reached, "session_cookie_signature_mismatch")
}

func TestCredentialRefusalNamesAnExpiredCookie(t *testing.T) {
	cookie := refusalCookie(t, refusalLogSecret, map[string]any{
		"uid": "6", "email": "e2e-member@autotest.local",
		"exp": time.Now().Add(-time.Minute).Unix(),
	})
	status, reached, logged := refuse(t, func(r *http.Request) {
		r.AddCookie(&http.Cookie{Name: "elitea_session", Value: cookie})
	})
	mustRefuseWithReason(t, logged, status, reached, "session_cookie_expired")
}

func TestCredentialRefusalNamesAnInvalidSubject(t *testing.T) {
	cookie := refusalCookie(t, refusalLogSecret, map[string]any{
		"uid": "0", "email": "e2e-member@autotest.local",
		"exp": time.Now().Add(time.Hour).Unix(),
	})
	status, reached, logged := refuse(t, func(r *http.Request) {
		r.AddCookie(&http.Cookie{Name: "elitea_session", Value: cookie})
	})
	mustRefuseWithReason(t, logged, status, reached, "session_cookie_subject_invalid")
}

// The composition defect PR #819 removed, kept as a named reason so its
// return is legible in one grep rather than in a diff of main.go.
func TestCredentialRefusalNamesAnAbsentSessionSecret(t *testing.T) {
	recorded := captureLog(t)
	reached := false
	handler := apimw.Auth(apimw.AuthConfig{})(
		http.HandlerFunc(func(http.ResponseWriter, *http.Request) { reached = true }))
	cookie := refusalCookie(t, refusalLogSecret, map[string]any{
		"uid": "6", "exp": time.Now().Add(time.Hour).Unix(),
	})
	request := httptest.NewRequest(http.MethodGet, "/api/v2/admin/users/default/1", nil)
	request.AddCookie(&http.Cookie{Name: "elitea_session", Value: cookie})
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)

	mustRefuseWithReason(t, recorded.String(), recorder.Code, reached, "session_secret_not_configured")
}

// The line must never carry the credential. A cookie value in an operator's
// log is a session anyone who reads the log can replay.
func TestCredentialRefusalLogsNoCredentialMaterial(t *testing.T) {
	const value = "cookie-value-that-must-not-be-logged"
	_, _, logged := refuse(t, func(r *http.Request) {
		r.AddCookie(&http.Cookie{Name: "elitea_session", Value: value})
	})
	if strings.Contains(logged, value) {
		t.Fatalf("the refusal log carries the cookie value:\n%s", logged)
	}
	if strings.Contains(logged, "unrelated_name_probe") {
		t.Fatalf("the refusal log carries cookie names:\n%s", logged)
	}
}
