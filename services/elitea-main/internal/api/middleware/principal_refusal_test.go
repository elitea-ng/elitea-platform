package middleware_test

// A database fault on the authentication path is a 5xx, not a 401 (#537).
//
// The four credential paths in middleware.Auth all called writeInactivePrincipal
// when the principal check failed, whatever the failure was. A connection-pool
// timeout and a suspended account then carried the same status and the same
// body, and nothing was written to the log, so an outage read as "your session
// expired" and could not be told apart from a suspension after the fact.
//
// The tests below drive the REAL validator — authsvc.NewPrincipalValidator over
// a closed pool — rather than a stub, so they pin what a caller of the shipped
// composition sees.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/middleware"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/middleware/authtest"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/auth"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/authsvc"
)

// unreachablePrincipalStore builds the production validator over a pool that
// is closed, so every query fails the way a dependency fault fails. It opens
// no connection: pgxpool.New parses the DSN and returns, and the pool is shut
// before anything asks it for one.
func unreachablePrincipalStore(t *testing.T) middleware.PrincipalValidator {
	t.Helper()
	pool, err := pgxpool.New(context.Background(),
		"postgres://principal:validation@127.0.0.1:1/unreachable?sslmode=disable")
	if err != nil {
		t.Fatalf("build the principal store pool: %v", err)
	}
	pool.Close()
	return authsvc.NewPrincipalValidator(pool)
}

// captureLog redirects the default logger for one test and returns what the
// test wrote to it. The tests that use it do not run in parallel: the default
// logger is process-wide.
func captureLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var recorded bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&recorded, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(previous) })
	return &recorded
}

func decodeErrorBody(t *testing.T, recorder *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var body struct {
		Error map[string]any `json:"error"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode the error body %q: %v", recorder.Body.String(), err)
	}
	return body.Error
}

// TestAuthAnswersServiceUnavailableWhenThePrincipalStoreFails is the defect
// itself: the store never answered, so the caller must not be told its
// principal is inactive.
func TestAuthAnswersServiceUnavailableWhenThePrincipalStoreFails(t *testing.T) {
	recorded := captureLog(t)
	handler := middleware.Auth(middleware.AuthConfig{
		Validator: tokenValidatorFunc(func(context.Context, string) (auth.User, error) {
			return auth.User{ID: "7", UserID: "7", AuthType: "user"}, nil
		}),
		PrincipalValidator: unreachablePrincipalStore(t),
	})(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("a request whose principal could not be read reached the protected handler")
	}))

	request := httptest.NewRequest(http.MethodPut, "/api/v2/projects/1", nil)
	request.Header.Set("Authorization", "Bearer valid-credential")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d: a database fault is not an inactive principal",
			recorder.Code, http.StatusServiceUnavailable)
	}
	body := decodeErrorBody(t, recorder)
	if message, _ := body["message"].(string); strings.Contains(message, "inactive") {
		t.Errorf("the body says %q, which names the wrong cause", message)
	}
	if message, _ := body["message"].(string); strings.Contains(message, "127.0.0.1") ||
		strings.Contains(message, "closed pool") {
		t.Errorf("the raw store error crossed the trust boundary: %q", message)
	}
	assertRefusalLogged(t, recorded, "principal_store_unavailable", "bearer_token")
}

// TestAuthLogsEveryPrincipalRefusalWithItsReason pins the second half of the
// correction. Without the line, a burst of refusals cannot be read after the
// fact: a store that REFUSED the principal and a store that failed EARLY and
// read nothing looked identical in the log, because neither wrote anything.
func TestAuthLogsEveryPrincipalRefusalWithItsReason(t *testing.T) {
	recorded := captureLog(t)
	handler := middleware.Auth(middleware.AuthConfig{
		Validator: tokenValidatorFunc(func(context.Context, string) (auth.User, error) {
			return auth.User{ID: "7", UserID: "7", AuthType: "user"}, nil
		}),
		PrincipalValidator: principalValidatorFunc(func(context.Context, auth.User) (auth.User, error) {
			return auth.User{}, fmt.Errorf("account 7: %w", auth.ErrPrincipalInactive)
		}),
	})(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("a suspended principal reached the protected handler")
	}))

	request := httptest.NewRequest(http.MethodGet, "/api/v2/projects", nil)
	request.Header.Set("Authorization", "Bearer valid-credential")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d: a suspended principal is still a 401",
			recorder.Code, http.StatusUnauthorized)
	}
	assertRefusalLogged(t, recorded, "principal_inactive", "bearer_token")
}

// TestAuthSeparatesTheRefusalReasonsOnEverySource walks the other three
// credential paths. Each one collapsed the same way, and a correction applied
// to one of four is a correction an operator cannot rely on.
func TestAuthSeparatesTheRefusalReasonsOnEverySource(t *testing.T) {
	cases := []struct {
		name    string
		source  string
		build   func(*testing.T, middleware.PrincipalValidator) http.Handler
		request func() *http.Request
	}{
		{
			name:   "api key",
			source: "api_key",
			build: func(_ *testing.T, validator middleware.PrincipalValidator) http.Handler {
				return middleware.Auth(middleware.AuthConfig{
					Validator: tokenValidatorFunc(func(context.Context, string) (auth.User, error) {
						return auth.User{ID: "7", UserID: "7", AuthType: "user"}, nil
					}),
					PrincipalValidator: validator,
				})(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
			},
			request: func() *http.Request {
				request := httptest.NewRequest(http.MethodGet, "/api/v2/projects", nil)
				request.Header.Set("X-API-Key", "valid-api-key")
				return request
			},
		},
		{
			name:   "form session",
			source: "form_session_cookie",
			build: func(_ *testing.T, validator middleware.PrincipalValidator) http.Handler {
				return middleware.Auth(middleware.AuthConfig{
					FormSessions:       authtest.FormSessions{},
					PrincipalValidator: validator,
				})(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
			},
			request: func() *http.Request {
				request := httptest.NewRequest(http.MethodGet, "/api/v2/projects", nil)
				authtest.SignIn(request, "7")
				return request
			},
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			recorded := captureLog(t)
			handler := testCase.build(t, unreachablePrincipalStore(t))
			recorder := httptest.NewRecorder()
			handler.ServeHTTP(recorder, testCase.request())

			if recorder.Code != http.StatusServiceUnavailable {
				t.Fatalf("status = %d, want %d", recorder.Code, http.StatusServiceUnavailable)
			}
			assertRefusalLogged(t, recorded, "principal_store_unavailable", testCase.source)
		})
	}
}

func assertRefusalLogged(t *testing.T, recorded *bytes.Buffer, reason, source string) {
	t.Helper()
	for _, line := range strings.Split(strings.TrimSpace(recorded.String()), "\n") {
		if line == "" {
			continue
		}
		var entry map[string]any
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			continue
		}
		if entry["reason"] == reason && entry["source"] == source {
			return
		}
	}
	t.Fatalf("no log line names reason %q on source %q; the log holds: %s",
		reason, source, recorded.String())
}
