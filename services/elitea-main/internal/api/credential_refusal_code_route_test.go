package api

// EVERY 401 THIS ROUTER WRITES NAMES THE CHECK THAT REFUSED THE REQUEST.
//
// #538 is the measurement this file answers. An E2E journey holding a valid
// session got `401 {"code":"unauthenticated","message":"missing authorization
// header"}` once in three runs, the retry passed, and after the fact nobody
// could say which of four findings it was: the browser sent no cookie, it sent
// one signed by another secret, the cookie had expired, or a server-side
// session had been revoked. All four wrote the SAME body.
//
// #537 gave the operator a log line. That log lives on the server, and the
// only evidence a CI run keeps is the response. So the code now names the
// finding, and these tests pin one code per finding through the real router,
// the real middleware and the real session manager.
//
// THE STATUS DOES NOT MOVE. Every case here is 401 `authentication_error`.
// A client is told WHICH of its own credentials failed, never anything about
// the deployment: `session_secret_not_configured` deliberately keeps the
// generic body, and its own case below proves it.

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

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth/browsersession"
)

const refusalRouteSecret = "credential-refusal-route-secret"

// refusalRouter is the browser-facing composition: a session secret for the
// legacy cookie and a session store for the server-side one, so every cookie
// branch of middleware.Auth is reachable through it.
func refusalRouter(t *testing.T) (http.Handler, *browsersession.Manager) {
	t.Helper()
	store := &routeTestSessions{rows: map[string]browsersession.Session{}}
	manager, err := browsersession.NewManager(store, browsersession.Policy{
		IdleTimeout: time.Hour, AbsoluteLifetime: 24 * time.Hour,
	})
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	router := NewRouter(RouterConfig{
		SessionSecret:      refusalRouteSecret,
		PrincipalValidator: activeRoutePrincipals{},
		Auth:               AuthDeps{SessionStore: manager},
	})
	return router, manager
}

// legacySessionCookie mints the HMAC cookie verifySessionCookie accepts,
// signed with `secret` so a test can sign with the WRONG one on purpose.
func legacySessionCookie(t *testing.T, secret string, claims map[string]any) string {
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

// refusalAnswer runs one request through the router and returns the status and
// the parsed error envelope.
func refusalAnswer(t *testing.T, router http.Handler, decorate func(*http.Request)) (int, string, string) {
	t.Helper()
	request := httptest.NewRequest(http.MethodGet, theAPIRoute, nil)
	if decorate != nil {
		decorate(request)
	}
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)

	var body struct {
		Error struct {
			Code    string `json:"code"`
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode refusal: %v (%s)", err, recorder.Body.String())
	}
	if recorder.Code == http.StatusUnauthorized && body.Error.Type != "authentication_error" {
		t.Fatalf("refusal type = %q, want authentication_error", body.Error.Type)
	}
	return recorder.Code, body.Error.Code, body.Error.Message
}

func withLegacyCookie(value string) func(*http.Request) {
	return func(r *http.Request) {
		r.AddCookie(&http.Cookie{Name: "elitea_session", Value: value})
	}
}

func withServerCookie(value string) func(*http.Request) {
	return func(r *http.Request) {
		r.AddCookie(&http.Cookie{Name: browsersession.CookieName, Value: value})
	}
}

// mustRefuse is the shared assertion: 401, and exactly this code and message.
func mustRefuse(t *testing.T, status int, code, message, wantCode, wantMessage string) {
	t.Helper()
	if status != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", status)
	}
	if code != wantCode || message != wantMessage {
		t.Fatalf("refusal = %q / %q, want %q / %q — this is the string a CI run keeps "+
			"after the stack is gone (#538)", code, message, wantCode, wantMessage)
	}
}

// 1. The browser sent nothing of ours. The message is the historical one,
// because for THIS finding it was always true.
func TestTheRouterNamesAnAbsentCredential(t *testing.T) {
	router, _ := refusalRouter(t)
	status, code, message := refusalAnswer(t, router, nil)
	mustRefuse(t, status, code, message, "no_credential", "missing authorization header")
}

// 2. A cookie signed by another deployment's secret. This is the finding a
// session-secret rotation on a stack restart produces, and it is one of the
// causes #538 could not exclude.
func TestTheRouterNamesAForeignCookieSignature(t *testing.T) {
	router, _ := refusalRouter(t)
	cookie := legacySessionCookie(t, "another-deployments-secret", map[string]any{
		"uid": "11", "email": "member@autotest.local",
		"exp": time.Now().Add(time.Hour).Unix(),
	})
	status, code, message := refusalAnswer(t, router, withLegacyCookie(cookie))
	mustRefuse(t, status, code, message,
		"session_cookie_signature_mismatch", "the session cookie signature does not match")
}

// 3. A cookie this deployment signed, whose `exp` has passed. The finding a
// long suite produces against a short-lived cookie.
func TestTheRouterNamesAnExpiredCookie(t *testing.T) {
	router, _ := refusalRouter(t)
	cookie := legacySessionCookie(t, refusalRouteSecret, map[string]any{
		"uid": "11", "email": "member@autotest.local",
		"exp": time.Now().Add(-time.Minute).Unix(),
	})
	status, code, message := refusalAnswer(t, router, withLegacyCookie(cookie))
	mustRefuse(t, status, code, message,
		"session_cookie_expired", "the session cookie expired")
}

func TestTheRouterNamesAMalformedCookie(t *testing.T) {
	router, _ := refusalRouter(t)
	status, code, message := refusalAnswer(t, router, withLegacyCookie("not-a-signed-token"))
	mustRefuse(t, status, code, message,
		"session_cookie_malformed", "the session cookie is malformed")
}

// 4. A server-side session that a LOGOUT ended. This is the finding of #832:
// one shared session per persona, and the logout journey ends it for every
// worker replaying the same cookie. It used to be indistinguishable from case
// 1, which is why the shared-session cause took a separate investigation.
func TestTheRouterNamesARevokedSession(t *testing.T) {
	router, manager := refusalRouter(t)
	value, err := manager.Create(context.Background(), browsersession.NewSession{
		UserID: 11, Email: "member@autotest.local", Provider: browsersession.ProviderOIDC,
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if status := serveWithSessionCookie(t, router, theAPIRoute, value); status == http.StatusUnauthorized {
		t.Fatal("the router refused a live session before it was revoked")
	}
	if err := manager.Revoke(context.Background(), value); err != nil {
		t.Fatalf("Revoke: %v", err)
	}
	status, code, message := refusalAnswer(t, router, withServerCookie(value))
	mustRefuse(t, status, code, message, "session_revoked", "the session was revoked")
}

// 5. A server-side identifier no row answers: a stack that was re-seeded, or a
// storage state kept from an earlier stack.
func TestTheRouterNamesAnUnknownSession(t *testing.T) {
	router, manager := refusalRouter(t)
	value, err := manager.Create(context.Background(), browsersession.NewSession{
		UserID: 11, Email: "member@autotest.local", Provider: browsersession.ProviderOIDC,
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	// Same shape, one character different, so LooksServerSide still routes it
	// to the store and the store has no row for it.
	unknown := value[:len(value)-1] + string(rune('a'+(value[len(value)-1]+1-'a')%26))
	status, code, message := refusalAnswer(t, router, withServerCookie(unknown))
	mustRefuse(t, status, code, message,
		"session_unknown", "the session is not known to this deployment")
}

// 6. A bearer token no validator accepts. Not a cookie finding, and the code
// says so.
func TestTheRouterNamesARejectedToken(t *testing.T) {
	router, _ := refusalRouter(t)
	status, code, message := refusalAnswer(t, router, func(r *http.Request) {
		r.Header.Set("Authorization", "Bearer not-a-token")
	})
	mustRefuse(t, status, code, message, "token_rejected", "token validation failed")
}

func TestTheRouterNamesAnUnsupportedScheme(t *testing.T) {
	router, _ := refusalRouter(t)
	status, code, message := refusalAnswer(t, router, func(r *http.Request) {
		r.Header.Set("Authorization", "Digest nonsense")
	})
	mustRefuse(t, status, code, message,
		"authorization_scheme_unsupported", "unsupported authorization scheme")
}

// 7. THE ONE THAT MUST STAY GENERIC. A deployment with no session secret is a
// fact about the SERVER, and an unauthenticated caller is told nothing about
// it. The operator reads `session_secret_not_configured` in the log instead.
func TestTheRouterKeepsAServerConfigurationFindingOutOfTheBody(t *testing.T) {
	router := NewRouter(RouterConfig{PrincipalValidator: activeRoutePrincipals{}})
	cookie := legacySessionCookie(t, refusalRouteSecret, map[string]any{
		"uid": "11", "exp": time.Now().Add(time.Hour).Unix(),
	})
	status, code, message := refusalAnswer(t, router, withLegacyCookie(cookie))
	mustRefuse(t, status, code, message, "unauthenticated", "missing authorization header")
}
