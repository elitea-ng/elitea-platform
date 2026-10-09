package api

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	apimw "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/middleware"
	desktopopsapi "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/desktopops"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/localturn"
	toolkitcalltoolapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/toolkitcalltool"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/audit"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/legacyrbac"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/storage"
)

// The gate is what this file measures; the turn binding and the agent
// authorization have their own tests, so here they admit everything.
type remoteGateTurns struct{}

func (remoteGateTurns) Live(_ context.Context, _, _ int64, executionID string) (localturn.LiveTurn, error) {
	return localturn.LiveTurn{ExecutionID: executionID, ApplicationID: 11, VersionID: 12}, nil
}

type remoteGateAuthorizer struct{}

func (remoteGateAuthorizer) AuthorizeRemoteTool(context.Context, storage.RemoteToolAuthorization) (storage.RemoteToolGrant, error) {
	return storage.RemoteToolGrant{ToolkitType: "github", ToolkitName: "gh"}, nil
}

type remoteGateAudit struct{}

func (remoteGateAudit) Record(context.Context, audit.Event) {}

// executeRemoteToolkitTool's gate on a CLEAN database (ADR-0029 decision 5b):
// the real Auth, the real legacyrbac resolver and the migration corpus, with a
// recording use case behind them. A viewer of a Go-provisioned project — no
// per-project rows, so the central fallback decides — reaches the use case
// through shared/0156's grant; a project the caller is not a member of does
// not; a session-shaped principal is refused before the use case.
const remoteToolkitViewerID = 15603

type remoteToolkitTokenValidator struct{ tokenID, authType string }

func (v remoteToolkitTokenValidator) ValidateToken(_ context.Context, token string) (auth.User, error) {
	if token != testAuthToken {
		return auth.User{}, fmt.Errorf("unexpected token %q", token)
	}
	id := fmt.Sprint(remoteToolkitViewerID)
	return auth.User{ID: id, UserID: id, TokenID: v.tokenID, AuthType: v.authType}, nil
}

type recordingToolRuns struct{ calls int }

func (r *recordingToolRuns) RunTool(_ context.Context, request toolkitcalltoolapp.RunRequest) (toolkitcalltoolapp.RunOutcome, error) {
	r.calls++
	return toolkitcalltoolapp.RunOutcome{
		ExecutionID: "e", Status: toolkitcalltoolapp.RunStatusOK, ResultJSON: `{}`, ToolName: request.ToolName,
	}, nil
}

func TestAViewerReachesTheRemoteToolkitCallOnACleanDatabase(t *testing.T) {
	pool := newCredentialJourneyPool(t)
	ctx, cancel := context.WithTimeout(context.Background(), credentialJourneyDeadline)
	defer cancel()
	if _, err := pool.Exec(ctx, fmt.Sprintf(`
INSERT INTO public.auth_core__user (id, email, name) VALUES (%[1]d, 'remote-viewer@test.local', 'Remote viewer')
ON CONFLICT (id) DO NOTHING;
INSERT INTO public.auth_core__project_role (project_id, name) VALUES (1, 'viewer')
ON CONFLICT (project_id, name) DO NOTHING;
INSERT INTO public.auth_core__project_user_role (project_id, user_id, role_id)
SELECT 1, %[1]d, id FROM public.auth_core__project_role WHERE project_id = 1 AND name = 'viewer'
ON CONFLICT DO NOTHING;
INSERT INTO public.auth_core__token (id, uuid, user_id, name) VALUES (%[1]d, 'remote-viewer-pat', %[1]d, 'desktop');`, remoteToolkitViewerID)); err != nil {
		t.Fatalf("seed the viewer: %v", err)
	}
	var overrides int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM public.auth_core__project_role_permission WHERE project_id = 1`).Scan(&overrides); err != nil {
		t.Fatal(err)
	}
	if overrides != 0 {
		t.Fatalf("project 1 carries %d per-project rows; this test measures the central fallback", overrides)
	}

	serve := func(validator remoteToolkitTokenValidator, runs *recordingToolRuns, path string) (int, string) {
		route, err := desktopopsapi.NewRemoteToolkitRoute(desktopopsapi.RemoteToolkitDependencies{
			Runs: runs, Worker: "rust", Authorizer: remoteGateAuthorizer{}, Turns: remoteGateTurns{}, Audit: remoteGateAudit{},
		}, apimw.AuthConfig{
			Validator: apimw.TokenValidator(validator), PrincipalValidator: testPrincipalValidator{},
		}, legacyrbac.NewPostgresResolver(pool))
		if err != nil {
			t.Fatal(err)
		}
		request := httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{"execution_id":"0123456789abcdef0123456789abcdef",`+
			`"application_id":11,"version_id":12,"toolkit_ref":"tkr1_00112233445566778899aabbccddeeff",`+
			`"tool_name":"read_file","arguments":{}}`))
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("Idempotency-Key", "gate-test")
		recorder := httptest.NewRecorder()
		route.ServeHTTP(recorder, testAuthHeader(request))
		return recorder.Code, recorder.Body.String()
	}

	token := remoteToolkitTokenValidator{tokenID: fmt.Sprint(remoteToolkitViewerID), authType: "token"}
	runs := &recordingToolRuns{}
	if status, body := serve(token, runs, "/api/v2/elitea_core/remote_toolkit_call/prompt_lib/1/61"); status != http.StatusOK || runs.calls != 1 {
		t.Fatalf("a viewer's token call answered %d (use case calls %d). Body: %s", status, runs.calls, body)
	}
	runs = &recordingToolRuns{}
	if status, body := serve(token, runs, "/api/v2/elitea_core/remote_toolkit_call/prompt_lib/2/61"); status != http.StatusForbidden || runs.calls != 0 {
		t.Fatalf("a project the caller is not a member of answered %d (calls %d). Body: %s", status, runs.calls, body)
	}
	runs = &recordingToolRuns{}
	if status, body := serve(remoteToolkitTokenValidator{}, runs, "/api/v2/elitea_core/remote_toolkit_call/prompt_lib/1/61"); status != http.StatusForbidden ||
		!strings.Contains(body, "remote_toolkit_requires_token") || runs.calls != 0 {
		t.Fatalf("a non-token principal answered %d (calls %d). Body: %s", status, runs.calls, body)
	}
}
