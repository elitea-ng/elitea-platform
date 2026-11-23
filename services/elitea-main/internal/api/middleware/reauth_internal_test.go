package middleware

// The recent-authentication helper (ADR-0059 R1.10a).
//
// Every case runs through the REAL apimw.Auth, so what is asserted is what a
// route behind Auth then RecentAuthentication.Require would answer, not what
// the helper does with a hand-built context. A principal the helper was never
// meant to accept is given every field a fresh browser session carries (a
// recent BrowserSessionAuthenticatedAt, a positive BrowserSessionRef), so a
// helper that trusted those fields alone fails here.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/auth"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/auth/browsersession"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/auth/runtoken"
)

// reauthNow is the clock of every case.
var reauthNow = time.Date(2026, time.November, 23, 12, 0, 0, 0, time.UTC)

// forgedFreshPrincipal claims everything a fresh browser session has.
func forgedFreshPrincipal() auth.User {
	return auth.User{
		ID: "7", UserID: "7", Email: "owner@example.test",
		BrowserSessionRef:             99,
		BrowserSessionAuthenticatedAt: reauthNow.Add(-time.Minute),
		BrowserSessionProvider:        browsersession.ProviderOIDC,
	}
}

type fixedTokenValidator struct{ user auth.User }

func (v fixedTokenValidator) ValidateToken(context.Context, string) (auth.User, error) {
	return v.user, nil
}

type fixedRunTokenValidator struct{ user auth.User }

func (v fixedRunTokenValidator) ValidateRunToken(context.Context, string, string) (auth.User, runtoken.Binding, error) {
	return v.user, runtoken.Binding{ProjectID: 1, ExecutionID: "run-1"}, nil
}

func sessionAuthenticatedAt(authenticatedAt time.Time) browsersession.Session {
	session := liveSession()
	session.Ref = 42
	session.AuthenticatedAt = authenticatedAt
	return session
}

// serveRecent runs request through Auth, then the helper, and reports the
// answer and whether the protected handler ran.
func serveRecent(t *testing.T, config AuthConfig, request *http.Request) (*httptest.ResponseRecorder, bool) {
	t.Helper()
	reached := false
	check := RecentAuthentication{MaxAge: 15 * time.Minute, Now: func() time.Time { return reauthNow }}
	handler := Auth(config)(check.Require(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		reached = true
	})))
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	return recorder, reached
}

func sessionRequest(t *testing.T) *http.Request {
	t.Helper()
	request := httptest.NewRequest(http.MethodPost, "/api/v2/anything", nil)
	request.AddCookie(&http.Cookie{Name: browsersession.CookieName, Value: serverSessionCookieValue(t)})
	return request
}

func assertReauthenticationRequired(t *testing.T, recorder *httptest.ResponseRecorder, reached bool) {
	t.Helper()
	if reached {
		t.Fatal("the protected handler ran for a request without a recent authentication")
	}
	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401: %s", recorder.Code, recorder.Body.String())
	}
	if code := jsonErrorCode(t, recorder); code != CodeReauthenticationRequired {
		t.Fatalf("error code = %q, want %q", code, CodeReauthenticationRequired)
	}
	if got := recorder.Header().Get(HeaderReauthenticationMaxAge); got != strconv.Itoa(15*60) {
		t.Fatalf("%s = %q, want %q", HeaderReauthenticationMaxAge, got, "900")
	}
}

func TestRecentAuthenticationRefusesASessionOlderThanTheLimit(t *testing.T) {
	store := &stubSessionStore{session: sessionAuthenticatedAt(reauthNow.Add(-16 * time.Minute))}
	recorder, reached := serveRecent(t, AuthConfig{
		SessionStore: store, PrincipalValidator: &recordingPrincipals{},
	}, sessionRequest(t))
	assertReauthenticationRequired(t, recorder, reached)
}

func TestRecentAuthenticationPassesAFreshSessionAndNamesItsKind(t *testing.T) {
	session := sessionAuthenticatedAt(reauthNow.Add(-14 * time.Minute))
	var seen BrowserAuthentication
	check := RecentAuthentication{MaxAge: 15 * time.Minute, Now: func() time.Time { return reauthNow }}
	handler := Auth(AuthConfig{
		SessionStore: &stubSessionStore{session: session}, PrincipalValidator: &recordingPrincipals{},
	})(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		authentication, ok := check.Check(r)
		if !ok {
			t.Fatal("an OIDC session authenticated 14 minutes ago did not satisfy a 15-minute limit")
		}
		seen = authentication
	}))
	handler.ServeHTTP(httptest.NewRecorder(), sessionRequest(t))
	if seen.Kind != SessionKindSession || seen.Provider != browsersession.ProviderOIDC ||
		!seen.AuthenticatedAt.Equal(session.AuthenticatedAt) {
		t.Fatalf("authentication = %+v, want kind %q at %s", seen, SessionKindSession, session.AuthenticatedAt)
	}
}

// A SAML or Form row records the sign-in, which is no proof of a fresh
// authentication until R1.10b (ForceAuthn, the password re-entry). An OIDC
// row whose authentication time is unknown (no auth_time) is refused too.
func TestRecentAuthenticationRefusesASessionWhoseAuthenticationIsNotProven(t *testing.T) {
	saml := sessionAuthenticatedAt(reauthNow.Add(-time.Minute))
	saml.Provider = browsersession.ProviderSAML
	form := sessionAuthenticatedAt(reauthNow.Add(-time.Minute))
	form.Provider = browsersession.ProviderForm
	unknown := sessionAuthenticatedAt(time.Time{})
	for name, session := range map[string]browsersession.Session{
		"a SAML session":                       saml,
		"a Form session":                       form,
		"an OIDC session with no auth_time":    unknown,
		"a sealed session (a future provider)": func() browsersession.Session { s := saml; s.Provider = "sealed"; return s }(),
	} {
		t.Run(name, func(t *testing.T) {
			recorder, reached := serveRecent(t, AuthConfig{
				SessionStore: &stubSessionStore{session: session}, PrincipalValidator: &recordingPrincipals{},
			}, sessionRequest(t))
			assertReauthenticationRequired(t, recorder, reached)
		})
	}
}

// policyStore is a session store that also states its policy, as
// *browsersession.Manager does.
type policyStore struct {
	stubSessionStore
	policy browsersession.Policy
}

func (s *policyStore) Policy() browsersession.Policy { return s.policy }

// ELITEA_SESSION_REAUTHENTICATION_MAX_AGE reaches the check: the zero-value
// RecentAuthentication applies the limit of the session manager Auth holds,
// and the refusal states it.
func TestRecentAuthenticationAppliesTheDeploymentsLimit(t *testing.T) {
	serve := func(authenticatedAt time.Time) (*httptest.ResponseRecorder, bool) {
		store := &policyStore{
			stubSessionStore: stubSessionStore{session: sessionAuthenticatedAt(authenticatedAt)},
			policy:           browsersession.Policy{ReauthenticationMaxAge: 5 * time.Minute},
		}
		reached := false
		check := RecentAuthentication{Now: func() time.Time { return reauthNow }}
		handler := Auth(AuthConfig{SessionStore: store, PrincipalValidator: &recordingPrincipals{}})(
			check.Require(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { reached = true })))
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, sessionRequest(t))
		return recorder, reached
	}

	if _, reached := serve(reauthNow.Add(-4 * time.Minute)); !reached {
		t.Fatal("a session 4 minutes old was refused under a 5-minute limit")
	}
	recorder, reached := serve(reauthNow.Add(-6 * time.Minute))
	if reached || recorder.Code != http.StatusUnauthorized {
		t.Fatalf("a session 6 minutes old under a 5-minute limit: status %d, reached %v", recorder.Code, reached)
	}
	if got := recorder.Header().Get(HeaderReauthenticationMaxAge); got != "300" {
		t.Fatalf("%s = %q, want the deployment's 300", HeaderReauthenticationMaxAge, got)
	}
}

// The real manager is a policy source; the composition root boxes it as the
// SessionStore, so the limit it read from the environment is the one applied.
var _ sessionPolicySource = (*browsersession.Manager)(nil)

// The authentication time, not the last use, decides: a session in constant
// use (last_seen_at a second ago) whose sign-in is old is refused.
func TestRecentAuthenticationIgnoresRecentActivity(t *testing.T) {
	session := sessionAuthenticatedAt(reauthNow.Add(-2 * time.Hour))
	session.CreatedAt = reauthNow.Add(-2 * time.Hour)
	session.LastSeenAt = reauthNow.Add(-time.Second)
	recorder, reached := serveRecent(t, AuthConfig{
		SessionStore: &stubSessionStore{session: session}, PrincipalValidator: &recordingPrincipals{},
	}, sessionRequest(t))
	assertReauthenticationRequired(t, recorder, reached)
}

func TestRecentAuthenticationIsNeverSatisfiedByATokenANativeSessionOrARunToken(t *testing.T) {
	personalToken := forgedFreshPrincipal()
	personalToken.TokenID = "11"
	nativeSession := forgedFreshPrincipal()
	nativeSession.NativeClientID = "elitea-desktop"
	runActor := forgedFreshPrincipal()
	runActor.TokenID = "12"

	for name, testCase := range map[string]struct {
		config  AuthConfig
		request func() *http.Request
	}{
		"a personal access token": {
			config: AuthConfig{Validator: fixedTokenValidator{user: personalToken}, PrincipalValidator: &recordingPrincipals{}},
			request: func() *http.Request {
				request := httptest.NewRequest(http.MethodPost, "/api/v2/anything", nil)
				request.Header.Set("Authorization", "Bearer pat-value")
				return request
			},
		},
		"a personal access token as X-API-Key": {
			config: AuthConfig{Validator: fixedTokenValidator{user: personalToken}, PrincipalValidator: &recordingPrincipals{}},
			request: func() *http.Request {
				request := httptest.NewRequest(http.MethodPost, "/api/v2/anything", nil)
				request.Header.Set("X-API-Key", "pat-value")
				return request
			},
		},
		"a native session": {
			config: AuthConfig{Validator: fixedTokenValidator{user: nativeSession}, PrincipalValidator: &recordingPrincipals{}},
			request: func() *http.Request {
				request := httptest.NewRequest(http.MethodPost, "/api/v2/anything", nil)
				request.Header.Set("Authorization", "Bearer native-access-token")
				return request
			},
		},
		"a run token": {
			config: AuthConfig{
				PrincipalValidator: &recordingPrincipals{},
				RunTokens: &RunTokenScope{
					Audience: runtoken.AudienceModelGateway, Validator: fixedRunTokenValidator{user: runActor},
				},
			},
			request: func() *http.Request {
				request := httptest.NewRequest(http.MethodPost, "/api/v2/anything", nil)
				request.Header.Set("Authorization", "Bearer "+runtoken.Prefix+"value")
				return request
			},
		},
	} {
		t.Run(name, func(t *testing.T) {
			recorder, reached := serveRecent(t, testCase.config, testCase.request())
			assertReauthenticationRequired(t, recorder, reached)
		})
	}
}

// The legacy signed cookie records no authentication time and no row, so it
// never satisfies the helper either.
func TestRecentAuthenticationIsNeverSatisfiedByTheLegacySignedCookie(t *testing.T) {
	const secret = "session-secret"
	request := httptest.NewRequest(http.MethodPost, "/api/v2/anything", nil)
	request.AddCookie(&http.Cookie{
		Name: browsersession.CookieName, Value: legacySessionCookie(secret, "7", time.Now().Add(time.Hour)),
	})
	recorder, reached := serveRecent(t, AuthConfig{
		SessionSecret: secret, PrincipalValidator: &recordingPrincipals{},
	}, request)
	assertReauthenticationRequired(t, recorder, reached)
}

func TestRecentAuthenticationZeroValueUsesTheDefaultLimit(t *testing.T) {
	request := httptest.NewRequest(http.MethodGet, "/", nil)
	if got := (RecentAuthentication{}).MaxAgeFor(request); got != browsersession.DefaultReauthenticationMaxAge {
		t.Fatalf("default limit = %s, want %s", got, browsersession.DefaultReauthenticationMaxAge)
	}
}

func jsonErrorCode(t *testing.T, recorder *httptest.ResponseRecorder) string {
	t.Helper()
	var body jsonError
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode the refusal: %v: %s", err, recorder.Body.String())
	}
	return body.Error.Code
}
