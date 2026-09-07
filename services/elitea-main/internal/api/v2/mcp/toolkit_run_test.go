package mcp

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	toolkitcalltoolapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/toolkitcalltool"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth"
	"github.com/go-chi/chi/v5"
)

type recordingToolkitRun struct {
	outcome  toolkitcalltoolapp.RunOutcome
	err      error
	calls    int
	captured toolkitcalltoolapp.RunRequest
}

func (r *recordingToolkitRun) RunTool(
	_ context.Context, request toolkitcalltoolapp.RunRequest,
) (toolkitcalltoolapp.RunOutcome, error) {
	r.calls++
	r.captured = request
	return r.outcome, r.err
}

// runnableToolkitTool is what `toolkitTools` builds: the published MCP name is
// `<toolkit>_<tool>`, and the SDK tool name beside it is the one the worker is
// given.
func runnableToolkitTool() Tool {
	return Tool{
		Name:            "github_get_issue",
		Description:     "seeded",
		InputSchema:     toolkitToolSchema(),
		toolkitID:       19,
		toolkitToolName: "get_issue",
	}
}

func newToolkitRunRouter(
	t *testing.T, start AgentStartUseCase, runs ToolkitRunUseCase,
) chi.Router {
	t.Helper()
	handler := NewHandlerWithToolkitRuns(nil, nil, start, runs, allowRuns())
	handler.source = staticSource(runnableToolkitTool())
	router := chi.NewRouter()
	router.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			user := auth.User{ID: "7", UserID: "7"}
			next.ServeHTTP(w, r.WithContext(auth.ContextWithUser(r.Context(), user)))
		})
	})
	router.Post("/app/{projectID}/mcp", handler.Endpoint)
	return router
}

func callGetIssue(t *testing.T, router chi.Router, arguments string) map[string]any {
	t.Helper()
	return resultOf(t, post(t, router, "/app/7/mcp",
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"github_get_issue","arguments":`+
			arguments+`}}`))
}

// Without a tool-run use case the sentence is UNCHANGED. A deployment with no
// worker genuinely cannot run a toolkit tool.
func TestToolkitCallWithoutAUseCaseKeepsTheRefusal(t *testing.T) {
	router := newToolkitRunRouter(t, &recordingStart{}, nil)
	result := callGetIssue(t, router, `{}`)
	if result["isError"] != true {
		t.Fatalf("isError = %v", result["isError"])
	}
	if got := textOf(t, result); got != ToolkitExecutionUnavailableReason {
		t.Fatalf("text = %q,\nwant %q", got, ToolkitExecutionUnavailableReason)
	}
}

// The worker is given the SDK tool name, never the published MCP name: no
// toolkit has a tool called `github_get_issue`.
func TestToolkitCallDispatchesTheSDKToolNameAndToolkitID(t *testing.T) {
	runs := &recordingToolkitRun{outcome: toolkitcalltoolapp.RunOutcome{
		ExecutionID: "exec-1", Status: toolkitcalltoolapp.RunStatusOK, ResultJSON: `{"number":7}`,
	}}
	router := newToolkitRunRouter(t, &recordingStart{}, runs)
	result := callGetIssue(t, router, `{"issue":7}`)

	if runs.calls != 1 {
		t.Fatalf("use case called %d times", runs.calls)
	}
	if runs.captured.ToolName != "get_issue" {
		t.Fatalf("tool name %q, want the SDK name", runs.captured.ToolName)
	}
	if runs.captured.ToolkitID != 19 || runs.captured.ProjectID != 7 || runs.captured.ActorUserID != 7 {
		t.Fatalf("request %+v", runs.captured)
	}
	// The arguments pass through unchanged — the schema is an open object
	// because this service does not hold the SDK's per-tool argument schemas.
	var arguments map[string]any
	if err := json.Unmarshal(runs.captured.Arguments, &arguments); err != nil {
		t.Fatalf("arguments %q: %v", runs.captured.Arguments, err)
	}
	if arguments["issue"] != float64(7) {
		t.Fatalf("arguments lost their content: %v", arguments)
	}
	if result["isError"] == true {
		t.Fatalf("a successful run reported isError: %v", result)
	}
	if got := textOf(t, result); got != `{"number":7}` {
		t.Fatalf("text = %q", got)
	}
}

// An absent `arguments` member is an empty object, not a refusal: a tool with
// no arguments is ordinary.
func TestToolkitCallWithNoArgumentsSendsAnEmptyObject(t *testing.T) {
	runs := &recordingToolkitRun{outcome: toolkitcalltoolapp.RunOutcome{
		ExecutionID: "exec-1", Status: toolkitcalltoolapp.RunStatusOK, ResultJSON: `"done"`,
	}}
	router := newToolkitRunRouter(t, &recordingStart{}, runs)
	resultOf(t, post(t, router, "/app/7/mcp",
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"github_get_issue"}}`))

	if string(runs.captured.Arguments) != `{}` {
		t.Fatalf("arguments %q, want an empty object", runs.captured.Arguments)
	}
}

func TestToolkitCallMapsEveryOutcomeOntoAnMCPResult(t *testing.T) {
	cases := []struct {
		name     string
		outcome  toolkitcalltoolapp.RunOutcome
		err      error
		isError  bool
		contains string
	}{
		{
			name:    "ok",
			outcome: toolkitcalltoolapp.RunOutcome{ExecutionID: "e1", Status: toolkitcalltoolapp.RunStatusOK, ResultJSON: `{"a":1}`},
			isError: false, contains: `{"a":1}`,
		},
		{
			// An isError RESULT, not a protocol error: the request was fine and
			// the tool did not deliver.
			name: "the tool raised",
			outcome: toolkitcalltoolapp.RunOutcome{
				ExecutionID: "e2", Status: toolkitcalltoolapp.RunStatusToolError,
				ErrorMessage: "401 from the provider",
			},
			isError: true, contains: "401 from the provider",
		},
		{
			name: "an unsupported toolkit",
			outcome: toolkitcalltoolapp.RunOutcome{
				ExecutionID: "e3", Status: toolkitcalltoolapp.RunStatusUnsupportedToolkit,
				ErrorMessage: "the image does not carry github",
			},
			isError: true, contains: "Nothing was executed",
		},
		{
			name: "an unknown tool",
			outcome: toolkitcalltoolapp.RunOutcome{
				ExecutionID: "e4", Status: toolkitcalltoolapp.RunStatusUnknownTool,
			},
			isError: true, contains: "no tool by that name",
		},
		{
			// A truncated result is NOT reported as an empty success: an agent
			// host would read that as "the tool produced nothing".
			name: "a truncated result",
			outcome: toolkitcalltoolapp.RunOutcome{
				ExecutionID: "e5", Status: toolkitcalltoolapp.RunStatusOK, Truncated: true,
			},
			isError: true, contains: "too large",
		},
		{
			name:    "a run that never settled",
			err:     &toolkitcalltoolapp.PendingRun{ExecutionID: "e6"},
			isError: true, contains: "STILL RUNNING as execution e6",
		},
		{
			name:    "a toolkit that vanished between list and call",
			err:     toolkitcalltoolapp.ErrToolkitNotVisible,
			isError: true, contains: "no longer visible",
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			runs := &recordingToolkitRun{outcome: testCase.outcome, err: testCase.err}
			router := newToolkitRunRouter(t, &recordingStart{}, runs)
			result := callGetIssue(t, router, `{}`)
			if testCase.isError && result["isError"] != true {
				t.Fatalf("isError = %v, want true (%v)", result["isError"], result)
			}
			if !testCase.isError && result["isError"] == true {
				t.Fatalf("isError = true, want false (%v)", result)
			}
			if !strings.Contains(textOf(t, result), testCase.contains) {
				t.Fatalf("text = %q, want it to contain %q", textOf(t, result), testCase.contains)
			}
		})
	}
}

// Running a toolkit tool spends the project's budget and drives its
// credentials, so it takes the SAME per-call permission an agent run takes.
func TestToolkitCallWithoutTheRunPermissionRefuses(t *testing.T) {
	runs := &recordingToolkitRun{}
	handler := NewHandlerWithToolkitRuns(nil, nil, &recordingStart{}, runs,
		&fakePermissions{granted: []string{"models.chat.conversations.list"}, userID: 7})
	handler.source = staticSource(runnableToolkitTool())
	router := chi.NewRouter()
	router.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			user := auth.User{ID: "7", UserID: "7"}
			next.ServeHTTP(w, r.WithContext(auth.ContextWithUser(r.Context(), user)))
		})
	})
	router.Post("/app/{projectID}/mcp", handler.Endpoint)

	result := callGetIssue(t, router, `{}`)
	if result["isError"] != true {
		t.Fatalf("isError = %v", result["isError"])
	}
	if !strings.Contains(textOf(t, result), runPermission) {
		t.Fatalf("text = %q, want it to name the required permission", textOf(t, result))
	}
	if runs.calls != 0 {
		t.Fatalf("a caller without the permission reached the use case %d times", runs.calls)
	}
}

// The toolkit half is decided BEFORE the agent guard: a deployment that can run
// toolkits and not agents must not be told the agent's sentence.
func TestToolkitCallDoesNotDependOnTheAgentHalf(t *testing.T) {
	runs := &recordingToolkitRun{outcome: toolkitcalltoolapp.RunOutcome{
		ExecutionID: "e1", Status: toolkitcalltoolapp.RunStatusOK, ResultJSON: `"ok"`,
	}}
	router := newToolkitRunRouter(t, nil, runs)
	result := callGetIssue(t, router, `{}`)
	if result["isError"] == true {
		t.Fatalf("a toolkit run was refused because the agent half is absent: %v", result)
	}
	if runs.calls != 1 {
		t.Fatalf("use case called %d times", runs.calls)
	}
}

// A descriptor that names neither a runnable agent nor a runnable toolkit tool
// keeps the refusal. This is the shape a listing built before the toolkit
// identity existed would produce.
func TestCallOfADescriptorWithNoTargetKeepsTheRefusal(t *testing.T) {
	handler := NewHandlerWithToolkitRuns(nil, nil, &recordingStart{}, &recordingToolkitRun{}, allowRuns())
	handler.source = staticSource(Tool{Name: "github_get_issue"})
	router := chi.NewRouter()
	router.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			user := auth.User{ID: "7", UserID: "7"}
			next.ServeHTTP(w, r.WithContext(auth.ContextWithUser(r.Context(), user)))
		})
	})
	router.Post("/app/{projectID}/mcp", handler.Endpoint)

	result := callGetIssue(t, router, `{}`)
	if got := textOf(t, result); got != ToolkitExecutionUnavailableReason {
		t.Fatalf("text = %q,\nwant %q", got, ToolkitExecutionUnavailableReason)
	}
}

// The two predicates are mutually exclusive: each listing query populates one
// pair of fields and never the other.
func TestRunnablePredicatesAreExclusive(t *testing.T) {
	agent := agentTool("my_agent", 41, 99)
	if !agent.runnableAgent() || agent.runnableToolkitTool() {
		t.Fatalf("an agent descriptor answers both predicates: %+v", agent)
	}
	toolkit := runnableToolkitTool()
	if toolkit.runnableAgent() || !toolkit.runnableToolkitTool() {
		t.Fatalf("a toolkit descriptor answers both predicates: %+v", toolkit)
	}
}
