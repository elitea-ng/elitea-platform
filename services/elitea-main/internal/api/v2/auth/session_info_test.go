package auth

// GET /forward-auth/info: how a failed user lookup is reported.
//
// DEFECT. Every failure exit of SessionHandler.Info answered
// `200 {"authenticated": false}`, including the error from the user SELECT. A
// database that cannot serve a query was therefore indistinguishable from a
// browser with no cookie.
//
// SCENARIO. The pool fails for a moment (failover, connection saturation). The
// browser holds a valid, unexpired elitea_session cookie. The web app reads
// `authenticated: false`, drops the user and redirects to the identity
// provider. The provider still holds its own session, so it bounces the
// browser back to the callback, where user provisioning hits the same broken
// pool and renders a bare 500 page. A recoverable blip costs a forced sign-out.
//
// EVIDENCE. oidc.go already separates pgx.ErrNoRows (absent or suspended user)
// from a real query error. Info did not.

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

	"github.com/jackc/pgx/v5"
)

// stubRow returns one prepared error or user id for Scan.
type stubRow struct {
	err    error
	userID int64
}

func (r stubRow) Scan(dest ...any) error {
	if r.err != nil {
		return r.err
	}
	if len(dest) == 1 {
		if target, ok := dest[0].(*int64); ok {
			*target = r.userID
		}
	}
	return nil
}

type stubUsers struct {
	row stubRow
}

func (s stubUsers) QueryRow(context.Context, string, ...any) pgx.Row { return s.row }

func infoResponse(t *testing.T, handler *SessionHandler, token string) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(http.MethodGet, "/forward-auth/info", nil)
	if token != "" {
		request.AddCookie(&http.Cookie{Name: "elitea_session", Value: token})
	}
	recorder := httptest.NewRecorder()
	handler.Info(recorder, request)
	return recorder
}

func TestSessionInfoSeparatesAStoreOutageFromNoSession(t *testing.T) {
	const secret = "session-secret"
	token := makeSessionToken(secret, "7", "owner@example.test")

	for _, test := range []struct {
		name          string
		row           stubRow
		wantStatus    int
		wantAuth      bool
		wantRetry     bool
		wantErrorCode string
		wantLoginURL  bool
	}{
		{
			name:       "a live session stays authenticated",
			row:        stubRow{userID: 7},
			wantStatus: http.StatusOK,
			wantAuth:   true,
		},
		{
			// The caller HELD a session and the account behind it is gone or
			// suspended. That is an expiry, not "you were never signed in":
			// the browser is sitting on a screen it can no longer load, so it
			// must go back to the identity provider. See writeSessionExpired.
			name:          "an absent or suspended user is expired, not merely unauthenticated",
			row:           stubRow{err: pgx.ErrNoRows},
			wantStatus:    http.StatusUnauthorized,
			wantAuth:      false,
			wantErrorCode: SessionExpiredCode,
			wantLoginURL:  true,
		},
		{
			name:          "a store outage is not an answer about the caller",
			row:           stubRow{err: errors.New("connection refused")},
			wantStatus:    http.StatusServiceUnavailable,
			wantRetry:     true,
			wantErrorCode: "session_store_unavailable",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			handler := NewSessionHandler(nil, secret)
			handler.users = stubUsers{row: test.row}

			recorder := infoResponse(t, handler, token)
			if recorder.Code != test.wantStatus {
				t.Fatalf("status = %d, want %d (body %s)", recorder.Code, test.wantStatus, recorder.Body.String())
			}

			var body map[string]any
			if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
				t.Fatalf("body %q: %v", recorder.Body.String(), err)
			}
			if test.wantErrorCode != "" {
				envelope, ok := body["error"].(map[string]any)
				if !ok || envelope["code"] != test.wantErrorCode {
					t.Fatalf("body = %v, want error code %q", body, test.wantErrorCode)
				}
			}
			if test.wantStatus == http.StatusServiceUnavailable {
				// The outage must never carry the sign-out verdict.
				if _, present := body["authenticated"]; present {
					t.Fatalf("body = %v, want no authenticated field", body)
				}
			} else if body["authenticated"] != test.wantAuth {
				t.Fatalf("authenticated = %v, want %v", body["authenticated"], test.wantAuth)
			}
			if test.wantLoginURL {
				// The hint the app shell navigates to, in the body and in the
				// header, so a caller can read whichever it already reads.
				if body["login_url"] == nil || body["login_url"] == "" {
					t.Fatalf("body = %v, want a login_url hint", body)
				}
				if recorder.Header().Get("Location") != body["login_url"] {
					t.Fatalf("Location = %q, want the login_url %v",
						recorder.Header().Get("Location"), body["login_url"])
				}
			}
			if got := recorder.Header().Get("Retry-After"); (got != "") != test.wantRetry {
				t.Fatalf("Retry-After = %q, want present=%v", got, test.wantRetry)
			}
		})
	}
}

// TestSessionInfoReportsAMisWiredHandler pins the split of the composition
// error away from the outage. A nil user store means the route was mounted
// without a pool, which no retry can repair.
func TestSessionInfoReportsAMisWiredHandler(t *testing.T) {
	const secret = "session-secret"
	handler := NewSessionHandler(nil, secret)

	recorder := infoResponse(t, handler, makeSessionToken(secret, "7", "owner@example.test"))
	if recorder.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500 (body %s)", recorder.Code, recorder.Body.String())
	}
}

// TestSessionInfoSeparatesNoCookieFromAnUnusableOne holds the ONE distinction
// the app shell routes on.
//
// NO COOKIE is 200 `authenticated: false`. Nobody was signed in, the shell
// shows its sign-in affordance, and nothing navigates.
//
// A COOKIE THAT NO LONGER WORKS is 401 `session_expired`. The user WAS signed
// in and is now looking at a screen that cannot load, so the shell sends the
// browser to the login start. Answering 200 for this case is the defect the
// smoke run found: an expired user sat on a dead page with no redirect.
func TestSessionInfoSeparatesNoCookieFromAnUnusableOne(t *testing.T) {
	const secret = "session-secret"
	handler := NewSessionHandler(nil, secret)
	handler.users = stubUsers{row: stubRow{userID: 7}}

	for _, test := range []struct {
		name       string
		token      string
		wantStatus int
	}{
		{name: "no cookie", token: "", wantStatus: http.StatusOK},
		{
			name:       "bad signature",
			token:      makeSessionToken("other-secret", "7", "owner@example.test"),
			wantStatus: http.StatusUnauthorized,
		},
		{
			name:       "no user id",
			token:      makeSessionToken(secret, "0", "owner@example.test"),
			wantStatus: http.StatusUnauthorized,
		},
		{
			name:       "an expired signed cookie",
			token:      expiredSessionToken(secret, "7", "owner@example.test"),
			wantStatus: http.StatusUnauthorized,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			recorder := infoResponse(t, handler, test.token)
			if recorder.Code != test.wantStatus {
				t.Fatalf("status = %d, want %d", recorder.Code, test.wantStatus)
			}
			var body map[string]any
			if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
				t.Fatal(err)
			}
			if body["authenticated"] != false {
				t.Fatalf("authenticated = %v, want false", body["authenticated"])
			}
			if test.wantStatus != http.StatusUnauthorized {
				if _, present := body["error"]; present {
					t.Fatalf("body = %v, want no error envelope on a plain no-session answer", body)
				}
				return
			}
			envelope, ok := body["error"].(map[string]any)
			if !ok || envelope["code"] != SessionExpiredCode {
				t.Fatalf("body = %v, want error code %q", body, SessionExpiredCode)
			}
		})
	}
}

// expiredSessionToken signs a legacy cookie whose `exp` has already passed.
func expiredSessionToken(secret, userID, email string) string {
	payload, _ := json.Marshal(map[string]any{
		"uid": userID, "email": email, "exp": time.Now().Add(-time.Hour).Unix(),
	})
	encoded := base64.RawURLEncoding.EncodeToString(payload)
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write([]byte(encoded))
	return encoded + "." + hex.EncodeToString(mac.Sum(nil))
}
