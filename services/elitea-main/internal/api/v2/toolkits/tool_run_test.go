package toolkits

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	toolkitcalltoolapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/toolkitcalltool"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth"
	"github.com/go-chi/chi/v5"
)

type stubToolRuns struct {
	outcome  toolkitcalltoolapp.RunOutcome
	err      error
	calls    int
	captured toolkitcalltoolapp.RunRequest
}

func (s *stubToolRuns) RunTool(
	_ context.Context, request toolkitcalltoolapp.RunRequest,
) (toolkitcalltoolapp.RunOutcome, error) {
	s.calls++
	s.captured = request
	return s.outcome, s.err
}

func newToolRunRequest(t *testing.T, path, projectID, toolID, body string) *http.Request {
	t.Helper()
	request := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	routeContext := chi.NewRouteContext()
	routeContext.URLParams.Add("projectID", projectID)
	if toolID != "" {
		routeContext.URLParams.Add("toolID", toolID)
	}
	ctx := context.WithValue(request.Context(), chi.RouteCtxKey, routeContext)
	ctx = auth.ContextWithUser(ctx, auth.User{ID: "7", UserID: "7", AuthType: "user"})
	return request.WithContext(ctx)
}

const toolRunBody = `{"toolkit_config":{"toolkit_id":19},"tool_name":"list_issues","tool_params":{"repo":"a"}}`

// The 503 is the honest answer where no runtime is composed, and its body is
// unchanged from what every deployment returned before this capability existed.
func TestTestToolAnswersTheHistorical503WithoutAUseCase(t *testing.T) {
	handler := NewHandlerWithRepo(nil)
	for name, run := range map[string]func(http.ResponseWriter, *http.Request){
		"test_tool":         handler.TestTool,
		"test_toolkit_tool": handler.TestToolkitTool,
	} {
		t.Run(name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			run(recorder, newToolRunRequest(t, "/x", "1", "19", toolRunBody))
			if recorder.Code != http.StatusServiceUnavailable {
				t.Fatalf("status %d, want 503", recorder.Code)
			}
			var body map[string]any
			if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
				t.Fatal(err)
			}
			if body["ok"] != false || body["error"] != "indexer service not available" {
				t.Fatalf("the historical 503 body changed: %v", body)
			}
		})
	}
}

func TestTestToolRunsTheToolAndAnswersTheResult(t *testing.T) {
	runs := &stubToolRuns{outcome: toolkitcalltoolapp.RunOutcome{
		ExecutionID: "exec-1",
		Status:      toolkitcalltoolapp.RunStatusOK,
		ResultJSON:  `{"issues":1}`,
		ToolName:    "list_issues",
	}}
	handler := NewHandlerWithRepo(nil, WithToolRuns(runs))
	recorder := httptest.NewRecorder()
	handler.TestTool(recorder, newToolRunRequest(t, "/x", "1", "19", toolRunBody))

	if recorder.Code != http.StatusOK {
		t.Fatalf("status %d: %s", recorder.Code, recorder.Body.String())
	}
	if runs.calls != 1 {
		t.Fatalf("the use case ran %d times", runs.calls)
	}
	// The ROUTE's toolkit id wins: it is what the permission gate resolved.
	if runs.captured.ToolkitID != 19 || runs.captured.ProjectID != 1 ||
		runs.captured.ActorUserID != 7 || runs.captured.ToolName != "list_issues" {
		t.Fatalf("request %+v", runs.captured)
	}
	var body map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body["ok"] != true || body["task_id"] != "exec-1" {
		t.Fatalf("body %v", body)
	}
}

// The sibling route names no toolkit, so the body's own id is used.
func TestTestToolkitToolTakesTheToolkitFromTheBody(t *testing.T) {
	runs := &stubToolRuns{outcome: toolkitcalltoolapp.RunOutcome{
		ExecutionID: "exec-2", Status: toolkitcalltoolapp.RunStatusOK, ResultJSON: `{}`,
	}}
	handler := NewHandlerWithRepo(nil, WithToolRuns(runs))
	recorder := httptest.NewRecorder()
	handler.TestToolkitTool(recorder, newToolRunRequest(t, "/x", "1", "", toolRunBody))

	if recorder.Code != http.StatusOK {
		t.Fatalf("status %d: %s", recorder.Code, recorder.Body.String())
	}
	if runs.captured.ToolkitID != 19 {
		t.Fatalf("toolkit id %d", runs.captured.ToolkitID)
	}
}

// The bounded wait expiring is a 504 that names the task id, so the caller can
// poll for a run that is still going.
func TestTestToolAnswers504WhenTheBoundedWaitExpires(t *testing.T) {
	runs := &stubToolRuns{err: &toolkitcalltoolapp.PendingRun{ExecutionID: "exec-9"}}
	handler := NewHandlerWithRepo(nil, WithToolRuns(runs))
	recorder := httptest.NewRecorder()
	handler.TestTool(recorder, newToolRunRequest(t, "/x", "1", "19", toolRunBody))

	if recorder.Code != http.StatusGatewayTimeout {
		t.Fatalf("status %d, want 504", recorder.Code)
	}
	var body map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body["task_id"] != "exec-9" {
		t.Fatalf("the 504 does not name the execution: %v", body)
	}
}

func TestTestToolRefusesAnUnauthenticatedCaller(t *testing.T) {
	runs := &stubToolRuns{}
	handler := NewHandlerWithRepo(nil, WithToolRuns(runs))
	request := httptest.NewRequest(http.MethodPost, "/x", strings.NewReader(toolRunBody))
	routeContext := chi.NewRouteContext()
	routeContext.URLParams.Add("projectID", "1")
	routeContext.URLParams.Add("toolID", "19")
	request = request.WithContext(context.WithValue(request.Context(), chi.RouteCtxKey, routeContext))

	recorder := httptest.NewRecorder()
	handler.TestTool(recorder, request)
	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("status %d, want 401", recorder.Code)
	}
	if runs.calls != 0 {
		t.Fatal("an unauthenticated caller reached the use case")
	}
}

func TestTestToolRefusesABodyItCannotBind(t *testing.T) {
	runs := &stubToolRuns{}
	handler := NewHandlerWithRepo(nil, WithToolRuns(runs))
	recorder := httptest.NewRecorder()
	handler.TestToolkitTool(recorder, newToolRunRequest(t, "/x", "1", "", `{"tool_name":""}`))

	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status %d, want 400", recorder.Code)
	}
	if runs.calls != 0 {
		t.Fatal("an unbindable body reached the use case")
	}
}
