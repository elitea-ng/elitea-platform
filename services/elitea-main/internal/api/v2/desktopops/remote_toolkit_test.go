package desktopops

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	executionapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/execution"
	toolkitcalltoolapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/toolkitcalltool"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth"
	executiondomain "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/execution"
)

type fakeToolRuns struct {
	outcome toolkitcalltoolapp.RunOutcome
	err     error
	request toolkitcalltoolapp.RunRequest
	calls   int
}

func (f *fakeToolRuns) RunTool(_ context.Context, request toolkitcalltoolapp.RunRequest) (toolkitcalltoolapp.RunOutcome, error) {
	f.calls++
	f.request = request
	return f.outcome, f.err
}

const remoteURL = "/api/v2/elitea_core/remote_toolkit_call/prompt_lib/3/61"

func desktopToken() *auth.User {
	return &auth.User{ID: "7", UserID: "7", TokenID: "70", AuthType: "token", NativeClientID: "ai.elitea.desktop"}
}

func TestRemoteToolkitRunsTheCallersToolThroughTheSharedUseCase(t *testing.T) {
	runs := &fakeToolRuns{outcome: toolkitcalltoolapp.RunOutcome{
		ExecutionID: "e1", Status: toolkitcalltoolapp.RunStatusOK, ResultJSON: `{"created":true}`,
		ToolkitType: "github", ToolName: "create_issue",
	}}
	h := &remoteToolkitHandler{useCase: runs, worker: "python"}
	response := serveWith(t, http.MethodPost, RemoteToolkitPath, remoteURL,
		`{"tool_name":"create_issue","arguments":{"title":"x"},"request_id":"r1"}`, desktopToken(), h.serve)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", response.Code, response.Body)
	}
	got := runs.request
	if got.ProjectID != 3 || got.ToolkitID != 61 || got.ActorUserID != 7 || got.ToolName != "create_issue" ||
		string(got.Arguments) != `{"title":"x"}` || got.RequestID != "r1" {
		t.Fatalf("run request = %+v", got)
	}
	var body map[string]any
	_ = json.Unmarshal(response.Body.Bytes(), &body)
	if body["ok"] != true || body["status"] != "ok" || body["task_id"] != "e1" ||
		body["result"].(map[string]any)["created"] != true {
		t.Fatalf("body = %s", response.Body)
	}
}

func TestRemoteToolkitRefusesABrowserSession(t *testing.T) {
	runs := &fakeToolRuns{}
	h := &remoteToolkitHandler{useCase: runs}
	session := &auth.User{ID: "7", UserID: "7", AuthType: "session"}
	response := serveWith(t, http.MethodPost, RemoteToolkitPath, remoteURL, `{"tool_name":"t"}`, session, h.serve)
	if response.Code != http.StatusForbidden || !strings.Contains(response.Body.String(), remoteToolkitRequiresToken) || runs.calls != 0 {
		t.Fatalf("status = %d, body %s, calls %d", response.Code, response.Body, runs.calls)
	}
}

func TestRemoteToolkitWithoutAWorkerAnswers501(t *testing.T) {
	h := &remoteToolkitHandler{}
	response := serveWith(t, http.MethodPost, RemoteToolkitPath, remoteURL, `{"tool_name":"t"}`, desktopToken(), h.serve)
	if response.Code != http.StatusNotImplemented || !strings.Contains(response.Body.String(), "remote_toolkit_unavailable") {
		t.Fatalf("status = %d, body %s", response.Code, response.Body)
	}
}

func TestRemoteToolkitBoundsTheRequest(t *testing.T) {
	runs := &fakeToolRuns{}
	h := &remoteToolkitHandler{useCase: runs}
	for name, tc := range map[string]struct {
		body   string
		status int
	}{
		"no tool name":         {`{"arguments":{}}`, http.StatusBadRequest},
		"arguments not object": {`{"tool_name":"t","arguments":[1]}`, http.StatusBadRequest},
		"two values":           {`{"tool_name":"t"} {}`, http.StatusBadRequest},
		"too large":            {`{"tool_name":"t","arguments":{"x":"` + strings.Repeat("a", 1<<20) + `"}}`, http.StatusRequestEntityTooLarge},
	} {
		response := serveWith(t, http.MethodPost, RemoteToolkitPath, remoteURL, tc.body, desktopToken(), h.serve)
		if response.Code != tc.status {
			t.Errorf("%s: status = %d, body %.200s", name, response.Code, response.Body)
		}
	}
	if runs.calls != 0 {
		t.Fatalf("a refused request reached the use case %d times", runs.calls)
	}
}

func TestRemoteToolkitOutcomesMapToTypedAnswers(t *testing.T) {
	challenge := &executiondomain.ToolkitAuthorizationRequired{
		ToolkitName: "jira", ToolkitType: "mcp", ToolkitID: "61", ServerURL: "https://mcp.example.test",
	}
	for name, tc := range map[string]struct {
		worker  string
		outcome toolkitcalltoolapp.RunOutcome
		status  int
		want    []string
	}{
		"tool error": {"python", toolkitcalltoolapp.RunOutcome{Status: toolkitcalltoolapp.RunStatusToolError, ErrorMessage: "boom"},
			http.StatusOK, []string{`"status":"tool_error"`, `"ok":false`}},
		"authorization": {"python", toolkitcalltoolapp.RunOutcome{Status: toolkitcalltoolapp.RunStatusAuthorizationRequired, AuthorizationRequired: challenge},
			http.StatusConflict, []string{`"error":"mcp_authorization_required"`, `"server_url":"https://mcp.example.test"`, `"toolkit_id":"61"`}},
		"unknown tool": {"python", toolkitcalltoolapp.RunOutcome{Status: toolkitcalltoolapp.RunStatusUnknownTool},
			http.StatusUnprocessableEntity, []string{`"remote_tool_unsupported"`, `"unknown_tool"`}},
		"rust refuses an effectful tool": {"rust", toolkitcalltoolapp.RunOutcome{
			Status: toolkitcalltoolapp.RunStatusRuntimeFailure, ToolName: "create_issue",
			FailureCode: "RUNTIME_ERROR_CODE_V1_UNSUPPORTED_CAPABILITY",
		}, http.StatusUnprocessableEntity, []string{`"worker_refused"`, "read-only", `\"create_issue\"`}},
		"provider failure": {"python", toolkitcalltoolapp.RunOutcome{
			Status: toolkitcalltoolapp.RunStatusRuntimeFailure, FailureCode: "RUNTIME_ERROR_CODE_V1_DEPENDENCY_UNAVAILABLE", ErrorMessage: "down",
		}, http.StatusBadGateway, []string{`"remote_toolkit_failed"`}},
	} {
		h := &remoteToolkitHandler{useCase: &fakeToolRuns{outcome: tc.outcome}, worker: tc.worker}
		response := serveWith(t, http.MethodPost, RemoteToolkitPath, remoteURL, `{"tool_name":"t"}`, desktopToken(), h.serve)
		if response.Code != tc.status {
			t.Errorf("%s: status = %d, body %s", name, response.Code, response.Body)
		}
		for _, want := range tc.want {
			if !strings.Contains(response.Body.String(), want) {
				t.Errorf("%s: body %s lacks %s", name, response.Body, want)
			}
		}
	}
}

func TestRemoteToolkitRunErrorsMapToTypedAnswers(t *testing.T) {
	for name, tc := range map[string]struct {
		err    error
		status int
		code   string
	}{
		"pending":       {&toolkitcalltoolapp.PendingRun{ExecutionID: "e9"}, http.StatusGatewayTimeout, "remote_toolkit_timeout"},
		"not visible":   {toolkitcalltoolapp.ErrToolkitNotVisible, http.StatusNotFound, "toolkit_not_found"},
		"idempotency":   {executionapp.ErrIdempotencyConflict, http.StatusConflict, "idempotency_conflict"},
		"unsupported":   {&toolkitcalltoolapp.UnsupportedToolkitTypeError{ToolkitType: "jira", Reason: "no"}, http.StatusUnprocessableEntity, "remote_tool_unsupported"},
		"invalid":       {toolkitcalltoolapp.ErrInvalidToolRun, http.StatusBadRequest, "invalid_remote_toolkit_call"},
		"settings down": {toolkitcalltoolapp.ErrToolkitSettingsResolutionUnavailable, http.StatusServiceUnavailable, "remote_toolkit_unavailable_now"},
		"other":         {errors.New("x"), http.StatusInternalServerError, "remote_toolkit_failed"},
	} {
		h := &remoteToolkitHandler{useCase: &fakeToolRuns{err: tc.err}}
		response := serveWith(t, http.MethodPost, RemoteToolkitPath, remoteURL, `{"tool_name":"t"}`, desktopToken(), h.serve)
		if response.Code != tc.status || !strings.Contains(response.Body.String(), `"`+tc.code+`"`) {
			t.Errorf("%s: status = %d, body %s", name, response.Code, response.Body)
		}
	}
}

func TestRemoteToolkitRouteRequiresTheCredentialPlane(t *testing.T) {
	if _, err := NewRemoteToolkitRoute(&fakeToolRuns{}, "python", apimwZero(), nil); !errors.Is(err, ErrInvalidRoute) {
		t.Fatalf("err = %v", err)
	}
}
