package api

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	apimw "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/middleware"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth/browsersession"
)

// The chat surface reads the execution-events stream with an EventSource, which
// can send a cookie and nothing else — no bearer, no forwarded identity. With
// the routes composed for forwarded identity alone, the product's own UI could
// not read a stream every server-side hop could (#93/#289).
//
// Accepting a session must not widen what a caller may see: these routes still
// require a runtime principal, and the handler behind them authorizes per
// request against the execution's project and capability.
func TestProductionRuntimeRoutesAcceptBrowserSession(t *testing.T) {
	t.Parallel()

	const secret = "production-runtime-session-secret"
	served := false
	handler := http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		// The principal must arrive as a RUNTIME principal, not merely an
		// authenticated user: requireRuntimePrincipal is what the route's
		// downstream authorizer depends on.
		if _, ok := auth.RuntimePrincipalFromContext(request.Context()); !ok {
			t.Error("session-authenticated caller did not become a runtime principal")
		}
		served = true
		writer.WriteHeader(http.StatusNoContent)
	})
	principal := productionRuntimePrincipalValidatorFunc(func(_ context.Context, user auth.User) (auth.User, error) {
		user.Email = "member@example.test"
		return user, nil
	})
	peer := productionRuntimePeerVerifierFunc(func(*http.Request) error {
		return nil
	})

	t.Run("session cookie is accepted when a secret is configured", func(t *testing.T) {
		routes, err := NewProductionRuntimeRoutes(handler, handler, principal, peer, apimw.AuthConfig{SessionSecret: secret})
		if err != nil {
			t.Fatal(err)
		}
		router := NewRouter(RouterConfig{ProductionRuntime: routes})

		request := httptest.NewRequest(http.MethodGet, "/api/v2/executions/42/execution-1/events", nil)
		request.AddCookie(&http.Cookie{Name: "elitea_session", Value: sessionToken(secret, "7")})
		response := httptest.NewRecorder()
		router.ServeHTTP(response, request)

		if response.Code != http.StatusNoContent {
			t.Fatalf("status = %d, want %d — the browser cannot read its own stream", response.Code, http.StatusNoContent)
		}
		if !served {
			t.Fatal("handler never ran")
		}
	})

	t.Run("an edge-only deployment is unchanged", func(t *testing.T) {
		// Passing "" must behave exactly as before this parameter existed:
		// no cookie is read, so the same request is rejected.
		routes, err := NewProductionRuntimeRoutes(handler, handler, principal, peer, apimw.AuthConfig{})
		if err != nil {
			t.Fatal(err)
		}
		router := NewRouter(RouterConfig{ProductionRuntime: routes})

		request := httptest.NewRequest(http.MethodGet, "/api/v2/executions/42/execution-1/events", nil)
		request.AddCookie(&http.Cookie{Name: "elitea_session", Value: sessionToken(secret, "7")})
		response := httptest.NewRecorder()
		router.ServeHTTP(response, request)

		if response.Code == http.StatusNoContent {
			t.Fatal("a cookie was honoured with no session secret configured")
		}
	})

	t.Run("a forged session is rejected", func(t *testing.T) {
		routes, err := NewProductionRuntimeRoutes(handler, handler, principal, peer, apimw.AuthConfig{SessionSecret: secret})
		if err != nil {
			t.Fatal(err)
		}
		router := NewRouter(RouterConfig{ProductionRuntime: routes})

		request := httptest.NewRequest(http.MethodGet, "/api/v2/executions/42/execution-1/events", nil)
		request.AddCookie(&http.Cookie{Name: "elitea_session", Value: sessionToken("a-different-secret", "7")})
		response := httptest.NewRecorder()
		router.ServeHTTP(response, request)

		if response.Code == http.StatusNoContent {
			t.Fatal("a session signed with the wrong secret was accepted")
		}
	})
}

// sessionToken mirrors v2auth.makeSessionToken: base64url(JSON payload) + "." +
// hex(HMAC-SHA256). Reproduced rather than imported because that helper is
// unexported and this package must not depend on the OIDC handler to state what
// a browser presents.
func sessionToken(secret, userID string) string {
	payload, _ := json.Marshal(map[string]any{
		"uid":   userID,
		"email": "member@example.test",
		"exp":   time.Now().Add(time.Hour).Unix(),
	})
	encoded := base64.RawURLEncoding.EncodeToString(payload)
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(encoded))
	return encoded + "." + hex.EncodeToString(mac.Sum(nil))
}

// TestProductionRuntimeRoutesAcceptAServerSideSession is the regression test
// for the defect this file's construction comment describes.
//
// WHAT BROKE. migrations/shared/0117 turned the browser session into a
// server-side row and changed the `elitea_session` VALUE from a signed JSON
// blob to an opaque `s1.` identifier. apimw.Auth reads such a value through
// AuthConfig.SessionStore. internal/api/router.go copied the new field into
// each of its own AuthConfigs; this route pair composed a SEPARATE AuthConfig
// and did not, so the identifier fell through to the legacy HMAC reader, failed
// its signature check, and the route answered 401.
//
// WHY IT MATTERED HERE MORE THAN ANYWHERE ELSE. This is the execution-events
// stream, and an EventSource can present a cookie and nothing else. So the one
// route with no second credential to fall back on was the one that lost the
// only credential it had. Every index and chat journey then admitted its run
// with 200 and never received a terminal event, which reads on screen as a run
// that hangs forever rather than as an authentication failure.
//
// WHY THE EXISTING TESTS ALL PASSED. TestProductionRuntimeRoutesAcceptBrowserSession
// above presents a LEGACY cookie, which this path still reads; the router's own
// TestEveryAuthenticatedGroupCarriesTheSessionStore walks NewRouter's groups,
// and these two routes are registered outside every one of them
// (production_router.go, `cfg.ProductionRuntime`). Both halves were right and
// nothing joined them.
func TestProductionRuntimeRoutesAcceptAServerSideSession(t *testing.T) {
	store := &routeTestSessions{rows: map[string]browsersession.Session{}}
	manager, err := browsersession.NewManager(store, browsersession.Policy{
		IdleTimeout: time.Hour, AbsoluteLifetime: 24 * time.Hour,
	})
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}

	served := 0
	handler := http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if _, ok := auth.RuntimePrincipalFromContext(request.Context()); !ok {
			t.Error("a session-authenticated caller did not become a runtime principal")
		}
		served++
		writer.WriteHeader(http.StatusNoContent)
	})
	principal := productionRuntimePrincipalValidatorFunc(func(_ context.Context, user auth.User) (auth.User, error) {
		return user, nil
	})
	peer := productionRuntimePeerVerifierFunc(func(*http.Request) error { return nil })

	// The value the composition root hands over: the /api/v2 group's own
	// AuthConfig, carrying BOTH cookie readers.
	routes, err := NewProductionRuntimeRoutes(handler, handler, principal, peer, apimw.AuthConfig{
		SessionSecret: "production-runtime-session-secret",
		SessionStore:  manager,
	})
	if err != nil {
		t.Fatal(err)
	}
	router := NewRouter(RouterConfig{ProductionRuntime: routes})

	value, err := manager.Create(context.Background(), browsersession.NewSession{
		UserID: 7, Email: "member@example.test", Provider: browsersession.ProviderOIDC,
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	const eventsPath = "/api/v2/executions/42/execution-1/events"
	if status := serveWithSessionCookie(t, router, eventsPath, value); status != http.StatusNoContent {
		t.Fatalf("status = %d, want %d — the events route refused a live server-side "+
			"session, so its AuthConfig does not carry SessionStore. An EventSource has "+
			"no other credential, so the browser can never read the stream it opened.",
			status, http.StatusNoContent)
	}
	if served != 1 {
		t.Fatalf("handler ran %d times, want 1", served)
	}

	// The other half: a revoked session must stop working here too, or the
	// route would be reading a credential nobody can withdraw.
	if err := manager.Revoke(context.Background(), value); err != nil {
		t.Fatalf("Revoke: %v", err)
	}
	if status := serveWithSessionCookie(t, router, eventsPath, value); status != http.StatusUnauthorized {
		t.Fatalf("status after revocation = %d, want 401 — a logout the events stream "+
			"does not honour is not a logout", status)
	}
	if served != 1 {
		t.Fatalf("handler ran %d times after revocation, want 1", served)
	}
}
