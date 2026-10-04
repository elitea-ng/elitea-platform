package middleware

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth"
)

func signInChain(authenticate func(http.Handler) http.Handler) http.Handler {
	return BrowserSignInRequired(
		authenticate,
		func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, "/auth/login", http.StatusFound) },
		func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusBadRequest) },
	)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusTeapot) }))
}

// Every 401 the Auth middleware can answer becomes the sign-in redirect.
func TestBrowserSignInRequiredBouncesEveryCredentialRefusal(t *testing.T) {
	for _, reason := range []string{
		reasonNoCredential, reasonSessionMalformed, reasonSessionBadSignature, reasonSessionExpired,
		reasonSessionSubject, reasonServerSessionUnknown, reasonServerSessionRevoked, reasonServerSessionExpired,
		reasonServerSessionIdle, reasonLegacySessionRejected, reasonSessionSecretAbsent,
	} {
		authenticate := func(http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				writeCredentialRefusal(w, r, sourceSession, reason)
			})
		}
		recorder := httptest.NewRecorder()
		signInChain(authenticate).ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/x", nil))
		if recorder.Code != http.StatusFound || recorder.Header().Get("Location") != "/auth/login" {
			t.Fatalf("%s: %d %q, want the sign-in redirect", reason, recorder.Code, recorder.Header().Get("Location"))
		}
	}
}

// An outage is not a reason to bounce a person to sign-in.
func TestBrowserSignInRequiredPassesA503Through(t *testing.T) {
	authenticate := func(http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Retry-After", "5")
			writeJSONError(w, http.StatusServiceUnavailable, "server_error", "session_store_unavailable", "x")
		})
	}
	recorder := httptest.NewRecorder()
	signInChain(authenticate).ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/x", nil))
	if recorder.Code != http.StatusServiceUnavailable || recorder.Header().Get("Retry-After") != "5" {
		t.Fatalf("503 = %d %v", recorder.Code, recorder.Header())
	}
}

func TestBrowserSignInRequiredRefusesTokens(t *testing.T) {
	authenticateAs := func(user auth.User, source auth.AuthenticationSource) func(http.Handler) http.Handler {
		return func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				next.ServeHTTP(w, r.WithContext(auth.ContextWithAuthenticatedUser(r.Context(), user, source)))
			})
		}
	}
	session := auth.User{ID: "7", UserID: "7", AuthType: "session"}
	for name, c := range map[string]struct {
		header, value string
		authenticate  func(http.Handler) http.Handler
		want          int
	}{
		"bearer header":   {"Authorization", "Bearer elnat_x", authenticateAs(session, auth.AuthenticationSourceSession), http.StatusBadRequest},
		"api key header":  {"X-API-Key", "k", authenticateAs(session, auth.AuthenticationSourceSession), http.StatusBadRequest},
		"forwarded token": {"", "", authenticateAs(auth.User{ID: "7", UserID: "7", TokenID: "9", AuthType: "token"}, auth.AuthenticationSourceForwarded), http.StatusBadRequest},
		"token source":    {"", "", authenticateAs(auth.User{ID: "7", UserID: "7", TokenID: "9", AuthType: "token"}, auth.AuthenticationSourceToken), http.StatusBadRequest},
		"session":         {"", "", authenticateAs(session, auth.AuthenticationSourceSession), http.StatusTeapot},
		"forwarded user":  {"", "", authenticateAs(auth.User{ID: "7", UserID: "7", AuthType: "user"}, auth.AuthenticationSourceForwarded), http.StatusTeapot},
	} {
		request := httptest.NewRequest(http.MethodGet, "/x", nil)
		if c.header != "" {
			request.Header.Set(c.header, c.value)
		}
		recorder := httptest.NewRecorder()
		signInChain(c.authenticate).ServeHTTP(recorder, request)
		if recorder.Code != c.want {
			t.Fatalf("%s = %d, want %d", name, recorder.Code, c.want)
		}
	}
}
