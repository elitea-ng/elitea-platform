package middleware

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth"
)

// These tests restore the fail-closed identity rule of the /llm edge (issue
// #461).
//
// The deleted runtimecomposition.CurrentLLMCallerResolver called
// auth.RuntimePrincipalFromContext and refused the request when the call
// failed. Its test,
// git show origin/main:services/elitea-main/internal/runtimecomposition/llm_adapters_test.go
// lines 43-99, pinned two refusals by name. The two names below are those
// names, unchanged. The subject moved from the deleted resolver to
// middleware.Project, because Project is now the single place where an identity
// becomes a billing project.
//
// The refusal is a real rule and not a duplicate of the signed identity header.
// The HMAC on X-Elitea-Project-Id proves that this process wrote the header
// (internal/llmproxy/identity.go). It does not prove how the caller
// authenticated: the signing input is the project, the user and the tenant, and
// none of them carries the authentication source. Without this rule an identity
// with no provenance is signed exactly like an identity with provenance. The
// end-to-end proof is
// TestEdge_UnprovenancedIdentityIsRefusedBeforeTheGateway in
// internal/llmproxy/project_provenance_test.go.

// provenanceUser is the same principal in every case below, so the provenance
// is the only variable.
var provenanceUser = auth.User{ID: "42", UserID: "42", TokenID: "900", AuthType: "token"}

// runProvenance drives the middleware over ctx and reports the result.
func runProvenance(ctx context.Context) (*httptest.ResponseRecorder, ProjectContext, bool) {
	var seen ProjectContext
	var invoked bool
	mw := Project(ProjectConfig{
		Resolver:        &fakeResolver{id: personalProject},
		PublicProjectID: 1,
		Membership:      NewProjectMembershipWith(&fakeMemberQuerier{allow: map[int32]bool{}}),
	})
	req := httptest.NewRequest(http.MethodPost, "/llm/v1/chat/completions", nil).WithContext(ctx)
	rec := httptest.NewRecorder()
	mw(captureHandler(&seen, &invoked)).ServeHTTP(rec, req)
	return rec, seen, invoked
}

// TestProject_FailsClosedWithoutTrustedProvenance is direction 1: an identity
// that no accepted authentication path recorded gets HTTP 401, and the request
// stops here.
func TestProject_FailsClosedWithoutTrustedProvenance(t *testing.T) {
	cases := map[string]context.Context{
		// A user placed by auth.ContextWithUser, which records no provenance.
		"plain context identity": auth.ContextWithUser(context.Background(), provenanceUser),
		// ADR-0017 retired AUTH_DEV_MODE. This source has no producer, and it
		// must stay refused if one ever returns.
		"development provenance": auth.ContextWithAuthenticatedUser(
			context.Background(), provenanceUser, auth.AuthenticationSourceDevelopment,
		),
		// The zero value of the source type. It reads as "no provenance".
		"unknown provenance": auth.ContextWithAuthenticatedUser(
			context.Background(), provenanceUser, auth.AuthenticationSourceUnknown,
		),
	}

	for name, ctx := range cases {
		t.Run(name, func(t *testing.T) {
			rec, seen, invoked := runProvenance(ctx)

			// The handler assertion comes first. A status test alone cannot
			// tell a refusal from a request that failed for another reason
			// (issue #461, criterion 4).
			if invoked {
				t.Error("the request reached the proxy handler")
			}
			if seen.ProjectID != 0 {
				t.Errorf("a project context was injected: %+v", seen)
			}
			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("status = %d, want 401; body = %s", rec.Code, rec.Body.String())
			}
			assertProjectJSONErrorBody(t, rec.Body.Bytes())
		})
	}
}

// TestProject_AcceptsEveryTrustedProvenance is direction 2: each of the three
// accepted provenance values still resolves the caller's project. The rule
// refuses an unrecorded identity, and it costs an authenticated caller nothing.
func TestProject_AcceptsEveryTrustedProvenance(t *testing.T) {
	cases := map[string]auth.AuthenticationSource{
		"api key": auth.AuthenticationSourceAPIKey,
		"token":   auth.AuthenticationSourceToken,
		"session": auth.AuthenticationSourceSession,
	}

	for name, source := range cases {
		t.Run(name, func(t *testing.T) {
			ctx := auth.ContextWithAuthenticatedUser(context.Background(), provenanceUser, source)
			rec, seen, invoked := runProvenance(ctx)

			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
			}
			if !invoked {
				t.Fatal("the request did not reach the proxy handler")
			}
			if seen.ProjectID != personalProject {
				t.Errorf("billed project = %d, want %d", seen.ProjectID, personalProject)
			}
		})
	}
}

// TestProject_NoIdentityStillPassesThrough states the one case this rule does
// not change. An absent identity is the Auth middleware's decision, and Project
// keeps deferring to it. A present identity with no recorded provenance is a
// claim, and Project refuses it.
func TestProject_NoIdentityStillPassesThrough(t *testing.T) {
	rec, seen, invoked := runProvenance(context.Background())

	if !invoked {
		t.Fatal("next handler should be invoked when there is no auth user")
	}
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
	if (seen != ProjectContext{}) {
		t.Errorf("expected no project context injected, got %+v", seen)
	}
}
