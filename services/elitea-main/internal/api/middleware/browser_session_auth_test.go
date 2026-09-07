package middleware

// The cookie branch of apimw.Auth after shared migration 0117.
//
// THE BRANCH IT TESTS is the one a browser takes on every request. Before
// 0117 the `elitea_session` cookie was the whole credential: an HMAC check with
// no read, so a copy of the value kept working after logout and there was no
// idle deadline. The middleware now sends a value carrying the `s1.` prefix to
// the session store and keeps the legacy reader for everything else.
//
// FOUR PROPERTIES ARE HELD HERE, and each of them is a way the change could
// silently do the wrong thing:
//
//  1. A live server-side session authenticates, and the principal validator
//     still runs on it. Skipping the validator is defect #301/#314/#370.
//  2. Revoked, expired and idle sessions are refused with 401.
//  3. A store that cannot ANSWER is 503, not 401. A 401 would sign out every
//     browser for as long as the database is unreachable.
//  4. The legacy signed cookie still works, and stops working exactly when the
//     operator sets ELITEA_SESSION_REJECT_LEGACY_COOKIES.

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth/browsersession"
)

// stubSessionStore answers one prepared result. It is a stub rather than a real
// manager because the property under test is what the MIDDLEWARE does with each
// answer, and browsersession's own suite already proves which answer each state
// produces.
type stubSessionStore struct {
	session browsersession.Session
	err     error
	calls   int
}

func (s *stubSessionStore) Validate(context.Context, string) (browsersession.Session, error) {
	s.calls++
	if s.err != nil {
		return browsersession.Session{}, s.err
	}
	return s.session, nil
}

// recordingPrincipals accepts every principal and counts the calls, so a branch
// that skipped the validator is visible.
type recordingPrincipals struct{ calls int }

func (p *recordingPrincipals) ValidatePrincipal(
	_ context.Context, principal auth.User,
) (auth.User, error) {
	p.calls++
	return principal, nil
}

func legacySessionCookie(secret, userID string, expiry time.Time) string {
	payload, _ := json.Marshal(map[string]any{
		"uid": userID, "email": "owner@example.test", "exp": expiry.Unix(),
	})
	encoded := base64.RawURLEncoding.EncodeToString(payload)
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write([]byte(encoded))
	return encoded + "." + hex.EncodeToString(mac.Sum(nil))
}

func serveWithCookie(t *testing.T, config AuthConfig, value string) (*httptest.ResponseRecorder, bool) {
	t.Helper()
	reached := false
	handler := Auth(config)(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		reached = true
	}))
	request := httptest.NewRequest(http.MethodGet, "/api/v2/anything", nil)
	request.AddCookie(&http.Cookie{Name: browsersession.CookieName, Value: value})
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	return recorder, reached
}

func liveSession() browsersession.Session {
	now := time.Now().UTC()
	return browsersession.Session{
		ID: "identifier", UserID: 7, Email: "owner@example.test",
		Provider: browsersession.ProviderOIDC,
		CreatedAt: now, LastSeenAt: now, ExpiresAt: now.Add(time.Hour),
		IdleTimeout: time.Hour,
	}
}

// serverSessionCookieValue is a syntactically valid identifier. The store is a
// stub, so the bytes never matter — but the PREFIX does: it is what routes the
// value to the store instead of to the HMAC reader.
func serverSessionCookieValue(t *testing.T) string {
	t.Helper()
	id, err := browsersession.NewID()
	if err != nil {
		t.Fatal(err)
	}
	return browsersession.CookieValue(id)
}

func TestServerSessionCookieAuthenticatesAndStillValidatesThePrincipal(t *testing.T) {
	store := &stubSessionStore{session: liveSession()}
	principals := &recordingPrincipals{}
	recorder, reached := serveWithCookie(t, AuthConfig{
		SessionSecret:      "session-secret",
		SessionStore:       store,
		PrincipalValidator: principals,
	}, serverSessionCookieValue(t))

	if recorder.Code != http.StatusOK || !reached {
		t.Fatalf("status = %d, handler reached = %v; want 200 and the handler reached (body %s)",
			recorder.Code, reached, recorder.Body.String())
	}
	if store.calls != 1 {
		t.Fatalf("the session store was consulted %d times, want 1", store.calls)
	}
	if principals.calls != 1 {
		t.Fatalf("the principal validator ran %d times, want 1 — a session is not proof "+
			"that the account behind it is still active (#301, #314, #370)", principals.calls)
	}
}

func TestServerSessionRefusalsAndOutagesTakeDifferentStatuses(t *testing.T) {
	for _, test := range []struct {
		name       string
		err        error
		wantStatus int
	}{
		{name: "unknown", err: browsersession.ErrNotFound, wantStatus: http.StatusUnauthorized},
		{name: "revoked", err: browsersession.ErrRevoked, wantStatus: http.StatusUnauthorized},
		{name: "expired", err: browsersession.ErrExpired, wantStatus: http.StatusUnauthorized},
		{name: "idle", err: browsersession.ErrIdle, wantStatus: http.StatusUnauthorized},
		{
			// The store did not ANSWER. Nothing about this session was read.
			name:       "the store could not be read",
			err:        errors.New("connection refused"),
			wantStatus: http.StatusServiceUnavailable,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			store := &stubSessionStore{err: test.err}
			principals := &recordingPrincipals{}
			recorder, reached := serveWithCookie(t, AuthConfig{
				SessionSecret:      "session-secret",
				SessionStore:       store,
				PrincipalValidator: principals,
			}, serverSessionCookieValue(t))

			if recorder.Code != test.wantStatus {
				t.Fatalf("status = %d, want %d (body %s)",
					recorder.Code, test.wantStatus, recorder.Body.String())
			}
			if reached {
				t.Fatal("a refused session reached the handler")
			}
			if test.wantStatus == http.StatusServiceUnavailable &&
				recorder.Header().Get("Retry-After") == "" {
				t.Fatal("a dependency fault carried no Retry-After")
			}
		})
	}
}

// TestTheLegacyCookieWindowOpensAndCloses is the migration path.
//
// A deployment that upgrades holds unexpired legacy cookies for up to a day.
// Refusing them at the moment the new binary starts would sign every active
// user out, so the reader stays. The operator closes the window deliberately.
func TestTheLegacyCookieWindowOpensAndCloses(t *testing.T) {
	const secret = "session-secret"
	legacy := legacySessionCookie(secret, "7", time.Now().Add(time.Hour))

	for _, test := range []struct {
		name       string
		reject     bool
		wantStatus int
	}{
		{name: "the window is open by default", reject: false, wantStatus: http.StatusOK},
		{name: "the operator closed the window", reject: true, wantStatus: http.StatusUnauthorized},
	} {
		t.Run(test.name, func(t *testing.T) {
			store := &stubSessionStore{session: liveSession()}
			recorder, reached := serveWithCookie(t, AuthConfig{
				SessionSecret:              secret,
				SessionStore:               store,
				PrincipalValidator:         &recordingPrincipals{},
				RejectLegacySessionCookies: test.reject,
			}, legacy)

			if recorder.Code != test.wantStatus {
				t.Fatalf("status = %d, want %d (body %s)",
					recorder.Code, test.wantStatus, recorder.Body.String())
			}
			if reached != (test.wantStatus == http.StatusOK) {
				t.Fatalf("handler reached = %v, want %v", reached, test.wantStatus == http.StatusOK)
			}
			if store.calls != 0 {
				t.Fatalf("a legacy cookie was sent to the session store %d times, want 0 — "+
					"the prefix is the discriminator", store.calls)
			}
		})
	}
}

// TestADeploymentWithNoStoreKeepsTheLegacyReader. Every consumer holds a
// possibly-nil store, and a nil one must leave the middleware exactly as it was.
func TestADeploymentWithNoStoreKeepsTheLegacyReader(t *testing.T) {
	const secret = "session-secret"
	recorder, reached := serveWithCookie(t, AuthConfig{
		SessionSecret:      secret,
		PrincipalValidator: &recordingPrincipals{},
	}, legacySessionCookie(secret, "7", time.Now().Add(time.Hour)))

	if recorder.Code != http.StatusOK || !reached {
		t.Fatalf("status = %d, reached = %v; want 200 and the handler reached", recorder.Code, reached)
	}
}
