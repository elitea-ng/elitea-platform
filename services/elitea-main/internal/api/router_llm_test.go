package api_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"


	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api"
	apimw "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/middleware"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth"
)

// --- stubs -------------------------------------------------------------------

// stubTokenValidator satisfies apimw.TokenValidator, returning a fixed user.
type stubTokenValidator struct {
	user auth.User
}

func (s *stubTokenValidator) ValidateToken(_ context.Context, _ string) (auth.User, error) {
	return s.user, nil
}

// stubProjectResolver satisfies apimw.PersonalProjectResolver.
type stubProjectResolver struct {
	id     int
	called bool
}

func (s *stubProjectResolver) PersonalProjectID(_ context.Context, _ string) (int, error) {
	s.called = true
	return s.id, nil
}

// recordingHandler captures whether it was reached and what project context it
// saw, then writes 200 OK. Used as the stub LLMProxy.
type recordingHandler struct {
	reached bool
	project apimw.ProjectContext
}

func (h *recordingHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.reached = true
	if pc, ok := apimw.ProjectFromContext(r.Context()); ok {
		h.project = pc
	}
	w.WriteHeader(http.StatusOK)
}

// buildMinimalRouterConfig returns a RouterConfig with the minimum deps needed
// to exercise the /llm route (Auth + Project + LLMProxy). All other optional
// fields are left nil so we don't need a live DB, Redis, etc.
func buildMinimalRouterConfig(t *testing.T, validator apimw.TokenValidator, resolver apimw.PersonalProjectResolver, llmProxy http.Handler) api.RouterConfig {
	t.Helper()
	return api.RouterConfig{
		Auth: api.AuthDeps{
			Validator: validator,
			// validatePrincipal fails closed, so a router with a token
			// validator and no principal validator refuses every request.
			// These tests are about /llm ROUTING, not principal reload.
			PrincipalValidator: testPrincipalValidator{},
		},
		LLMProxy:           llmProxy,
		LLMProjectResolver: resolver,
	}
}

// --- tests -------------------------------------------------------------------

// TestLLMRoute_SaysNotConfiguredWhenProxyNil verifies that when no proxy is
// composed the /llm path answers 503 llm_gateway_not_configured (issue #463).
//
// This test previously required 404. That was the defect: the chart ships
// LLM_GATEWAY_URL empty, so 404 was the answer every Kubernetes install gave,
// and it is the same answer a misspelt path gives. The operator could not tell
// an unconfigured deployment from a typo.
//
// The body is asserted, not only the status. A status alone would still pass if
// some other layer began answering 503 for an unrelated reason.
func TestLLMRoute_SaysNotConfiguredWhenProxyNil(t *testing.T) {
	cfg := buildMinimalRouterConfig(t, nil, nil, nil)
	r := api.NewRouter(cfg)

	req := httptest.NewRequest(http.MethodGet, "/llm/v1/models", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 when no LLM backend is composed, got %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), api.LLMNotConfiguredCode) {
		t.Fatalf("expected the body to carry %q, got %s", api.LLMNotConfiguredCode, rec.Body.String())
	}
	// The remedy has to be in the answer. An operator reading this response
	// must learn which variable turns the path on.
	if !strings.Contains(rec.Body.String(), "LLM_GATEWAY_URL") {
		t.Fatalf("expected the body to name LLM_GATEWAY_URL, got %s", rec.Body.String())
	}
}

// TestLLMRoute_UnauthenticatedReturns401 verifies that /llm is behind Auth and
// rejects callers without credentials.
func TestLLMRoute_UnauthenticatedReturns401(t *testing.T) {
	proxy := &recordingHandler{}
	cfg := buildMinimalRouterConfig(t, nil, nil, proxy)
	r := api.NewRouter(cfg)

	req := httptest.NewRequest(http.MethodPost, "/llm/v1/chat/completions", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 for unauthenticated /llm request, got %d", rec.Code)
	}
	if proxy.reached {
		t.Error("proxy must not be reached for unauthenticated requests")
	}
}

// TestLLMRoute_SystemProjectUserNameIsNotAnUncheckedClaim is the router-level
// form of issue #459.
//
// The route composes the membership checker from RouterConfig.Pool
// (apimw.NewProjectMembership). This configuration has no pool, so no checker
// is composed, and no project the principal name asks for may be admitted. The
// caller's own project is the answer instead.
//
// Direction 2 — an entitled caller keeps its named project — needs an injected
// membership answer, which RouterConfig cannot carry. It is proved in
// internal/api/middleware (TestPrincipalName_MemberProjectIsBilled) and end to
// end in internal/llmproxy
// (TestEdge_PrincipalNameMemberProjectReachesTheGatewayIdentity).
func TestLLMRoute_SystemProjectUserNameIsNotAnUncheckedClaim(t *testing.T) {
	// System project-user name asks for project 42.
	systemUser := auth.User{
		ID:       "100",
		UserID:   "100",
		Name:     ":system:project:42:",
		AuthType: "token",
	}
	validator := &stubTokenValidator{user: systemUser}
	resolver := &stubProjectResolver{id: 999}
	proxy := &recordingHandler{}

	cfg := buildMinimalRouterConfig(t, validator, resolver, proxy)
	r := api.NewRouter(cfg)

	req := httptest.NewRequest(http.MethodPost, "/llm/v1/chat/completions", nil)
	req.Header.Set("Authorization", "Bearer test-system-token")
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
	if !resolver.called {
		t.Error("personal project resolver must run after a refused principal name")
	}
	if proxy.project.ProjectID != 999 {
		t.Errorf("expected the caller's own project 999, got %d", proxy.project.ProjectID)
	}
}

// TestLLMRoute_PersonalProjectFallback verifies that for a regular (non-system)
// user the Project middleware consults the resolver and injects the returned id.
func TestLLMRoute_PersonalProjectFallback(t *testing.T) {
	regularUser := auth.User{
		ID:       "55",
		Name:     "alice@example.com",
		AuthType: "token",
	}
	validator := &stubTokenValidator{user: regularUser}
	resolver := &stubProjectResolver{id: 77}
	proxy := &recordingHandler{}

	cfg := buildMinimalRouterConfig(t, validator, resolver, proxy)
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

// TestLLMRoute_SubpathPreserved verifies that a request to /llm/v1/models
// reaches the proxy (chi Mount strips the /llm prefix; we just check the proxy
// is reached regardless of sub-path).
func TestLLMRoute_SubpathPreserved(t *testing.T) {
	// A regular user with a resolved personal project. The principal name is
	// not a project source here (issue #459), so this test must not depend on
	// one.
	user := auth.User{ID: "1", UserID: "1", Name: "alice@example.com", AuthType: "token"}
	validator := &stubTokenValidator{user: user}
	proxy := &recordingHandler{}

	cfg := buildMinimalRouterConfig(t, validator, &stubProjectResolver{id: 5}, proxy)
	r := api.NewRouter(cfg)

	for _, path := range []string{"/llm/v1/models", "/llm/v1/chat/completions", "/llm/v1/embeddings"} {
		proxy.reached = false
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Header.Set("Authorization", "Bearer tok")
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)

		if !proxy.reached {
			t.Errorf("proxy not reached for path %s (status %d)", path, rec.Code)
		}
	}
}

// TestForwardedHeadersAloneAreRefusedThroughTheRouter is the router-level guard
// for #390. It asserts the contract through NewRouter, so it covers every Auth
// middleware construction, not the middleware in isolation.
//
// This test replaces TestTrustedProxyCIDRs_ThreadedThroughAuthDeps, which
// asserted the opposite: that X-Auth-Type / X-Auth-Id from a trusted CIDR
// authenticate a caller with no Bearer token. That path is deleted. It called
// serveAuthenticated directly, so no principal check ever ran on it, and the
// headers were the whole credential.
func TestForwardedHeadersAloneAreRefusedThroughTheRouter(t *testing.T) {
	// Name the range that used to grant access, through the switch that used
	// to enable it. Neither may authenticate the caller now.
	t.Setenv("TRUSTED_PROXY_CIDRS", "127.0.0.0/8")

	proxy := &recordingHandler{}
	cfg := api.RouterConfig{
		// No credential reader at all: the forwarded headers below are the
		// only thing the caller presents, and they must not authenticate it.
		LLMProxy:           proxy,
		LLMProjectResolver: &stubProjectResolver{id: 10},
	}
	r := api.NewRouter(cfg)

	for _, path := range []string{"/llm/v1/chat/completions", "/api/v2/projects"} {
		req := httptest.NewRequest(http.MethodPost, path, nil)
		req.RemoteAddr = "127.0.0.1:12345"
		req.Header.Set("X-Auth-Type", "user")
		req.Header.Set("X-Auth-Id", "1")
		rec := httptest.NewRecorder()

		r.ServeHTTP(rec, req)

		if rec.Code != http.StatusUnauthorized {
			t.Errorf("%s: status = %d, want %d", path, rec.Code, http.StatusUnauthorized)
		}
	}
	if proxy.reached {
		t.Error("forwarded headers alone reached the LLM proxy")
	}
}
