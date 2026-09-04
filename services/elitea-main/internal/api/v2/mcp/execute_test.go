package mcp

// Unit tests for the execution half of `tools/call`.
//
// Three things are pinned here that no integration test can pin as cheaply:
//
//   - the WIRE SHAPE of Tool, which the target discriminator must not change;
//   - the nil-runtime refusal, which must stay the sentence it always was;
//   - every branch of turnState, including the two pauses, which a real run
//     would only reach by arranging a worker to pause.
//
// The path that actually admits a turn is asserted against a real database in
// execute_postgres_integration_test.go: a fake pool here would prove nothing
// about the rows the runtime resolver joins.

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	agentexecutionapi "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/agentexecution"

	agentexecutionapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/agentexecution"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth"
)

/* ── harness ───────────────────────────────────────────────────────────── */

// agentTool is a descriptor as the catalog would build one for an agent: with
// the unexported target populated. Tests that want a TOOLKIT tool use a bare
// Tool, which is what toolkitTools produces.
func agentTool(name string, applicationID, versionID int64) Tool {
	return Tool{
		Name:                 name,
		Description:          "seeded",
		InputSchema:          agentTaskSchema(),
		applicationID:        applicationID,
		applicationVersionID: versionID,
	}
}

// recordingStart is a start use case that records the request and answers with
// a canned outcome. It never touches a database, so any test using it must
// arrange for the handler to fail BEFORE the admission (nil pool) or must be an
// integration test.
type recordingStart struct {
	request agentexecutionapp.CurrentApplicationStartRequest
	calls   int
	outcome agentexecutionapp.CurrentApplicationStartOutcome
	err     error
}

func (s *recordingStart) StartCurrentApplication(
	_ context.Context, request agentexecutionapp.CurrentApplicationStartRequest,
) (agentexecutionapp.CurrentApplicationStartOutcome, error) {
	s.calls++
	s.request = request
	return s.outcome, s.err
}

// fakePermissions answers with a fixed permission set, or with an error.
type fakePermissions struct {
	granted []string
	userID  int64
	err     error
	calls   int
}

func (p *fakePermissions) ResolvePermissions(
	_ context.Context, _ auth.User, _ string, _ string,
) (auth.PermissionResolution, error) {
	p.calls++
	if p.err != nil {
		return auth.PermissionResolution{}, p.err
	}
	return auth.PermissionResolution{UserID: p.userID, Permissions: p.granted}, nil
}

// allowRuns is the resolver a member who may start a chat turn gets.
func allowRuns() *fakePermissions {
	return &fakePermissions{granted: []string{runPermission}, userID: 7}
}

// newRunnableRouter builds a handler with a catalog, a start use case and a
// permissive resolver, and wraps the router so the request carries an
// authenticated user — the MCP endpoint's own middleware does that in
// production.
func newRunnableRouter(t *testing.T, source toolSource, start AgentStartUseCase) chi.Router {
	t.Helper()
	handler := NewHandler(nil, nil, start, allowRuns())
	handler.source = source
	router := chi.NewRouter()
	router.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			user := auth.User{ID: "7", UserID: "7"}
			next.ServeHTTP(w, r.WithContext(auth.ContextWithUser(r.Context(), user)))
		})
	})
	router.Post("/app/{projectID}/mcp", handler.Endpoint)
	router.Post("/app/{projectID}/mcp/*", handler.Endpoint)
	return router
}

// textOf reads the first content block, whether the result came back through a
// JSON round trip (`[]any`) or straight out of a function under test
// (`[]map[string]any`).
func textOf(t *testing.T, result map[string]any) string {
	t.Helper()
	var block map[string]any
	switch content := result["content"].(type) {
	case []any:
		if len(content) == 0 {
			t.Fatalf("content is empty: %v", result)
		}
		block, _ = content[0].(map[string]any)
	case []map[string]any:
		if len(content) == 0 {
			t.Fatalf("content is empty: %v", result)
		}
		block = content[0]
	default:
		t.Fatalf("content = %v, want at least one block", result["content"])
	}
	if block["type"] != "text" {
		t.Fatalf("content block type = %v, want text", block["type"])
	}
	text, _ := block["text"].(string)
	return text
}

/* ── step zero: the discriminator must not reach the wire ──────────────── */

// The target discriminator is the whole reason execution can resolve anything,
// and it MUST NOT be serialised: `tools/list` is a response every existing
// client has already parsed, and a new member would change it for all of them.
//
// The assertion is on the exact bytes rather than on "the extra keys are
// absent", because absence-shaped assertions are how a renamed field slips
// through: this fails if the marshalled document differs by so much as key
// order.
func TestToolWireShapeExcludesTargetDiscriminator(t *testing.T) {
	populated := Tool{
		Name:                 "my_agent",
		Description:          "writes release notes",
		InputSchema:          map[string]any{"type": "object"},
		applicationID:        41,
		applicationVersionID: 99,
		toolkitID:            73,
		toolkitToolName:      "get_issue",
	}
	bare := Tool{
		Name:        "my_agent",
		Description: "writes release notes",
		InputSchema: map[string]any{"type": "object"},
	}

	withTarget, err := json.Marshal(populated)
	if err != nil {
		t.Fatalf("marshal populated: %v", err)
	}
	withoutTarget, err := json.Marshal(bare)
	if err != nil {
		t.Fatalf("marshal bare: %v", err)
	}
	if string(withTarget) != string(withoutTarget) {
		t.Fatalf("a populated target changed the wire shape:\n got %s\nwant %s", withTarget, withoutTarget)
	}
	const want = `{"name":"my_agent","description":"writes release notes","inputSchema":{"type":"object"}}`
	if string(withTarget) != want {
		t.Fatalf("tool JSON = %s, want %s", withTarget, want)
	}
}

// And the listing served over the protocol carries the same three keys, so the
// guarantee holds through the handler and not only through json.Marshal.
func TestToolsListStillServesExactlyThreeKeys(t *testing.T) {
	router := newTestRouter(newTestHandler(t, staticSource(agentTool("my_agent", 41, 99))))
	result := resultOf(t, post(t, router, "/app/7/mcp", `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))

	tools, ok := result["tools"].([]any)
	if !ok || len(tools) != 1 {
		t.Fatalf("tools = %v, want one entry", result["tools"])
	}
	entry, _ := tools[0].(map[string]any)
	if len(entry) != 3 {
		t.Fatalf("tool has %d keys (%v), want exactly name/description/inputSchema", len(entry), entry)
	}
	for _, key := range []string{"name", "description", "inputSchema"} {
		if _, present := entry[key]; !present {
			t.Fatalf("tool is missing %q: %v", key, entry)
		}
	}
}

/* ── no runtime: the original refusal, unchanged ───────────────────────── */

// A deployment with `runtime.enabled` off hands this package a nil start use
// case, and nothing about what it can do has changed — so neither may the
// sentence. This asserts the CONSTANT is what comes back, for an agent tool
// (the half that now works when a runtime IS present), which is the case a
// regression would break first.
func TestCallWithoutRuntimeReturnsTheOriginalRefusalVerbatim(t *testing.T) {
	router := newTestRouter(newTestHandler(t, staticSource(agentTool("my_agent", 41, 99))))
	result := resultOf(t, post(t, router, "/app/7/mcp",
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"my_agent","arguments":{"task":"hi"}}}`))

	if result["isError"] != true {
		t.Fatalf("isError = %v, want true", result["isError"])
	}
	if got := textOf(t, result); got != ToolExecutionUnavailableReason {
		t.Fatalf("text = %q,\nwant %q", got, ToolExecutionUnavailableReason)
	}
}

// The nil check must come BEFORE the toolkit split, or a runtime-less
// deployment would answer a toolkit call with a sentence claiming agents can be
// run there.
func TestCallOfAToolkitToolWithoutRuntimeReturnsTheOriginalRefusal(t *testing.T) {
	router := newTestRouter(newTestHandler(t, staticSource(Tool{Name: "github_get_issue"})))
	result := resultOf(t, post(t, router, "/app/7/mcp",
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"github_get_issue","arguments":{}}}`))

	if got := textOf(t, result); got != ToolExecutionUnavailableReason {
		t.Fatalf("text = %q,\nwant the original refusal", got)
	}
}

/* ── with a runtime: the two halves separate ───────────────────────────── */

// A toolkit tool has no application behind it, so it is refused — but with the
// sentence that names ONLY the missing half, and it must not reach the start
// use case.
func TestCallOfAToolkitToolWithRuntimeRefusesOnlyTheToolkitHalf(t *testing.T) {
	start := &recordingStart{}
	router := newRunnableRouter(t, staticSource(Tool{Name: "github_get_issue"}), start)
	result := resultOf(t, post(t, router, "/app/7/mcp",
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"github_get_issue","arguments":{}}}`))

	if result["isError"] != true {
		t.Fatalf("isError = %v, want true", result["isError"])
	}
	if got := textOf(t, result); got != ToolkitExecutionUnavailableReason {
		t.Fatalf("text = %q,\nwant %q", got, ToolkitExecutionUnavailableReason)
	}
	if start.calls != 0 {
		t.Fatalf("start use case called %d times for a toolkit tool, want 0", start.calls)
	}
}

// An empty `task` is the REQUEST being wrong — the schema this server published
// marks it required — so it is a protocol error, and no turn is admitted.
func TestCallOfAnAgentWithoutATaskIsInvalidParams(t *testing.T) {
	start := &recordingStart{}
	router := newRunnableRouter(t, staticSource(agentTool("my_agent", 41, 99)), start)
	rpcError := errorOf(t, post(t, router, "/app/7/mcp",
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"my_agent","arguments":{"task":"   "}}}`))

	if rpcError["code"] != float64(codeInvalidParams) {
		t.Fatalf("code = %v, want %d", rpcError["code"], codeInvalidParams)
	}
	if start.calls != 0 {
		t.Fatalf("start use case called %d times for an empty task, want 0", start.calls)
	}
}

/* ── executing is authorized, listing is not ───────────────────────────── */

// routerWithPermissions is newRunnableRouter with the resolver under the test's
// control.
func routerWithPermissions(t *testing.T, start AgentStartUseCase, permissions auth.PermissionResolver) chi.Router {
	t.Helper()
	handler := NewHandler(nil, nil, start, permissions)
	handler.source = staticSource(agentTool("my_agent", 41, 99))
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

func callMyAgent(t *testing.T, router chi.Router) map[string]any {
	t.Helper()
	return resultOf(t, post(t, router, "/app/7/mcp",
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"my_agent","arguments":{"task":"hi"}}}`))
}

// The check that stops this endpoint being a way around the chat permission: a
// project MEMBER who may not create chat messages may not run an agent either,
// even though membership is all the route itself requires.
func TestCallOfAnAgentWithoutTheChatPermissionRefuses(t *testing.T) {
	start := &recordingStart{}
	permissions := &fakePermissions{granted: []string{"models.chat.conversations.list"}, userID: 7}
	result := callMyAgent(t, routerWithPermissions(t, start, permissions))

	if result["isError"] != true {
		t.Fatalf("isError = %v, want true", result["isError"])
	}
	if !strings.Contains(textOf(t, result), runPermission) {
		t.Fatalf("text = %q, want it to name the required permission", textOf(t, result))
	}
	if start.calls != 0 {
		t.Fatalf("start use case called %d times without the permission, want 0", start.calls)
	}
}

// It is the SAME permission the chat start route requires. Executing through
// MCP at a weaker tier than through chat is the defect this guards.
func TestMCPRunUsesTheChatStartPermission(t *testing.T) {
	if runPermission != agentexecutionapi.CurrentApplicationStartPermission {
		t.Fatalf("run permission = %q, want the chat start route's %q",
			runPermission, agentexecutionapi.CurrentApplicationStartPermission)
	}
	if runPermissionMode != agentexecutionapi.CurrentApplicationStartMode {
		t.Fatalf("run mode = %q, want the chat start route's %q",
			runPermissionMode, agentexecutionapi.CurrentApplicationStartMode)
	}
}

// A handler with no resolver cannot decide, and "cannot decide" must not mean
// "allowed" for the one capability here that spends money.
func TestCallOfAnAgentWithoutAResolverFailsClosed(t *testing.T) {
	start := &recordingStart{}
	result := callMyAgent(t, routerWithPermissions(t, start, nil))

	if result["isError"] != true {
		t.Fatalf("isError = %v, want true", result["isError"])
	}
	if start.calls != 0 {
		t.Fatalf("start use case called %d times with no resolver, want 0", start.calls)
	}
}

// A resolver that FAILS is an infrastructure fault, not a grant.
func TestCallOfAnAgentWhenThePermissionLookupFailsRefuses(t *testing.T) {
	start := &recordingStart{}
	permissions := &fakePermissions{err: errors.New("connection refused")}
	result := callMyAgent(t, routerWithPermissions(t, start, permissions))

	if result["isError"] != true {
		t.Fatalf("isError = %v, want true", result["isError"])
	}
	if start.calls != 0 {
		t.Fatalf("start use case called %d times after a failed lookup, want 0", start.calls)
	}
}

// A resolver that grants the permission but resolves no user id is a resolver
// bug; admitting a turn authored by user 0 would write chat rows nobody can be
// held to.
func TestCallOfAnAgentWithNoResolvedUserRefuses(t *testing.T) {
	start := &recordingStart{}
	permissions := &fakePermissions{granted: []string{runPermission}, userID: 0}
	result := callMyAgent(t, routerWithPermissions(t, start, permissions))

	if result["isError"] != true {
		t.Fatalf("isError = %v, want true", result["isError"])
	}
	if start.calls != 0 {
		t.Fatalf("start use case called %d times with no resolved user, want 0", start.calls)
	}
}

// LISTING is not authorized by this check. A viewer must still see the tool
// list — the permission gate is on execution only, which is why it lives in
// callTool and not on the route.
func TestToolsListIsNotGatedByTheRunPermission(t *testing.T) {
	permissions := &fakePermissions{granted: nil, userID: 7}
	router := routerWithPermissions(t, &recordingStart{}, permissions)
	result := resultOf(t, post(t, router, "/app/7/mcp", `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))

	tools, ok := result["tools"].([]any)
	if !ok || len(tools) != 1 {
		t.Fatalf("tools = %v, want the listing to be served regardless of the run permission", result["tools"])
	}
	if permissions.calls != 0 {
		t.Fatalf("tools/list resolved permissions %d times, want 0", permissions.calls)
	}
}

/* ── turnState: every terminal branch is honest ────────────────────────── */

func TestTurnStateResultReportsTheAnswer(t *testing.T) {
	result := turnState{settled: true, text: "the release notes"}.result(agentTool("my_agent", 1, 2), "exec-1")
	if _, present := result["isError"]; present {
		t.Fatalf("a completed run reported isError: %v", result)
	}
	if textOf(t, result) != "the release notes" {
		t.Fatalf("text = %q, want the agent's answer", textOf(t, result))
	}
}

// A failed run must be reported as failed, not waited out. The runtime's own
// safe message is relayed, so an operator sees the same reason the transcript
// shows.
func TestTurnStateResultReportsAFailure(t *testing.T) {
	result := turnState{settled: true, isError: true, failure: "model refused"}.
		result(agentTool("my_agent", 1, 2), "exec-1")
	if result["isError"] != true {
		t.Fatalf("isError = %v, want true", result["isError"])
	}
	text := textOf(t, result)
	if !strings.Contains(text, "model refused") || !strings.Contains(text, "exec-1") {
		t.Fatalf("text = %q, want the reason and the execution id", text)
	}
}

// Both pauses are TERMINAL and both are errors: MCP has no representation for
// "answer this and resume me", so waiting would burn the deadline on a run that
// cannot move on its own. Each must name the pause AND the execution id.
func TestTurnStateResultReportsEachPauseAsTerminal(t *testing.T) {
	for _, testCase := range []struct {
		name  string
		state turnState
		want  string
	}{
		{"hitl", turnState{settled: true, hitlPause: true}, "human approval"},
		{"authorization", turnState{settled: true, authorizationPause: true}, "MCP authorization"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			result := testCase.state.result(agentTool("my_agent", 1, 2), "exec-9")
			if result["isError"] != true {
				t.Fatalf("isError = %v, want true", result["isError"])
			}
			text := textOf(t, result)
			if !strings.Contains(text, testCase.want) {
				t.Fatalf("text = %q, want it to name %q", text, testCase.want)
			}
			if !strings.Contains(text, "exec-9") {
				t.Fatalf("text = %q, want it to name the execution id", text)
			}
		})
	}
}

// The rule this whole package lives by: never an empty successful result.
func TestTurnStateResultRefusesToReportAnEmptySuccess(t *testing.T) {
	result := turnState{settled: true, text: "   \n"}.result(agentTool("my_agent", 1, 2), "exec-1")
	if result["isError"] != true {
		t.Fatalf("a settled run with no text reported success: %v", result)
	}
	if !strings.Contains(textOf(t, result), "exec-1") {
		t.Fatalf("text = %q, want it to name the execution id", textOf(t, result))
	}
}

/* ── the deadline is real ──────────────────────────────────────────────── */

// A run that never settles must end in a bounded isError NAMING THE EXECUTION,
// not in a hung request and not in an empty success.
//
// The wait is driven through an already-cancelled context rather than by
// sleeping for mcpRunDeadline: the branch under test is the deadline branch,
// and `context.WithTimeout` on a dead parent fires immediately.
func TestAwaitRunResultIsBoundedAndNamesTheExecution(t *testing.T) {
	handler := NewHandler(nil, nil, &recordingStart{}, allowRuns())
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	result := handler.awaitRunResult(ctx, `"p_1"`,
		agentexecutionapp.CurrentApplicationStartOutcome{
			ExecutionID:       "exec-42",
			ResponseMessageID: "11111111-1111-4111-8111-111111111111",
		}, agentTool("my_agent", 1, 2))

	if result["isError"] != true {
		t.Fatalf("isError = %v, want true", result["isError"])
	}
	text := textOf(t, result)
	if !strings.Contains(text, "exec-42") {
		t.Fatalf("text = %q, want it to name the execution id", text)
	}
	if !strings.Contains(text, "STILL RUNNING") {
		t.Fatalf("text = %q, want it to say the run continues", text)
	}
}

// The deadline is a real duration and not a placeholder that would let a call
// hang until the client gives up.
func TestRunDeadlineIsBounded(t *testing.T) {
	if mcpRunDeadline <= 0 || mcpRunDeadline > 5*time.Minute {
		t.Fatalf("mcpRunDeadline = %s, want a bound inside what an MCP client waits", mcpRunDeadline)
	}
}

/* ── small helpers ─────────────────────────────────────────────────────── */

func TestRunConversationNameIsBounded(t *testing.T) {
	if got := runConversationName("short"); got != "MCP: short" {
		t.Fatalf("name = %q, want %q", got, "MCP: short")
	}
	long := runConversationName(strings.Repeat("é", 500))
	if count := len([]rune(long)); count != maxRunConversationName {
		t.Fatalf("name rune count = %d, want %d", count, maxRunConversationName)
	}
}

// A handler with no pool must refuse rather than dereference one, which is the
// composition fault this package already guards everywhere else.
func TestPrepareRunConversationWithoutAPoolRefuses(t *testing.T) {
	handler := NewHandler(nil, nil, &recordingStart{}, allowRuns())
	if _, _, err := handler.prepareRunConversation(
		context.Background(), `"p_1"`, 1, 7, agentTool("my_agent", 1, 2),
	); !errors.Is(err, errNoPool) {
		t.Fatalf("err = %v, want errNoPool", err)
	}
}
