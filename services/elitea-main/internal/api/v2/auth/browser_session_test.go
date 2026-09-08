package auth

// What a LOGIN leaves behind, and what a LOGOUT takes away.
//
// The three browser planes — OIDC, SAML and the Form graph — must agree on two
// things after shared migration 0117: a sign-in creates a session ROW, and it
// asks for the caller's personal project. Neither used to happen. The cookie
// was self-contained, so nothing recorded the session and logout could only
// delete the browser's copy; and the personal project appeared only once some
// later screen happened to read it.
//
// Two levels of check, on purpose. The behaviour of the shared helpers is
// driven directly. WHETHER EACH PLANE CALLS THEM is read off the source,
// because standing up an identity provider to observe it would test the
// provider, and because "this call was quietly dropped" is exactly the failure
// a behavioural test of the helper alone cannot see.

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth/browsersession"
)

// memorySessions implements browsersession.Store for these tests.
type memorySessions struct {
	mu   sync.Mutex
	rows map[string]browsersession.Session
}

func newMemorySessions() *memorySessions {
	return &memorySessions{rows: map[string]browsersession.Session{}}
}

func (s *memorySessions) Insert(_ context.Context, session browsersession.Session) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rows[session.ID] = session
	return nil
}

func (s *memorySessions) Get(_ context.Context, id string) (browsersession.Session, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	row, ok := s.rows[id]
	if !ok {
		return browsersession.Session{}, browsersession.ErrNotFound
	}
	return row, nil
}

func (s *memorySessions) Touch(_ context.Context, id string, seenAt time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	row, ok := s.rows[id]
	if ok {
		row.LastSeenAt = seenAt
		s.rows[id] = row
	}
	return nil
}

func (s *memorySessions) Revoke(_ context.Context, id string, revokedAt time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	row, ok := s.rows[id]
	if ok && row.RevokedAt == nil {
		row.RevokedAt = &revokedAt
		s.rows[id] = row
	}
	return nil
}

func (s *memorySessions) RevokeAllForUser(
	_ context.Context, userID int64, revokedAt time.Time,
) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var count int64
	for id, row := range s.rows {
		if row.UserID == userID && row.RevokedAt == nil {
			row.RevokedAt = &revokedAt
			s.rows[id] = row
			count++
		}
	}
	return count, nil
}

func (s *memorySessions) only(t *testing.T) browsersession.Session {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.rows) != 1 {
		t.Fatalf("the store holds %d sessions, want exactly 1", len(s.rows))
	}
	for _, row := range s.rows {
		return row
	}
	return browsersession.Session{}
}

func newTestSessions(t *testing.T) (*browsersession.Manager, *memorySessions) {
	t.Helper()
	store := newMemorySessions()
	manager, err := browsersession.NewManager(store, browsersession.Policy{
		IdleTimeout: time.Hour, AbsoluteLifetime: 24 * time.Hour,
	})
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	return manager, store
}

// recordingEnsurer records which accounts were asked for a personal project.
type recordingEnsurer struct {
	mu  sync.Mutex
	ids []int64
}

func (e *recordingEnsurer) EnsureAsync(userID int64) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.ids = append(e.ids, userID)
}

func (e *recordingEnsurer) seen() []int64 {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]int64(nil), e.ids...)
}

// TestEnsurePersonalProjectAsksOnlyForARealAccount.
//
// The id arrives as the STRING a provisioning path returned, and a value that
// is not a positive integer would name a project belonging to whichever
// account happened to share that number — or no account at all.
func TestEnsurePersonalProjectAsksOnlyForARealAccount(t *testing.T) {
	for _, test := range []struct {
		name   string
		userID string
		want   []int64
	}{
		{name: "a real account", userID: "42", want: []int64{42}},
		{name: "zero", userID: "0"},
		{name: "negative", userID: "-1"},
		{name: "not a number", userID: "system"},
		{name: "empty", userID: ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			ensurer := &recordingEnsurer{}
			ensurePersonalProject(ensurer, test.userID, time.Millisecond)
			got := ensurer.seen()
			if len(got) != len(test.want) {
				t.Fatalf("asked for %v, want %v", got, test.want)
			}
			for i := range got {
				if got[i] != test.want[i] {
					t.Fatalf("asked for %v, want %v", got, test.want)
				}
			}
		})
	}
}

// TestANilEnsurerIsAWorkingNoValue. A composition with no project provisioner
// must need no branch at the call site.
func TestANilEnsurerIsAWorkingNoValue(t *testing.T) {
	ensurePersonalProject(nil, "42", time.Millisecond)
}

// TestASignInCreatesASessionRowPerProvider. The row records WHICH plane signed
// the person in, because that is what a federated single logout has to know.
func TestASignInCreatesASessionRowPerProvider(t *testing.T) {
	for _, test := range []struct {
		name     string
		request  browsersession.NewSession
		provider string
	}{
		{
			name: "OIDC",
			request: browsersession.NewSession{
				UserID: 7, Email: "owner@example.test", Provider: browsersession.ProviderOIDC,
			},
			provider: browsersession.ProviderOIDC,
		},
		{
			name: "SAML carries the session index a LogoutRequest needs",
			request: browsersession.NewSession{
				UserID: 8, Email: "owner@example.test", Provider: browsersession.ProviderSAML,
				ProviderSessionIndex: "provider-session-index",
			},
			provider: browsersession.ProviderSAML,
		},
		{
			name: "the Form graph",
			request: browsersession.NewSession{
				UserID: 9, Email: "owner@example.test", Provider: browsersession.ProviderForm,
			},
			provider: browsersession.ProviderForm,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			manager, store := newTestSessions(t)
			recorder := httptest.NewRecorder()
			request := httptest.NewRequest(http.MethodGet, "/forward-auth/auth_oidc/callback", nil)

			if !issueBrowserSession(recorder, manager, "unused-secret", true, test.request, request) {
				t.Fatalf("issueBrowserSession refused a valid sign-in: %s", recorder.Body.String())
			}

			row := store.only(t)
			if row.UserID != test.request.UserID || row.Provider != test.provider ||
				row.ProviderSessionIndex != test.request.ProviderSessionIndex {
				t.Fatalf("row = %+v, want the sign-in it was given", row)
			}

			cookie := sessionCookieOf(t, recorder)
			if !browsersession.LooksServerSide(cookie.Value) {
				t.Fatalf("cookie value %q is not a server-side identifier", cookie.Value)
			}
			if !cookie.HttpOnly || !cookie.Secure || cookie.SameSite != http.SameSiteLaxMode {
				t.Fatalf("cookie = %+v, want HttpOnly, Secure and SameSite=Lax", cookie)
			}
			// The cookie must name the row that was written, or the browser
			// holds a value nothing can validate.
			if browsersession.CookieValue(row.ID) != cookie.Value {
				t.Fatalf("cookie %q does not name the stored session %q", cookie.Value, row.ID)
			}
		})
	}
}

// TestADeploymentWithNoStoreStillSignsPeopleIn. Composition without a pool is
// the shape every handler tolerates; the legacy signed cookie is still issued.
func TestADeploymentWithNoStoreStillSignsPeopleIn(t *testing.T) {
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/forward-auth/auth_oidc/callback", nil)
	if !issueBrowserSession(recorder, nil, "session-secret", true, browsersession.NewSession{
		UserID: 7, Email: "owner@example.test", Provider: browsersession.ProviderOIDC,
	}, request) {
		t.Fatal("issueBrowserSession refused a sign-in on a deployment with no store")
	}
	cookie := sessionCookieOf(t, recorder)
	if browsersession.LooksServerSide(cookie.Value) {
		t.Fatal("a deployment with no store issued a server-side identifier")
	}
	if _, err := verifySessionToken("session-secret", cookie.Value); err != nil {
		t.Fatalf("the legacy cookie does not verify: %v", err)
	}
}

// TestLogoutRevokesTheSessionRow. Deleting the browser's copy is not a
// sign-out: any other holder of the same value keeps using it until the
// absolute deadline. This is the property the signed cookie could never have.
func TestLogoutRevokesTheSessionRow(t *testing.T) {
	manager, store := newTestSessions(t)
	handler := NewSessionHandler(nil, "session-secret").WithSessionManager(manager)

	value, err := manager.Create(context.Background(), browsersession.NewSession{
		UserID: 7, Email: "owner@example.test", Provider: browsersession.ProviderOIDC,
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := manager.Validate(context.Background(), value); err != nil {
		t.Fatalf("the fresh session does not validate: %v", err)
	}

	request := httptest.NewRequest(http.MethodGet, "/forward-auth/logout", nil)
	request.AddCookie(&http.Cookie{Name: browsersession.CookieName, Value: value})
	recorder := httptest.NewRecorder()
	handler.Logout(recorder, request)

	if recorder.Code != http.StatusFound {
		t.Fatalf("status = %d, want 302", recorder.Code)
	}
	if row := store.only(t); row.RevokedAt == nil {
		t.Fatal("logout did not revoke the session row")
	}
	if _, err := manager.Validate(context.Background(), value); err == nil {
		t.Fatal("the session still validates after logout")
	}
	// The cookie is cleared too, with the attributes it was set with.
	cookie := sessionCookieOf(t, recorder)
	if cookie.Value != "" || cookie.MaxAge >= 0 {
		t.Fatalf("cleared cookie = %+v, want an empty value and a negative MaxAge", cookie)
	}
}

// TestSessionInfoAnswersTheExpiryContractForAServerSession is the response the
// app shell routes on. See writeSessionExpired.
func TestSessionInfoAnswersTheExpiryContractForAServerSession(t *testing.T) {
	manager, _ := newTestSessions(t)
	handler := NewSessionHandler(nil, "session-secret").WithSessionManager(manager)
	handler.users = stubUsers{row: stubRow{userID: 7}}

	value, err := manager.Create(context.Background(), browsersession.NewSession{
		UserID: 7, Email: "owner@example.test", Provider: browsersession.ProviderOIDC,
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	if recorder := infoResponse(t, handler, value); recorder.Code != http.StatusOK {
		t.Fatalf("a live session answered %d, want 200 (%s)", recorder.Code, recorder.Body.String())
	}

	if err := manager.Revoke(context.Background(), value); err != nil {
		t.Fatalf("Revoke: %v", err)
	}
	recorder := infoResponse(t, handler, value)
	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("a revoked session answered %d, want 401 (%s)", recorder.Code, recorder.Body.String())
	}
	if !strings.Contains(recorder.Body.String(), SessionExpiredCode) {
		t.Fatalf("body %s does not carry %q", recorder.Body.String(), SessionExpiredCode)
	}
	if got := recorder.Header().Get("Location"); !strings.HasPrefix(got, "/forward-auth/login?") {
		t.Fatalf("Location = %q, want the login start", got)
	}
}

// TestTheExpiryHintPreservesTheReturnTarget. The browser must come back to the
// page it was on, and the value is caller-supplied, so it goes through
// browserflow.CanonicalReturnTarget first.
func TestTheExpiryHintPreservesTheReturnTarget(t *testing.T) {
	for _, test := range []struct {
		name   string
		target string
		want   string
	}{
		{name: "a same-origin path", target: "/agents/42", want: "%2Fagents%2F42"},
		{name: "an absolute URL is refused", target: "https://evil.test/", want: "%2F"},
		{name: "a protocol-relative URL is refused", target: "//evil.test/", want: "%2F"},
		{name: "no target", target: "", want: "%2F"},
	} {
		t.Run(test.name, func(t *testing.T) {
			path := "/forward-auth/info"
			if test.target != "" {
				path += "?target_to=" + test.target
			}
			request := httptest.NewRequest(http.MethodGet, path, nil)
			recorder := httptest.NewRecorder()
			writeSessionExpired(recorder, request)

			if got := recorder.Header().Get("Location"); !strings.HasSuffix(got, test.want) {
				t.Fatalf("Location = %q, want it to end %q", got, test.want)
			}
		})
	}
}

// TestEveryFederatedSignInPathCreatesASessionAndAsksForThePersonalProject reads
// this package as SOURCE.
//
// It is the check a behavioural test cannot make. Both effects are invisible in
// the response — the row is written to a store, and the personal project is
// asked for off the request path — so a plane that stopped calling either one
// would still answer 302 with a cookie, and every response-level assertion
// would keep passing. Issue #830's class exactly: the route works, the
// behaviour is gone.
func TestEveryFederatedSignInPathCreatesASessionAndAsksForThePersonalProject(t *testing.T) {
	for _, test := range []struct {
		file     string
		function string
	}{
		{file: "oidc.go", function: "Callback"},
		{file: "saml.go", function: "ACS"},
	} {
		t.Run(test.file+"."+test.function, func(t *testing.T) {
			body := functionSource(t, test.file, test.function)
			for _, call := range []string{"issueBrowserSession", "ensurePersonalProject"} {
				if !strings.Contains(body, call+"(") {
					t.Fatalf("%s.%s no longer calls %s. A sign-in that skips it leaves "+
						"either an unrevocable session or an account with no personal project, "+
						"and the response looks identical either way.",
						test.file, test.function, call)
				}
			}
		})
	}
}

// functionSource returns the source text of one top-level method body.
func functionSource(t *testing.T, file, name string) string {
	t.Helper()
	fileSet := token.NewFileSet()
	parsed, err := parser.ParseFile(fileSet, file, nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", file, err)
	}
	raw, err := os.ReadFile(file)
	if err != nil {
		t.Fatalf("read %s: %v", file, err)
	}
	source := string(raw)
	for _, declaration := range parsed.Decls {
		function, ok := declaration.(*ast.FuncDecl)
		if !ok || function.Name.Name != name || function.Body == nil {
			continue
		}
		start := fileSet.Position(function.Body.Pos()).Offset
		end := fileSet.Position(function.Body.End()).Offset
		return source[start:end]
	}
	t.Fatalf("%s has no function %s", file, name)
	return ""
}

func sessionCookieOf(t *testing.T, recorder *httptest.ResponseRecorder) *http.Cookie {
	t.Helper()
	for _, cookie := range recorder.Result().Cookies() {
		if cookie.Name == browsersession.CookieName {
			return cookie
		}
	}
	t.Fatalf("no %s cookie was set", browsersession.CookieName)
	return nil
}
