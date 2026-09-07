package api_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api"
	apimw "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/middleware"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth"
)

// testPrincipalValidator passes the authenticated principal through.
//
// This file is in the EXTERNAL test package, so it cannot see the identical
// double in artifact_rbac_test_doubles_test.go. Duplicated rather than
// exported from production code: a pass-through principal validator is a test
// affordance, and putting one where non-test code can reach it is how it ends
// up composed by accident.
//
// It is needed because validatePrincipal fails closed — a router with a token
// validator and no principal validator now refuses every request.
type testPrincipalValidator struct{}

func (testPrincipalValidator) ValidatePrincipal(_ context.Context, user auth.User) (auth.User, error) {
	return user, nil
}

func buildGatewayRouterConfig(t *testing.T, validator apimw.TokenValidator, resolver apimw.PersonalProjectResolver, proxy http.Handler) api.RouterConfig {
	t.Helper()
	return api.RouterConfig{
		AuthValidator:          validator,
		PrincipalValidator:     testPrincipalValidator{},
		GatewayProxy:           proxy,
		GatewayProjectResolver: resolver,
	}
}

// Issue #463 renamed this from TestGatewayProxy_NotMountedWhenNil and changed
// the expected status from 404 to 503.
//
// The chart ships LLM_GATEWAY_URL empty, so a nil GatewayProxy was the DEFAULT
// state of a Kubernetes install, and 404 is the same answer a misspelt path
// gets. An operator could not tell an unconfigured deployment from a typo. The
// path is now registered with a handler that says which variable is missing.
func TestGatewayProxy_SaysNotConfiguredWhenNil(t *testing.T) {
	cfg := buildGatewayRouterConfig(t, nil, nil, nil)
	r := api.NewRouter(cfg)

	req := httptest.NewRequest(http.MethodGet, "/llm/v1/models", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 when GatewayProxy is nil, got %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), api.LLMNotConfiguredCode) {
		t.Fatalf("expected the body to carry %q, got %s", api.LLMNotConfiguredCode, rec.Body.String())
	}
}

func TestGatewayProxy_UnauthenticatedReturns401(t *testing.T) {
	proxy := &recordingHandler{}
	cfg := buildGatewayRouterConfig(t, nil, nil, proxy)
	r := api.NewRouter(cfg)

	req := httptest.NewRequest(http.MethodPost, "/llm/v1/chat/completions", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 for unauthenticated request, got %d", rec.Code)
	}
	if proxy.reached {
		t.Error("proxy must not be reached for unauthenticated requests")
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json (spec §2.5)", ct)
	}
	var body struct {
		Error struct {
			Message string `json:"message"`
			Type    string `json:"type"`
			Code    string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("response body is not valid JSON: %v (body: %s)", err, rec.Body.String())
	}
	if body.Error.Message == "" || body.Error.Type == "" || body.Error.Code == "" {
		t.Errorf("expected nested error{message,type,code} all populated, got %+v", body.Error)
	}
}

// TestGatewayProxy_AuthenticatedWithProjectContext mirrors
// TestLLMRoute_SystemProjectUserNameIsNotAnUncheckedClaim for the GatewayProxy
// mount. The gateway mount composes the same membership checker from
// RouterConfig.Pool, and this configuration has no pool, so a project the
// principal name asks for is never admitted (issue #459).
func TestGatewayProxy_AuthenticatedWithProjectContext(t *testing.T) {
	systemUser := auth.User{
		ID:       "100",
		UserID:   "100",
		Name:     ":system:project:42:",
		AuthType: "token",
	}
	validator := &stubTokenValidator{user: systemUser}
	resolver := &stubProjectResolver{id: 999}
	proxy := &recordingHandler{}

	cfg := buildGatewayRouterConfig(t, validator, resolver, proxy)
	r := api.NewRouter(cfg)

	req := httptest.NewRequest(http.MethodPost, "/llm/v1/chat/completions", nil)
	req.Header.Set("Authorization", "Bearer test-token")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if proxy.project.ProjectID == 42 {
		t.Fatal("the proxy received project 42, which only the principal name asked for")
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d (body: %s)", rec.Code, rec.Body.String())
	}
	if !proxy.reached {
		t.Fatal("proxy was not reached for authenticated /llm request")
	}
	if proxy.project.ProjectID != 999 {
		t.Errorf("expected the caller's own project 999, got %d", proxy.project.ProjectID)
	}
	if !resolver.called {
		t.Error("personal project resolver must run after a refused principal name")
	}
}

// TestGatewayProxy_PersonalProjectFallback verifies that for a regular
// (non-system) user the Project middleware consults the
// DBPersonalProjectResolver and injects the returned id, mirroring
// TestLLMRoute_PersonalProjectFallback for the GatewayProxy mount.
func TestGatewayProxy_PersonalProjectFallback(t *testing.T) {
	regularUser := auth.User{
		ID:       "55",
		Name:     "alice@example.com",
		AuthType: "token",
	}
	validator := &stubTokenValidator{user: regularUser}
	resolver := &stubProjectResolver{id: 77}
	proxy := &recordingHandler{}

	cfg := buildGatewayRouterConfig(t, validator, resolver, proxy)
	r := api.NewRouter(cfg)

	req := httptest.NewRequest(http.MethodPost, "/llm/v1/chat/completions", nil)
	req.Header.Set("Authorization", "Bearer test-regular-token")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d (body: %s)", rec.Code, rec.Body.String())
	}
	if !proxy.reached {
		t.Fatal("proxy was not reached")
	}
	if !resolver.called {
		t.Error("personal project resolver should be called for non-system user")
	}
	if proxy.project.ProjectID != 77 {
		t.Errorf("expected ProjectID 77 in proxy context, got %d", proxy.project.ProjectID)
	}
}

func TestGatewayProxy_SubpathsReachProxy(t *testing.T) {
	// A regular user with a resolved personal project. The principal name is
	// not a project source here (issue #459), so this test must not depend on
	// one.
	user := auth.User{ID: "1", UserID: "1", Name: "alice@example.com", AuthType: "token"}
	validator := &stubTokenValidator{user: user}
	proxy := &recordingHandler{}

	cfg := buildGatewayRouterConfig(t, validator, &stubProjectResolver{id: 5}, proxy)
	r := api.NewRouter(cfg)

	for _, path := range []string{"/llm/v1/models", "/llm/v1/chat/completions", "/llm/v1/embeddings", "/llm/v1/messages"} {
		proxy.reached = false
		req := httptest.NewRequest(http.MethodPost, path, nil)
		req.Header.Set("Authorization", "Bearer tok")
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)

		if !proxy.reached {
			t.Errorf("proxy not reached for path %s (status %d)", path, rec.Code)
		}
	}
}
