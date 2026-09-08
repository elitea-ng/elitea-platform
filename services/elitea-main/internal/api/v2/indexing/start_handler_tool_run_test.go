package indexing

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	indexingapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/indexing"
	toolkitcalltoolapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/toolkitcalltool"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth"
	"github.com/go-chi/chi/v5"
)

type toolRunStub struct {
	outcome  toolkitcalltoolapp.RunOutcome
	err      error
	calls    int
	captured toolkitcalltoolapp.RunRequest
}

func (s *toolRunStub) RunTool(
	_ context.Context, request toolkitcalltoolapp.RunRequest,
) (toolkitcalltoolapp.RunOutcome, error) {
	s.calls++
	s.captured = request
	return s.outcome, s.err
}

type unusedStartUseCase struct{ calls int }

func (u *unusedStartUseCase) StartIndexData(
	context.Context, indexingapp.StartRequest,
) (indexingapp.StartOutcome, error) {
	u.calls++
	return indexingapp.StartOutcome{TaskID: "task-1"}, nil
}

const toolRunStartBody = `{"toolkit_config":{"toolkit_id":19},"tool_name":"list_issues","tool_params":{"repo":"a"}}`

func newStartRequest(t *testing.T, query, body string, authenticated bool) *http.Request {
	t.Helper()
	request := httptest.NewRequest(http.MethodPost, "/x"+query, strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	routeContext := chi.NewRouteContext()
	routeContext.URLParams.Add("projectID", "1")
	ctx := context.WithValue(request.Context(), chi.RouteCtxKey, routeContext)
	if authenticated {
		ctx = auth.ContextWithUser(ctx, auth.User{ID: "7", UserID: "7", AuthType: "user"})
	}
	return request.WithContext(ctx)
}

// This is the reachable route wherever the runtime is composed
// (indexingapi.CurrentIndexStartPath shadows the toolkits handler on the same
// path), so the synchronous branch has to live here for a tool run to work at
// all. It used to be a validation refusal.
func TestStartRunsAToolWhenAwaitResponseIsNotFalse(t *testing.T) {
	runs := &toolRunStub{outcome: toolkitcalltoolapp.RunOutcome{
		ExecutionID: "exec-1", Status: toolkitcalltoolapp.RunStatusOK,
		ResultJSON: `{"issues":1}`, ToolName: "list_issues",
	}}
	start := &unusedStartUseCase{}
	handler, err := NewStartHandlerWithToolRuns(start, runs)
	if err != nil {
		t.Fatal(err)
	}
	for _, query := range []string{"", "?await_response=true", "?await_response=yes"} {
		recorder := httptest.NewRecorder()
		handler.Start(recorder, newStartRequest(t, query, toolRunStartBody, true))
		if recorder.Code != http.StatusOK {
			t.Fatalf("query %q: status %d: %s", query, recorder.Code, recorder.Body.String())
		}
	}
	if runs.calls != 3 {
		t.Fatalf("the tool ran %d times, want 3", runs.calls)
	}
	if runs.captured.ToolkitID != 19 || runs.captured.ProjectID != 1 || runs.captured.ActorUserID != 7 {
		t.Fatalf("request %+v", runs.captured)
	}
	if start.calls != 0 {
		t.Fatal("a tool run reached the index_data use case")
	}
}

// The ASYNCHRONOUS branch is unchanged. `await_response=false` is index_data's
// alone, and a tool run must not reach it.
func TestStartKeepsTheAsynchronousIndexBranch(t *testing.T) {
	runs := &toolRunStub{}
	start := &unusedStartUseCase{}
	handler, err := NewStartHandlerWithToolRuns(start, runs)
	if err != nil {
		t.Fatal(err)
	}
	recorder := httptest.NewRecorder()
	handler.Start(recorder, newStartRequest(t, "?await_response=false",
		`{"toolkit_config":{"toolkit_id":19},"tool_name":"index_data","tool_params":{"index_name":"docs"}}`, true))

	if recorder.Code != http.StatusOK {
		t.Fatalf("status %d: %s", recorder.Code, recorder.Body.String())
	}
	if start.calls != 1 {
		t.Fatalf("index_data reached the use case %d times", start.calls)
	}
	if runs.calls != 0 {
		t.Fatal("an asynchronous index_data request reached the tool-run use case")
	}
}

// A NON-index tool asked for asynchronously is still refused: nothing on that
// branch would ever hand its result back.
func TestStartStillRefusesAnAsynchronousNonIndexTool(t *testing.T) {
	runs := &toolRunStub{}
	handler, err := NewStartHandlerWithToolRuns(&unusedStartUseCase{}, runs)
	if err != nil {
		t.Fatal(err)
	}
	recorder := httptest.NewRecorder()
	handler.Start(recorder, newStartRequest(t, "?await_response=false", toolRunStartBody, true))
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status %d, want 400", recorder.Code)
	}
	if runs.calls != 0 {
		t.Fatal("an asynchronous non-index tool reached the tool-run use case")
	}
}

// Without a tool-run use case the refusal is UNCHANGED, word for word.
func TestStartWithoutAToolRunUseCaseKeepsTheRefusal(t *testing.T) {
	handler, err := NewStartHandler(&unusedStartUseCase{})
	if err != nil {
		t.Fatal(err)
	}
	recorder := httptest.NewRecorder()
	handler.Start(recorder, newStartRequest(t, "", toolRunStartBody, true))
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status %d, want 400", recorder.Code)
	}
	if !strings.Contains(recorder.Body.String(), "Only asynchronous index_data admission is supported") {
		t.Fatalf("the historical refusal changed: %s", recorder.Body.String())
	}
}

func TestStartToolRunRefusesAnUnauthenticatedCaller(t *testing.T) {
	runs := &toolRunStub{}
	handler, err := NewStartHandlerWithToolRuns(&unusedStartUseCase{}, runs)
	if err != nil {
		t.Fatal(err)
	}
	recorder := httptest.NewRecorder()
	handler.Start(recorder, newStartRequest(t, "", toolRunStartBody, false))
	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("status %d, want 401", recorder.Code)
	}
	if runs.calls != 0 {
		t.Fatal("an unauthenticated caller reached the tool-run use case")
	}
}

func TestStartToolRunAnswers504WhenTheBoundedWaitExpires(t *testing.T) {
	runs := &toolRunStub{err: &toolkitcalltoolapp.PendingRun{ExecutionID: "exec-9"}}
	handler, err := NewStartHandlerWithToolRuns(&unusedStartUseCase{}, runs)
	if err != nil {
		t.Fatal(err)
	}
	recorder := httptest.NewRecorder()
	handler.Start(recorder, newStartRequest(t, "", toolRunStartBody, true))
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

func TestStartToolRunRefusesANonJSONContentType(t *testing.T) {
	runs := &toolRunStub{}
	handler, err := NewStartHandlerWithToolRuns(&unusedStartUseCase{}, runs)
	if err != nil {
		t.Fatal(err)
	}
	request := newStartRequest(t, "", toolRunStartBody, true)
	request.Header.Set("Content-Type", "text/plain")
	recorder := httptest.NewRecorder()
	handler.Start(recorder, request)
	if recorder.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("status %d, want 415", recorder.Code)
	}
	if runs.calls != 0 {
		t.Fatal("a non-JSON body reached the tool-run use case")
	}
}
