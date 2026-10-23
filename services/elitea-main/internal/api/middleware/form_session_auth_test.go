package middleware_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/middleware"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/middleware/authtest"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/auth"
)

// The Form session cookie is read in-process (#1359). It replaced the X-Auth-*
// projection an authenticating edge used to add, so a Form-login deployment
// authenticates with no edge in front of elitea-main at all.

func activePrincipals() middleware.PrincipalValidator {
	return principalValidatorFunc(func(_ context.Context, user auth.User) (auth.User, error) {
		user.Email = "member@example.test"
		return user, nil
	})
}

// formSessionRoute is apimw.Auth over a handler that records the principal and
// the source it was authenticated by.
func formSessionRoute(cfg middleware.AuthConfig, seen *auth.User, source *auth.AuthenticationSource) http.Handler {
	return middleware.Auth(cfg)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*seen, _ = auth.UserFromContext(r.Context())
		*source, _ = auth.AuthenticationSourceFromContext(r.Context())
		w.WriteHeader(http.StatusNoContent)
	}))
}

func TestAuthResolvesTheFormSessionAsASessionPrincipal(t *testing.T) {
	var seen auth.User
	var source auth.AuthenticationSource
	route := formSessionRoute(middleware.AuthConfig{
		FormSessions:       authtest.FormSessions{},
		PrincipalValidator: activePrincipals(),
	}, &seen, &source)

	request := httptest.NewRequest(http.MethodGet, "/api/v2/elitea_core/projects", nil)
	authtest.SignIn(request, "42")
	response := httptest.NewRecorder()
	route.ServeHTTP(response, request)

	if response.Code != http.StatusNoContent {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	if seen.UserID != "42" || seen.Email != "member@example.test" {
		t.Fatalf("principal = %+v, want the validated user 42", seen)
	}
	// A cookie authenticated it, so BrowserWriteOrigin must treat it as one.
	if source != auth.AuthenticationSourceSession {
		t.Fatalf("source = %d, want AuthenticationSourceSession", source)
	}
}

func TestAuthRefusesWhatTheFormSessionDoesNotProve(t *testing.T) {
	for _, test := range []struct {
		name     string
		cfg      middleware.AuthConfig
		prepare  func(*http.Request)
		want     int
		wantBody string
	}{
		{
			name: "X-Auth-* headers alone",
			cfg:  middleware.AuthConfig{FormSessions: authtest.FormSessions{}, PrincipalValidator: activePrincipals()},
			prepare: func(r *http.Request) {
				r.Header.Set("X-Auth-Type", "user")
				r.Header.Set("X-Auth-ID", "1")
				r.Header.Set("X-Auth-User-ID", "1")
			},
			want: http.StatusUnauthorized,
		},
		{
			name: "forged or expired Form cookie",
			cfg: middleware.AuthConfig{
				FormSessions:       authtest.FormSessions{Err: middleware.ErrFormSessionRejected},
				PrincipalValidator: activePrincipals(),
			},
			prepare:  func(r *http.Request) { authtest.SignIn(r, "42") },
			want:     http.StatusUnauthorized,
			wantBody: "form_session_rejected",
		},
		{
			name:     "malformed Form cookie value",
			cfg:      middleware.AuthConfig{FormSessions: authtest.FormSessions{}, PrincipalValidator: activePrincipals()},
			prepare:  func(r *http.Request) { authtest.SignIn(r, "forged") },
			want:     http.StatusUnauthorized,
			wantBody: "form_session_rejected",
		},
		{
			name: "Form cookie with no Form reader composed",
			cfg:  middleware.AuthConfig{PrincipalValidator: activePrincipals()},
			prepare: func(r *http.Request) {
				authtest.SignIn(r, "42")
			},
			want: http.StatusUnauthorized,
		},
		{
			name: "inactive principal behind a live Form session",
			cfg: middleware.AuthConfig{
				FormSessions: authtest.FormSessions{},
				PrincipalValidator: principalValidatorFunc(func(context.Context, auth.User) (auth.User, error) {
					return auth.User{}, auth.ErrPrincipalInactive
				}),
			},
			prepare: func(r *http.Request) { authtest.SignIn(r, "42") },
			want:    http.StatusUnauthorized,
		},
		{
			name: "Form session store outage",
			cfg: middleware.AuthConfig{
				FormSessions:       authtest.FormSessions{Err: errors.New("store down")},
				PrincipalValidator: activePrincipals(),
			},
			prepare:  func(r *http.Request) { authtest.SignIn(r, "42") },
			want:     http.StatusServiceUnavailable,
			wantBody: "session_store_unavailable",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			reached := false
			route := middleware.Auth(test.cfg)(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
				reached = true
			}))
			request := httptest.NewRequest(http.MethodGet, "/api/v2/elitea_core/projects", nil)
			test.prepare(request)
			response := httptest.NewRecorder()
			route.ServeHTTP(response, request)
			if response.Code != test.want || reached {
				t.Fatalf("status = %d (reached=%v), want %d; body = %s", response.Code, reached, test.want, response.Body.String())
			}
			if !strings.Contains(response.Body.String(), test.wantBody) {
				t.Fatalf("body = %s, want it to name %q", response.Body.String(), test.wantBody)
			}
		})
	}
}

// A bearer or API key is the caller's explicit choice of credential, so the
// Form cookie is not read at all, exactly as the X-Auth-* source used to be
// skipped.
func TestAuthPrefersAnExplicitTokenOverTheFormCookie(t *testing.T) {
	var seen auth.User
	var source auth.AuthenticationSource
	route := formSessionRoute(middleware.AuthConfig{
		FormSessions:       authtest.FormSessions{Err: errors.New("the Form store must not be read")},
		Validator:          authtest.Tokens{},
		PrincipalValidator: activePrincipals(),
	}, &seen, &source)

	request := httptest.NewRequest(http.MethodGet, "/api/v2/elitea_core/projects", nil)
	authtest.SignIn(request, "42")
	authtest.BearerToken(request, "900", "7")
	response := httptest.NewRecorder()
	route.ServeHTTP(response, request)

	if response.Code != http.StatusNoContent || seen.UserID != "7" || source != auth.AuthenticationSourceToken {
		t.Fatalf("status = %d principal = %+v source = %d; body = %s", response.Code, seen, source, response.Body.String())
	}
}

// A stale Form cookie must not lock out a browser that has since signed in
// through single sign-on: with an `elitea_session` cookie present the
// rejection falls through to it. Here that cookie is not readable (no secret),
// so the request is still refused — but by the session-cookie source, not by
// the Form one.
func TestAuthFallsThroughARejectedFormCookieWhenASessionCookieIsPresent(t *testing.T) {
	route := middleware.Auth(middleware.AuthConfig{
		FormSessions:       authtest.FormSessions{Err: middleware.ErrFormSessionRejected},
		PrincipalValidator: activePrincipals(),
	})(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Error("an unauthenticated request reached the handler")
	}))
	request := httptest.NewRequest(http.MethodGet, "/api/v2/elitea_core/projects", nil)
	authtest.SignIn(request, "42")
	request.AddCookie(&http.Cookie{Name: "elitea_session", Value: "s1.opaque"})
	response := httptest.NewRecorder()
	route.ServeHTTP(response, request)

	if response.Code != http.StatusUnauthorized || strings.Contains(response.Body.String(), "form_session_rejected") {
		t.Fatalf("status = %d body = %s; want a 401 from the session-cookie source", response.Code, response.Body.String())
	}
}

// The coordinator's CSRF requirement end to end: a Form-cookie write from
// another site is refused by the origin gate, and the same write from this
// origin is served.
func TestFormCookieWritesAreHeldToTheOriginGate(t *testing.T) {
	for _, test := range []struct {
		name    string
		headers map[string]string
		want    int
	}{
		{"cross-site", map[string]string{"Sec-Fetch-Site": "cross-site", "Origin": "https://evil.example.com"}, http.StatusForbidden},
		{"sibling site", map[string]string{"Sec-Fetch-Site": "same-site"}, http.StatusForbidden},
		{"this origin", map[string]string{"Sec-Fetch-Site": "same-origin", "Origin": "https://app.example.com"}, http.StatusNoContent},
	} {
		t.Run(test.name, func(t *testing.T) {
			route := middleware.Auth(middleware.AuthConfig{
				FormSessions:       authtest.FormSessions{},
				PrincipalValidator: activePrincipals(),
			})(middleware.BrowserWriteOrigin(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusNoContent)
			})))
			request := httptest.NewRequest(http.MethodPost, "https://app.example.com/api/v2/elitea_core/projects", nil)
			authtest.SignIn(request, "42")
			for name, value := range test.headers {
				request.Header.Set(name, value)
			}
			response := httptest.NewRecorder()
			route.ServeHTTP(response, request)
			if response.Code != test.want {
				t.Fatalf("status = %d, want %d; body = %s", response.Code, test.want, response.Body.String())
			}
		})
	}
}
