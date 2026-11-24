package browserauth

// The Form password prompt (ADR-0059 R1.10b), end to end: the real Form
// service, verifier and HTTP handler, the real Form session reader, and the
// real apimw.Auth in front of apimw.RecentAuthentication. What is asserted is
// what a route behind the recent-authentication check answers for the cookie
// a browser holds after each step.

import (
	"context"
	"crypto/sha256"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	apimw "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/middleware"
	browserapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/browserauth"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/identity"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/auth"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/auth/browserflow"
	sessionstate "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/auth/session"
)

type reauthenticationRig struct {
	handler  *Handler
	sessions *componentSessionStore
	admitter *attemptAdmitterStub
	cookies  *CookiePolicy
	reader   *FormSessions
}

type activePrincipals struct{}

func (activePrincipals) ValidatePrincipal(_ context.Context, principal auth.User) (auth.User, error) {
	// A validator that REBUILDS the principal, as the production one does:
	// what the helper reads must survive it.
	return auth.User{ID: principal.ID, UserID: principal.UserID, AuthType: principal.AuthType}, nil
}

func newReauthenticationRig(t *testing.T) *reauthenticationRig {
	t.Helper()
	sessions := &componentSessionStore{records: make(map[string]sessionstate.State)}
	flow, err := browserapp.NewService(
		sessions,
		&componentTransactionStore{records: make(map[string]browserflow.Transaction)},
		&componentProvisioner{result: identity.ProvisionResult{UserID: 71}},
		componentPrincipalValidator{},
		5*time.Minute,
	)
	if err != nil {
		t.Fatal(err)
	}
	provider, err := browserapp.NewFormProvider([]byte(`{"users":[
		{"login":"admin","password":"secret","email":"admin@example.test"}
	]}`))
	if err != nil {
		t.Fatal(err)
	}
	cookies, err := NewCookiePolicy(CookieConfig{
		Name: "centry_auth_session", Secure: true,
		SameSite: http.SameSiteLaxMode, Lifetime: 7 * 24 * time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	events := make([]string, 0, 8)
	admitter := &attemptAdmitterStub{events: &events}
	handler, err := NewHandler(flow, provider, admitter,
		&clientKeyResolverStub{events: &events, key: "client-7"}, cookies,
		Config{DefaultLoginTarget: "/", DefaultLogoutTarget: "/"})
	if err != nil {
		t.Fatal(err)
	}
	reader, err := NewFormSessions(cookies, flow)
	if err != nil {
		t.Fatal(err)
	}
	return &reauthenticationRig{handler: handler, sessions: sessions, admitter: admitter, cookies: cookies, reader: reader}
}

// signIn runs the ordinary Form sign-in and answers the authenticated cookie.
func (rig *reauthenticationRig) signIn(t *testing.T) *http.Cookie {
	t.Helper()
	begin := httptest.NewRecorder()
	mount(rig.handler).ServeHTTP(begin, httptest.NewRequest(http.MethodGet, BasePath+LoginPath, nil))
	location, err := url.Parse(begin.Header().Get("Location"))
	if err != nil || len(begin.Result().Cookies()) != 1 {
		t.Fatalf("begin: %d %q", begin.Code, begin.Header().Get("Location"))
	}
	form := url.Values{
		"target": {location.Query().Get("target_to")}, "login": {"admin"}, "password": {"secret"},
	}.Encode()
	request := httptest.NewRequest(http.MethodPost, BasePath+FormAuthorizePath, strings.NewReader(form))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.AddCookie(begin.Result().Cookies()[0])
	recorder := httptest.NewRecorder()
	mount(rig.handler).ServeHTTP(recorder, request)
	if recorder.Code != http.StatusFound || len(recorder.Result().Cookies()) != 1 {
		t.Fatalf("sign-in: %d %q", recorder.Code, recorder.Body.String())
	}
	return recorder.Result().Cookies()[0]
}

func (rig *reauthenticationRig) sessionID(t *testing.T, cookie *http.Cookie) string {
	t.Helper()
	request := httptest.NewRequest(http.MethodGet, "/", nil)
	request.AddCookie(cookie)
	id, err := rig.cookies.Read(request)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

// age moves the session's last password entry into the past, as time would.
func (rig *reauthenticationRig) age(t *testing.T, cookie *http.Cookie, by time.Duration) {
	t.Helper()
	id := rig.sessionID(t, cookie)
	state := rig.sessions.records[id]
	if state.AuthenticatedAt == nil {
		t.Fatal("the Form sign-in recorded no authentication time")
	}
	older := state.AuthenticatedAt.Add(-by)
	state.AuthenticatedAt = &older
	rig.sessions.records[id] = state
}

// recent answers whether a route behind apimw.Auth and the
// recent-authentication check admits the cookie, and the kind it saw.
func (rig *reauthenticationRig) recent(t *testing.T, cookie *http.Cookie) (apimw.SessionKind, int) {
	t.Helper()
	var kind apimw.SessionKind
	check := apimw.RecentAuthentication{MaxAge: 15 * time.Minute}
	handler := apimw.Auth(apimw.AuthConfig{FormSessions: rig.reader, PrincipalValidator: activePrincipals{}})(
		check.Require(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			authentication, _ := apimw.BrowserAuthenticationOf(r)
			kind = authentication.Kind
			w.WriteHeader(http.StatusNoContent)
		})))
	request := httptest.NewRequest(http.MethodPost, "/api/v2/anything", nil)
	request.AddCookie(cookie)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	return kind, recorder.Code
}

// prompt renders the password prompt and answers its token.
func (rig *reauthenticationRig) prompt(t *testing.T, cookie *http.Cookie) string {
	t.Helper()
	request := httptest.NewRequest(http.MethodGet, BasePath+FormReauthenticatePath+"?target_to=%2Fadmin%2Froles", nil)
	request.AddCookie(cookie)
	recorder := httptest.NewRecorder()
	mount(rig.handler).ServeHTTP(recorder, request)
	body := recorder.Body.String()
	if recorder.Code != http.StatusOK || !strings.Contains(body, `action="/auth/form/reauthenticate"`) ||
		!strings.Contains(body, "admin") {
		t.Fatalf("prompt: %d %s", recorder.Code, body)
	}
	return formReauthenticationToken(rig.sessionID(t, cookie))
}

func (rig *reauthenticationRig) submit(t *testing.T, cookie *http.Cookie, token, password string) *httptest.ResponseRecorder {
	t.Helper()
	form := url.Values{
		"target": {"/admin/roles"}, "token": {token}, "login": {"admin"}, "password": {password},
	}.Encode()
	request := httptest.NewRequest(http.MethodPost, BasePath+FormReauthenticatePath, strings.NewReader(form))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.AddCookie(cookie)
	recorder := httptest.NewRecorder()
	mount(rig.handler).ServeHTTP(recorder, request)
	return recorder
}

func TestAFreshFormSignInSatisfiesTheRecentAuthenticationCheck(t *testing.T) {
	rig := newReauthenticationRig(t)
	cookie := rig.signIn(t)
	kind, status := rig.recent(t, cookie)
	if status != http.StatusNoContent || kind != apimw.SessionKindForm {
		t.Fatalf("a Form session signed in just now: status %d kind %q, want 204 %q", status, kind, apimw.SessionKindForm)
	}
}

// The acceptance criterion: the session passes the helper after the password
// is entered again, and not before; a wrong password moves nothing and is
// counted by the limiter.
func TestAFormSessionPassesTheCheckAfterThePasswordIsEnteredAgainAndNotBefore(t *testing.T) {
	rig := newReauthenticationRig(t)
	cookie := rig.signIn(t)
	rig.age(t, cookie, time.Hour)

	if _, status := rig.recent(t, cookie); status != http.StatusUnauthorized {
		t.Fatalf("a Form session last authenticated an hour ago: status %d, want 401", status)
	}

	token := rig.prompt(t, cookie)
	before := rig.sessions.records[rig.sessionID(t, cookie)].AuthenticatedAt.UTC()
	callsBefore := rig.admitter.calls

	wrong := rig.submit(t, cookie, token, "not-the-password")
	if wrong.Code != http.StatusFound ||
		wrong.Header().Get("Location") != BasePath+FormReauthenticatePath+"?error=true&target_to=%2Fadmin%2Froles" {
		t.Fatalf("a wrong password: %d %q", wrong.Code, wrong.Header().Get("Location"))
	}
	if len(wrong.Result().Cookies()) != 0 {
		t.Fatalf("a wrong password set cookies: %+v", wrong.Result().Cookies())
	}
	if rig.admitter.calls != callsBefore+1 || rig.admitter.attempt.Stage != BrowserAttemptFormCredential ||
		rig.admitter.attempt.LoginDigest != sha256.Sum256([]byte("admin")) {
		t.Fatalf("a wrong password was not counted as a Form credential attempt for the login: %+v", rig.admitter.attempt)
	}
	if after := rig.sessions.records[rig.sessionID(t, cookie)].AuthenticatedAt.UTC(); !after.Equal(before) {
		t.Fatalf("a wrong password moved the authentication time from %s to %s", before, after)
	}
	if _, status := rig.recent(t, cookie); status != http.StatusUnauthorized {
		t.Fatalf("after a wrong password: status %d, want 401", status)
	}

	right := rig.submit(t, cookie, token, "secret")
	if right.Code != http.StatusFound || right.Header().Get("Location") != "/admin/roles" {
		t.Fatalf("the right password: %d %q %s", right.Code, right.Header().Get("Location"), right.Body.String())
	}
	if rig.admitter.calls != callsBefore+2 {
		t.Fatalf("the re-entry was not counted: %d attempts", rig.admitter.calls-callsBefore)
	}
	rotated := right.Result().Cookies()
	if len(rotated) != 1 || rotated[0].Value == cookie.Value {
		t.Fatalf("a re-entry did not rotate the session cookie: %+v", rotated)
	}
	kind, status := rig.recent(t, rotated[0])
	if status != http.StatusNoContent || kind != apimw.SessionKindForm {
		t.Fatalf("after the re-entry: status %d kind %q, want 204 %q", status, kind, apimw.SessionKindForm)
	}
	if _, status := rig.recent(t, cookie); status == http.StatusNoContent {
		t.Fatal("the pre-rotation cookie still passes the check")
	}
}

// A POST the prompt did not render for this session (a cross-site form, a
// stale tab) verifies nothing and spends no attempt.
func TestAReEntryWithoutThePromptsTokenIsNotVerified(t *testing.T) {
	rig := newReauthenticationRig(t)
	cookie := rig.signIn(t)
	rig.age(t, cookie, time.Hour)
	calls := rig.admitter.calls

	recorder := rig.submit(t, cookie, "forged-token", "secret")
	if recorder.Code != http.StatusFound ||
		recorder.Header().Get("Location") != BasePath+FormReauthenticatePath+"?target_to=%2Fadmin%2Froles" {
		t.Fatalf("a forged token: %d %q", recorder.Code, recorder.Header().Get("Location"))
	}
	if rig.admitter.calls != calls || len(recorder.Result().Cookies()) != 0 {
		t.Fatalf("a forged token reached the limiter (%d) or set cookies", rig.admitter.calls-calls)
	}
	if _, status := rig.recent(t, cookie); status != http.StatusUnauthorized {
		t.Fatalf("a forged token moved the authentication time: status %d", status)
	}
}

// A browser with no Form session is sent to the ordinary sign-in, with the
// same target: a sign-in is a password entry too.
func TestThePromptWithoutAFormSessionSendsTheBrowserToSignIn(t *testing.T) {
	rig := newReauthenticationRig(t)
	recorder := httptest.NewRecorder()
	mount(rig.handler).ServeHTTP(recorder,
		httptest.NewRequest(http.MethodGet, BasePath+FormReauthenticatePath+"?target_to=%2Fadmin%2Froles", nil))
	if recorder.Code != http.StatusFound ||
		recorder.Header().Get("Location") != BasePath+LoginPath+"?target_to=%2Fadmin%2Froles" {
		t.Fatalf("no session: %d %q", recorder.Code, recorder.Header().Get("Location"))
	}
	requireSecurityHeaders(t, recorder.Header())
}

// A target that is not a same-origin path is replaced by the default, never
// followed.
func TestThePromptNeverFollowsAForeignTarget(t *testing.T) {
	rig := newReauthenticationRig(t)
	recorder := httptest.NewRecorder()
	mount(rig.handler).ServeHTTP(recorder,
		httptest.NewRequest(http.MethodGet, BasePath+FormReauthenticatePath+"?target_to=https%3A%2F%2Fevil.example", nil))
	if location := recorder.Header().Get("Location"); strings.Contains(location, "evil") {
		t.Fatalf("a foreign target was carried: %q", location)
	}
}
