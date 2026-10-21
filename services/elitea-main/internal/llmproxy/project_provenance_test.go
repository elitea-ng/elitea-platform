package llmproxy

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/middleware"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/llmproxy/llmproxytest"
)

// This file proves two rules of the /llm edge over the real chain: the project
// middleware in front of the streaming proxy, with a backend that records what
// the gateway received.
//
// Rule 1, issue #461. The edge refuses an identity that no accepted
// authentication path recorded. The gateway is never called.
//
// Rule 2, issue #459. The edge refuses a project that the principal name asks
// for when the caller is not a member of it. The gateway is called, and the
// signed X-Elitea-Project-Id names the caller's own project.
//
// The signed header is the assertion that matters. The gateway bills strictly
// on it (elitea-llm-gateway/internal/llmproxy/budget_gate.go), and it also
// selects the provider credentials that are decrypted and spent.

// withEdgeProvenance records the provenance the Auth middleware records for a
// bearer token, and returns the request. Every edge test uses it, because the
// edge refuses an identity with no recorded provenance.
func withEdgeProvenance(req *http.Request, user auth.User) *http.Request {
	return req.WithContext(
		auth.ContextWithAuthenticatedUser(req.Context(), user, auth.AuthenticationSourceToken),
	)
}

// gatewaySpy is the backend, plus a record of whether the gateway was reached.
type gatewaySpy struct {
	called  bool
	headers http.Header
}

// provenanceStack builds the real /llm chain over a spy gateway. The membership
// answer is supplied, so the same helper serves both rules.
func provenanceStack(t *testing.T, spy *gatewaySpy, allow map[int32]bool) http.Handler {
	t.Helper()

	backend := llmproxytest.Start(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		spy.called = true
		spy.headers = r.Header.Clone()
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"ok":true}`)
	}))
	t.Cleanup(backend.Close)

	mw := middleware.Project(middleware.ProjectConfig{
		Resolver:        stubPersonalResolver{id: edgePersonalProject},
		PublicProjectID: 1,
		Membership:      middleware.NewProjectMembershipWith(stubMemberQuerier{allow: allow}),
	})
	return mw(proxyTo(t, backend, "sekret"))
}

func edgeBody() *strings.Reader {
	return strings.NewReader(`{"model":"gpt-4o-mini"}`)
}

// TestEdge_UnprovenancedIdentityIsRefusedBeforeTheGateway is direction 1 of
// issue #461, end to end. It also proves that the signed identity header does
// not carry provenance: without the rule the edge signs this identity exactly
// like any other, so the signature cannot refuse it.
func TestEdge_UnprovenancedIdentityIsRefusedBeforeTheGateway(t *testing.T) {
	user := auth.User{ID: "42", UserID: "42", TokenID: "900", AuthType: "token"}

	// The two names come from the deleted test. Each builder writes the same
	// principal, so the provenance is the only variable.
	cases := map[string]func(context.Context) context.Context{
		// auth.ContextWithUser records no provenance at all.
		"plain context identity": func(ctx context.Context) context.Context {
			return auth.ContextWithUser(ctx, user)
		},
		// ADR-0017 retired AUTH_DEV_MODE. This source must stay refused.
		"development provenance": func(ctx context.Context) context.Context {
			return auth.ContextWithAuthenticatedUser(ctx, user, auth.AuthenticationSourceDevelopment)
		},
	}

	for name, withIdentity := range cases {
		t.Run(name, func(t *testing.T) {
			spy := &gatewaySpy{}
			stack := provenanceStack(t, spy, map[int32]bool{})

			req := httptest.NewRequest(http.MethodPost, "/llm/v1/chat/completions", edgeBody())
			req = req.WithContext(withIdentity(req.Context()))

			rec := httptest.NewRecorder()
			stack.ServeHTTP(rec, req)

			if spy.called {
				t.Errorf("the gateway was called; it received %q as the billed project",
					spy.headers.Get(HeaderProjectID))
			}
			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("status = %d, want 401; body = %s", rec.Code, rec.Body.String())
			}
		})
	}
}

// TestEdge_EveryTrustedProvenanceReachesTheGateway is direction 2 of issue
// #461, end to end. Each of the three accepted provenance values reaches the
// gateway, and each carries the correct signed project.
func TestEdge_EveryTrustedProvenanceReachesTheGateway(t *testing.T) {
	cases := map[string]auth.AuthenticationSource{
		"api key": auth.AuthenticationSourceAPIKey,
		"token":   auth.AuthenticationSourceToken,
		"session": auth.AuthenticationSourceSession,
	}

	for name, source := range cases {
		t.Run(name, func(t *testing.T) {
			spy := &gatewaySpy{}
			stack := provenanceStack(t, spy, map[int32]bool{})

			req := httptest.NewRequest(http.MethodPost, "/llm/v1/chat/completions", edgeBody())
			user := auth.User{ID: "42", UserID: "42", TokenID: "900", AuthType: "token"}
			req = req.WithContext(auth.ContextWithAuthenticatedUser(req.Context(), user, source))

			rec := httptest.NewRecorder()
			stack.ServeHTTP(rec, req)

			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
			}
			if !spy.called {
				t.Fatal("the gateway was never called")
			}
			assertBilledProject(t, spy.headers, "7")
		})
	}
}

// TestEdge_PrincipalNameForeignProjectNeverReachesTheGatewayIdentity is
// direction 1 of issue #459, end to end. The principal name asks for project
// 9999. The caller is not a member of it. The gateway must not receive 9999.
func TestEdge_PrincipalNameForeignProjectNeverReachesTheGatewayIdentity(t *testing.T) {
	spy := &gatewaySpy{}
	stack := provenanceStack(t, spy, map[int32]bool{})

	req := httptest.NewRequest(http.MethodPost, "/llm/v1/chat/completions", edgeBody())
	user := auth.User{ID: "11", UserID: "11", TokenID: "900", Name: ":system:project:9999:", AuthType: "token"}
	req = withEdgeProvenance(req, user)

	rec := httptest.NewRecorder()
	stack.ServeHTTP(rec, req)

	if !spy.called {
		t.Fatal("the gateway was never called; the refusal must be silent")
	}
	if got := spy.headers.Get(HeaderProjectID); got == "9999" {
		t.Fatal("the gateway billed project 9999, which the principal name asked for")
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}
	assertBilledProject(t, spy.headers, "7")
}

// TestEdge_PrincipalNameMemberProjectReachesTheGatewayIdentity is direction 2
// of issue #459, end to end. The Pylon system project-user still bills the
// project its name asks for, because it belongs to that project.
func TestEdge_PrincipalNameMemberProjectReachesTheGatewayIdentity(t *testing.T) {
	spy := &gatewaySpy{}
	stack := provenanceStack(t, spy, map[int32]bool{42: true})

	req := httptest.NewRequest(http.MethodPost, "/llm/v1/chat/completions", edgeBody())
	user := auth.User{ID: "11", UserID: "11", TokenID: "900", Name: ":system:project:42:", AuthType: "token"}
	req = withEdgeProvenance(req, user)

	rec := httptest.NewRecorder()
	stack.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}
	if !spy.called {
		t.Fatal("the gateway was never called")
	}
	assertBilledProject(t, spy.headers, "42")
}
