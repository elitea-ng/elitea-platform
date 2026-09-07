package api

// A LOGOUT ENDS THE SESSION FOR EVERY HOLDER OF THE COOKIE — through the real
// router, the real logout handler and the real /api/v2 authentication.
//
// WHY THIS EXISTS. `browser_session_route_test.go` proves the router admits a
// live session and refuses one that `manager.Revoke` ended. It revokes by
// calling the manager directly, so it says nothing about what a BROWSER's
// logout does, and nothing about the answer the next request then gets.
//
// Both of those were the whole failure. The E2E suite's `auth.setup.ts` mints
// one server-side session per persona and every worker replays its cookie, so
// the first journey that signed out signed out the entire suite. What CI then
// reported was `401 {"code":"unauthenticated","message":"missing authorization
// header"}` on routes that were correctly wired, and a shell that navigated
// itself to the identity provider mid-journey — two symptoms that read as a
// composition gap and are not one.
//
// THAT BODY NO LONGER HIDES THE CAUSE (#538). A revoked session answers
// `session_revoked`, and a request with no cookie answers `no_credential`.
// The investigation above cost a separate measurement because the two strings
// were one string. `credential_refusal_code_route_test.go` holds one case per
// finding.
//
// So this file pins the two answers by their exact shape, next to the
// sequence that produces them:
//
//   1. `/api/v2` refuses a revoked session with `session_revoked`, which no
//      other finding writes.
//   2. `/forward-auth/info` refuses it with `session_expired`, which is the
//      one code `apps/elitea-web/src/app/session-probe-client.ts` navigates
//      the browser on.
//
// The harness rule that keeps a journey from ending a session it does not own
// lives in `apps/elitea-web/scripts/e2e-journey-shape.test.mjs`.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	v2auth "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/auth"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth/browsersession"
)

// theAPIRoute needs only a principal, so a refusal here is about the
// credential and nothing else.
const theAPIRoute = "/api/v2/auth/token/"

func TestALogoutRefusesEverySecondHolderOfTheSameCookie(t *testing.T) {
	store := &routeTestSessions{rows: map[string]browsersession.Session{}}
	manager, err := browsersession.NewManager(store, browsersession.Policy{
		IdleTimeout: time.Hour, AbsoluteLifetime: 24 * time.Hour,
	})
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}

	// The REAL session handler, holding the same manager the middleware reads.
	// A nil pool is what this test can supply and is all Logout needs; Info
	// answers the expiry contract before it ever reaches the user lookup.
	sessions := v2auth.NewSessionHandler(nil, "session-secret").WithSessionManager(manager)

	router := NewRouter(RouterConfig{
		SessionSecret:      "session-secret",
		PrincipalValidator: activeRoutePrincipals{},
		Auth: AuthDeps{
			SessionStore:   manager,
			SessionHandler: sessions,
		},
	})

	value, err := manager.Create(context.Background(), browsersession.NewSession{
		UserID: 11, Email: "member@autotest.local", Provider: browsersession.ProviderOIDC,
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	// TWO holders of one cookie. This is the E2E suite's shape: `auth.setup.ts`
	// writes the value to a file and every worker loads it.
	if status := serveWithSessionCookie(t, router, theAPIRoute, value); status == http.StatusUnauthorized {
		t.Fatalf("%s refused a live session before anybody signed out", theAPIRoute)
	}

	logout := httptest.NewRequest(http.MethodGet,
		"/forward-auth/logout?target_to=%2Fforward-auth%2Flogin", nil)
	logout.AddCookie(&http.Cookie{Name: browsersession.CookieName, Value: value})
	logoutRecorder := httptest.NewRecorder()
	router.ServeHTTP(logoutRecorder, logout)
	if logoutRecorder.Code != http.StatusFound {
		t.Fatalf("logout status = %d, want 302", logoutRecorder.Code)
	}

	// 1. The OTHER holder — the one that never called logout — is refused, and
	//    the body it gets NAMES the revocation.
	refused := httptest.NewRequest(http.MethodGet, theAPIRoute, nil)
	refused.AddCookie(&http.Cookie{Name: browsersession.CookieName, Value: value})
	apiRecorder := httptest.NewRecorder()
	router.ServeHTTP(apiRecorder, refused)
	if apiRecorder.Code != http.StatusUnauthorized {
		t.Fatalf("status after logout = %d, want 401", apiRecorder.Code)
	}
	var body struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(apiRecorder.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode refusal: %v (%s)", err, apiRecorder.Body.String())
	}
	if body.Error.Code != "session_revoked" || body.Error.Message != "the session was revoked" {
		t.Fatalf("refusal body = %q, want the `session_revoked` / `the session was revoked` "+
			"pair. A revoked session used to answer exactly what an ABSENT cookie answers, and "+
			"reading that as a wiring gap is what cost the 1.60.0 smoke run its E2E suite. "+
			"This code is what stops the next run from repeating that reading (#538).",
			apiRecorder.Body.String())
	}

	// 2. ...and the shell's probe says `session_expired`, which is the code it
	//    navigates the browser to the identity provider on.
	probe := httptest.NewRequest(http.MethodGet, "/forward-auth/info", nil)
	probe.AddCookie(&http.Cookie{Name: browsersession.CookieName, Value: value})
	probeRecorder := httptest.NewRecorder()
	router.ServeHTTP(probeRecorder, probe)
	if probeRecorder.Code != http.StatusUnauthorized {
		t.Fatalf("/forward-auth/info status after logout = %d, want 401", probeRecorder.Code)
	}
	if err := json.Unmarshal(probeRecorder.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode probe refusal: %v (%s)", err, probeRecorder.Body.String())
	}
	if body.Error.Code != v2auth.SessionExpiredCode {
		t.Fatalf("/forward-auth/info code = %q, want %q — the app shell navigates on this one "+
			"string and on nothing else", body.Error.Code, v2auth.SessionExpiredCode)
	}
}

// TestASessionOutlivesEveryRequestThatIsNotALogout keeps the test above from
// passing for the wrong reason: an /api/v2 route that refused the cookie for
// any reason would satisfy step 1 as well.
func TestASessionOutlivesEveryRequestThatIsNotALogout(t *testing.T) {
	store := &routeTestSessions{rows: map[string]browsersession.Session{}}
	manager, err := browsersession.NewManager(store, browsersession.Policy{
		IdleTimeout: time.Hour, AbsoluteLifetime: 24 * time.Hour,
	})
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	sessions := v2auth.NewSessionHandler(nil, "session-secret").WithSessionManager(manager)
	router := NewRouter(RouterConfig{
		SessionSecret:      "session-secret",
		PrincipalValidator: activeRoutePrincipals{},
		Auth:               AuthDeps{SessionStore: manager, SessionHandler: sessions},
	})

	value, err := manager.Create(context.Background(), browsersession.NewSession{
		UserID: 11, Provider: browsersession.ProviderOIDC,
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	for attempt := range 5 {
		if status := serveWithSessionCookie(t, router, theAPIRoute, value); status == http.StatusUnauthorized {
			t.Fatalf("request %d refused a session nobody signed out", attempt+1)
		}
	}
}
