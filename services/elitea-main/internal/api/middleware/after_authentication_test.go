package middleware_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	apimw "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/middleware"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth"
)

type afterAuthValidator struct{}

func (afterAuthValidator) ValidateToken(_ context.Context, token string) (auth.User, error) {
	if token != "good" {
		return auth.User{}, errors.New("rejected")
	}
	return auth.User{ID: "5", UserID: "5", AuthType: "token"}, nil
}

type afterAuthPrincipals struct{}

func (afterAuthPrincipals) ValidatePrincipal(_ context.Context, user auth.User) (auth.User, error) {
	return user, nil
}

// recordingGate records what it saw: whether a principal was on the context
// when it ran, and how many times it ran.
type recordingGate struct {
	calls        int
	sawPrincipal bool
}

func (g *recordingGate) middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		g.calls++
		_, g.sawPrincipal = auth.UserFromContext(r.Context())
		next.ServeHTTP(w, r)
	})
}

func afterAuthRequest(token string) *http.Request {
	request := httptest.NewRequest(http.MethodGet, "/api/v2/x", nil)
	if token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	return request
}

var afterAuthConfig = apimw.AuthConfig{Validator: afterAuthValidator{}, PrincipalValidator: afterAuthPrincipals{}}

// The gates run between the handler's own Auth and the handler, with the
// principal on the context — the position they hold in the /api/v2 group.
func TestAfterAuthenticationRunsTheGatesBehindTheHandlersOwnAuth(t *testing.T) {
	gate := &recordingGate{}
	reached := false
	handler := apimw.AfterAuthentication(gate.middleware)(apimw.Auth(afterAuthConfig)(
		http.HandlerFunc(func(http.ResponseWriter, *http.Request) { reached = true })))

	handler.ServeHTTP(httptest.NewRecorder(), afterAuthRequest("good"))

	if gate.calls != 1 || !gate.sawPrincipal {
		t.Fatalf("gate calls=%d sawPrincipal=%v, want one call with the principal on the context", gate.calls, gate.sawPrincipal)
	}
	if !reached {
		t.Fatal("the handler was not reached through a gate that passes")
	}
}

// A refusing gate stops the request before the handler, as in the group.
func TestAfterAuthenticationGateRefusalStopsTheHandler(t *testing.T) {
	refuse := func(http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusServiceUnavailable)
		})
	}
	reached := false
	handler := apimw.AfterAuthentication(refuse)(apimw.Auth(afterAuthConfig)(
		http.HandlerFunc(func(http.ResponseWriter, *http.Request) { reached = true })))

	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, afterAuthRequest("good"))

	if recorder.Code != http.StatusServiceUnavailable || reached {
		t.Fatalf("status %d reached=%v, want 503 and the handler untouched", recorder.Code, reached)
	}
}

// Authentication is unchanged: a refused credential is answered 401 by Auth,
// and no gate runs — the group's order, where Auth refuses before Maintenance.
func TestAfterAuthenticationLeavesARefusedCredentialToAuth(t *testing.T) {
	for _, token := range []string{"", "bad"} {
		gate := &recordingGate{}
		handler := apimw.AfterAuthentication(gate.middleware)(apimw.Auth(afterAuthConfig)(
			http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})))

		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, afterAuthRequest(token))

		if recorder.Code != http.StatusUnauthorized {
			t.Errorf("token %q: status %d, want 401", token, recorder.Code)
		}
		if gate.calls != 0 {
			t.Errorf("token %q: a gate ran %d time(s) for a refused credential", token, gate.calls)
		}
	}
}

// A handler that nests a second Auth does not run the gates twice: the first
// admission consumes them.
func TestAfterAuthenticationGatesRunOnceUnderNestedAuth(t *testing.T) {
	gate := &recordingGate{}
	inner := apimw.Auth(afterAuthConfig)(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	handler := apimw.AfterAuthentication(gate.middleware)(apimw.Auth(afterAuthConfig)(inner))

	handler.ServeHTTP(httptest.NewRecorder(), afterAuthRequest("good"))

	if gate.calls != 1 {
		t.Fatalf("gate ran %d times under two nested Auth layers, want 1", gate.calls)
	}
}

// Without AfterAuthentication, Auth behaves exactly as before.
func TestAuthWithoutScheduledGatesIsUnchanged(t *testing.T) {
	reached := false
	handler := apimw.Auth(afterAuthConfig)(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { reached = true }))
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, afterAuthRequest("good"))
	if !reached || recorder.Code != http.StatusOK {
		t.Fatalf("reached=%v status=%d", reached, recorder.Code)
	}
}
