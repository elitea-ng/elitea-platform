package browserauth

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	apimw "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/middleware"
	browserapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/browserauth"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/auth"
)

type formSessionAuthorizerFunc func(context.Context, string) (browserapp.Authorization, error)

func (f formSessionAuthorizerFunc) Authorize(ctx context.Context, sessionID string) (browserapp.Authorization, error) {
	return f(ctx, sessionID)
}

func newTestFormSessions(t *testing.T, authorize formSessionAuthorizerFunc) *FormSessions {
	t.Helper()
	cookies, err := NewCookiePolicy(CookieConfig{
		Name:     "elitea_browser_auth",
		Secure:   true,
		SameSite: http.SameSiteLaxMode,
		Lifetime: time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	sessions, err := NewFormSessions(cookies, authorize)
	if err != nil {
		t.Fatal(err)
	}
	return sessions
}

func formSessionRequest(values ...string) *http.Request {
	request := httptest.NewRequest(http.MethodGet, "/api/v2/elitea_core/projects", nil)
	for _, value := range values {
		request.AddCookie(&http.Cookie{Name: "elitea_browser_auth", Value: value})
	}
	return request
}

func sessionAuthorization(userID string) browserapp.Authorization {
	return browserapp.Authorization{Principal: auth.User{ID: userID, UserID: userID, AuthType: "session"}}
}

func TestFormSessionsResolveALiveSession(t *testing.T) {
	sessionID := canonicalSessionID(3)
	sessions := newTestFormSessions(t, func(_ context.Context, got string) (browserapp.Authorization, error) {
		if got != sessionID {
			t.Errorf("session id = %q, want %q", got, sessionID)
		}
		return sessionAuthorization("71"), nil
	})

	principal, err := sessions.AuthenticateFormSession(formSessionRequest(CookieValuePrefix + sessionID))
	if err != nil || principal.UserID != "71" || principal.ID != "71" || principal.AuthType != "session" {
		t.Fatalf("principal = %+v, %v", principal, err)
	}
}

func TestFormSessionsClassifyEveryRefusal(t *testing.T) {
	valid := CookieValuePrefix + canonicalSessionID(4)
	storeDown := errors.New("session store unavailable")
	for _, test := range []struct {
		name      string
		cookies   []string
		authorize formSessionAuthorizerFunc
		want      error
	}{
		{name: "no cookie", want: apimw.ErrFormSessionAbsent},
		{name: "forged value", cookies: []string{"v1.forged"}, want: apimw.ErrFormSessionRejected},
		{name: "legacy Flask value", cookies: []string{canonicalSessionID(4)}, want: apimw.ErrFormSessionRejected},
		{name: "duplicate cookie", cookies: []string{valid, valid}, want: apimw.ErrFormSessionRejected},
		{name: "unknown session", cookies: []string{valid}, want: apimw.ErrFormSessionRejected,
			authorize: func(context.Context, string) (browserapp.Authorization, error) {
				return browserapp.Authorization{}, browserapp.ErrUnauthenticated
			}},
		{name: "expired session", cookies: []string{valid}, want: apimw.ErrFormSessionRejected,
			authorize: func(context.Context, string) (browserapp.Authorization, error) {
				return browserapp.Authorization{}, browserapp.ErrAuthenticationExpired
			}},
		{name: "invalid request", cookies: []string{valid}, want: apimw.ErrFormSessionRejected,
			authorize: func(context.Context, string) (browserapp.Authorization, error) {
				return browserapp.Authorization{}, browserapp.ErrInvalidRequest
			}},
		{name: "token principal", cookies: []string{valid}, want: apimw.ErrFormSessionRejected,
			authorize: func(context.Context, string) (browserapp.Authorization, error) {
				return browserapp.Authorization{Principal: auth.User{
					ID: "900", TokenID: "900", UserID: "71", AuthType: "token",
				}}, nil
			}},
		{name: "mismatched ids", cookies: []string{valid}, want: apimw.ErrFormSessionRejected,
			authorize: func(context.Context, string) (browserapp.Authorization, error) {
				return browserapp.Authorization{Principal: auth.User{ID: "1", UserID: "71", AuthType: "session"}}, nil
			}},
		{name: "non-positive user", cookies: []string{valid}, want: apimw.ErrFormSessionRejected,
			authorize: func(context.Context, string) (browserapp.Authorization, error) {
				return sessionAuthorization("0"), nil
			}},
		{name: "store outage stays an outage", cookies: []string{valid}, want: storeDown,
			authorize: func(context.Context, string) (browserapp.Authorization, error) {
				return browserapp.Authorization{}, storeDown
			}},
	} {
		t.Run(test.name, func(t *testing.T) {
			authorize := test.authorize
			if authorize == nil {
				authorize = func(context.Context, string) (browserapp.Authorization, error) {
					t.Error("the session store must not be read for an unreadable cookie")
					return sessionAuthorization("1"), nil
				}
			}
			sessions := newTestFormSessions(t, authorize)
			if _, err := sessions.AuthenticateFormSession(formSessionRequest(test.cookies...)); !errors.Is(err, test.want) {
				t.Fatalf("error = %v, want %v", err, test.want)
			}
		})
	}
}

func TestNewFormSessionsRefusesAnIncompleteComposition(t *testing.T) {
	cookies, err := NewCookiePolicy(CookieConfig{
		Name: "elitea_browser_auth", Secure: true, SameSite: http.SameSiteLaxMode, Lifetime: time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	authorize := formSessionAuthorizerFunc(func(context.Context, string) (browserapp.Authorization, error) {
		return browserapp.Authorization{}, nil
	})
	if _, err := NewFormSessions(nil, authorize); !errors.Is(err, ErrInvalidHandlerConfiguration) {
		t.Fatalf("nil cookies error = %v", err)
	}
	if _, err := NewFormSessions(cookies, nil); !errors.Is(err, ErrInvalidHandlerConfiguration) {
		t.Fatalf("nil sessions error = %v", err)
	}
}
