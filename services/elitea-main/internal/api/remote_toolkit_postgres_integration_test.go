package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	apimw "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/middleware"
	desktopopsapi "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/v2/desktopops"
	v2mcp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/v2/mcp"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/localturn"
	toolkitcalltoolapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/toolkitcalltool"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/audit"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/auth"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/legacyrbac"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/storage"
)

// The gate is what this file measures; the turn binding and the agent
// authorization have their own tests, so here they admit everything.
type remoteGateTurns struct{}

func (remoteGateTurns) Live(_ context.Context, _, _ int64, _ localturn.Credential, executionID string) (localturn.LiveTurn, error) {
	return localturn.LiveTurn{ExecutionID: executionID, ApplicationID: 11, VersionID: 12}, nil
}

type remoteGateLedger struct{}

func (remoteGateLedger) NextConfirmationInterruptID(_ context.Context, _, callDigest string) (string, error) {
	return localturn.ConfirmationInterruptID(callDigest, 0), nil
}

func (remoteGateLedger) ConsumeConfirmation(context.Context, localturn.ConfirmationClaim) (localturn.ConfirmationOutcome, error) {
	return localturn.ConfirmationOutcome{Accepted: true}, nil
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
			Confirmations: remoteGateLedger{},
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

type remoteGatePlatformAuthorizer struct{}

func (remoteGatePlatformAuthorizer) AuthorizeRemoteTool(_ context.Context, request storage.RemoteToolAuthorization) (storage.RemoteToolGrant, error) {
	if request.ToolkitID != 0 {
		return storage.RemoteToolGrant{}, storage.ErrRemoteToolNotInAgent
	}
	return storage.RemoteToolGrant{ToolkitType: "mcp_elitea_internal_applications", ToolkitName: "Elitea Applications", PlatformMCP: true}, nil
}

const remoteToolkitEditorID = 15604

// freshReplays claims every key: each call below has its own Idempotency-Key,
// and the replay store's semantics have their own tests against a real
// JetStream (desktopops' platform_replay_nats_test.go).
type freshReplays struct{}

func (freshReplays) Claim(context.Context, string, string) (desktopopsapi.PlatformReplay, error) {
	return desktopopsapi.PlatformReplay{Verdict: desktopopsapi.ReplayClaimed}, nil
}

func (freshReplays) Complete(context.Context, string, desktopopsapi.PlatformReplay, desktopopsapi.PlatformReplayResult) error {
	return nil
}

func (freshReplays) Release(context.Context, string, desktopopsapi.PlatformReplay) error { return nil }

// One internal builder (Elitea Applications) through executeRemoteToolkitTool
// on a CLEAN database (issue 1231): the real Auth and legacyrbac resolver, and
// the REAL MCP server dispatch behind the router's bridge. The call runs as
// the caller: a viewer lists the project's agents, and is refused the create
// by the operation's own permission, which an editor holds.
func TestAPlatformMCPToolRunsAsTheCallerOnACleanDatabase(t *testing.T) {
	pool := newCredentialJourneyPool(t)
	ctx, cancel := context.WithTimeout(context.Background(), credentialJourneyDeadline)
	defer cancel()
	for id, role := range map[int]string{remoteToolkitViewerID: "viewer", remoteToolkitEditorID: "editor"} {
		if _, err := pool.Exec(ctx, fmt.Sprintf(`
INSERT INTO public.auth_core__user (id, email, name) VALUES (%[1]d, 'platform-%[2]s@test.local', 'Platform %[2]s')
ON CONFLICT (id) DO NOTHING;
INSERT INTO public.auth_core__project_role (project_id, name) VALUES (1, '%[2]s')
ON CONFLICT (project_id, name) DO NOTHING;
INSERT INTO public.auth_core__project_user_role (project_id, user_id, role_id)
SELECT 1, %[1]d, id FROM public.auth_core__project_role WHERE project_id = 1 AND name = '%[2]s'
ON CONFLICT DO NOTHING;
INSERT INTO public.auth_core__token (id, uuid, user_id, name) VALUES (%[1]d, 'platform-%[2]s-pat', %[1]d, 'desktop')
ON CONFLICT (id) DO NOTHING;`, id, role)); err != nil {
			t.Fatalf("seed the %s: %v", role, err)
		}
	}

	permissions := legacyrbac.NewPostgresResolver(pool)
	bridge := desktopopsapi.NewPlatformToolBridge()
	bridge.Bind(platformToolAdapter{handler: v2mcp.NewHandler(pool, nil, nil, permissions)})
	serve := func(userID int, tool, arguments string) map[string]any {
		t.Helper()
		route, err := desktopopsapi.NewRemoteToolkitRoute(desktopopsapi.RemoteToolkitDependencies{
			Worker: "rust", Authorizer: remoteGatePlatformAuthorizer{}, Turns: remoteGateTurns{}, Audit: remoteGateAudit{},
			Confirmations: remoteGateLedger{}, Platform: bridge, Replays: freshReplays{},
		}, apimw.AuthConfig{
			Validator:          apimw.TokenValidator(platformTokenValidator{userID: userID}),
			PrincipalValidator: testPrincipalValidator{},
		}, permissions)
		if err != nil {
			t.Fatal(err)
		}
		request := httptest.NewRequest(http.MethodPost, "/api/v2/elitea_core/remote_toolkit_call/prompt_lib/1/0",
			strings.NewReader(`{"execution_id":"0123456789abcdef0123456789abcdef","application_id":11,"version_id":12,`+
				`"toolkit_ref":"tkr1_00112233445566778899aabbccddeeff","tool_name":"`+tool+`","arguments":`+arguments+`}`))
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("Idempotency-Key", fmt.Sprintf("platform-%d-%s", userID, tool))
		recorder := httptest.NewRecorder()
		route.ServeHTTP(recorder, testAuthHeader(request))
		if recorder.Code != http.StatusOK {
			t.Fatalf("%s as %d answered %d: %s", tool, userID, recorder.Code, recorder.Body)
		}
		var body map[string]any
		if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		return body
	}

	listed := serve(remoteToolkitViewerID, "get_elitea_core_applications", `{"limit":5}`)
	if listed["ok"] != true || listed["toolkit_type"] != "mcp_elitea_internal_applications" {
		t.Fatalf("the viewer's list answered %v", listed)
	}
	if _, ok := listed["result"].(map[string]any)["total"]; !ok {
		t.Fatalf("the list result is not the operation's page: %v", listed["result"])
	}
	create := `{"name":"from-desktop","description":"d","versions":[{"name":"base","instructions":"","llm_settings":{}}]}`
	refused := serve(remoteToolkitViewerID, "post_elitea_core_applications", create)
	if refused["ok"] != false || refused["status"] != "tool_error" ||
		!strings.Contains(fmt.Sprint(refused["error"]), "models.applications.applications.create") {
		t.Fatalf("the viewer's create answered %v", refused)
	}
	editor := serve(remoteToolkitEditorID, "post_elitea_core_applications", create)
	created, _ := editor["result"].(map[string]any)
	details, _ := created["version_details"].(map[string]any)
	if editor["ok"] != true || details == nil || fmt.Sprint(details["author_id"]) != fmt.Sprint(remoteToolkitEditorID) {
		t.Fatalf("the editor's create did not run as the editor: %v", editor)
	}
}

type platformTokenValidator struct{ userID int }

func (v platformTokenValidator) ValidateToken(_ context.Context, token string) (auth.User, error) {
	if token != testAuthToken {
		return auth.User{}, fmt.Errorf("unexpected token %q", token)
	}
	id := fmt.Sprint(v.userID)
	return auth.User{ID: id, UserID: id, TokenID: id, AuthType: "token"}, nil
}
