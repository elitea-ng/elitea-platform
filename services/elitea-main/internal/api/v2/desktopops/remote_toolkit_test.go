package desktopops

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	executionapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/execution"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/localturn"
	toolkitcalltoolapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/toolkitcalltool"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/audit"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth"
	executiondomain "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/execution"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/guardrails"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/failurelimit"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/storage"
)

const (
	remoteExecution = "0123456789abcdef0123456789abcdef"
	remoteRef       = "tkr1_00112233445566778899aabbccddeeff"
)

// remoteBody is a well-formed call naming the live turn, the version it runs
// and the toolkit reference, plus whatever extra fields a test adds.
func remoteBody(extra string) string {
	body := `"execution_id":"` + remoteExecution + `","application_id":11,"version_id":12,"toolkit_ref":"` + remoteRef + `"`
	if extra != "" {
		body += "," + extra
	}
	return "{" + body + "}"
}

type fakeTurns struct {
	turn       localturn.LiveTurn
	err        error
	calls      int
	credential localturn.Credential
}

func (f *fakeTurns) Live(_ context.Context, _, _ int64, credential localturn.Credential, executionID string) (localturn.LiveTurn, error) {
	f.calls++
	f.credential = credential
	if f.err != nil {
		return localturn.LiveTurn{}, f.err
	}
	turn := f.turn
	turn.ExecutionID = executionID
	return turn, nil
}

type fakeAuthorizer struct {
	grant   storage.RemoteToolGrant
	err     error
	request storage.RemoteToolAuthorization
	calls   int
}

func (f *fakeAuthorizer) AuthorizeRemoteTool(_ context.Context, request storage.RemoteToolAuthorization) (storage.RemoteToolGrant, error) {
	f.calls++
	f.request = request
	return f.grant, f.err
}

type recordedAudit struct {
	mu     sync.Mutex
	events []audit.Event
}

func (r *recordedAudit) Record(_ context.Context, event audit.Event) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, event)
}

// remoteHandler is a handler whose turn is live (agent 11, version 12) and
// whose authorizer grants a non-sensitive github tool, unless a test swaps
// either out.
func remoteHandler(runs RemoteToolkitUseCase, worker string) (*remoteToolkitHandler, *fakeTurns, *fakeAuthorizer, *recordedAudit) {
	turns := &fakeTurns{turn: localturn.LiveTurn{ApplicationID: 11, VersionID: 12}}
	authorizer := &fakeAuthorizer{grant: storage.RemoteToolGrant{
		ToolkitType: "github", ToolkitName: "gh",
		LLMModel: "version-model", LLMSettings: json.RawMessage(`{"temperature":0.2}`),
	}}
	recorder := &recordedAudit{}
	h := newRemoteToolkitHandler(RemoteToolkitDependencies{
		Runs: runs, Worker: worker, Authorizer: authorizer, Turns: turns, Audit: recorder,
	})
	return h, turns, authorizer, recorder
}

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
	h, turns, authorizer, recorder := remoteHandler(runs, "python")
	response := serveWith(t, http.MethodPost, RemoteToolkitPath, remoteURL,
		remoteBody(`"tool_name":"create_issue","arguments":{"title":"x"},"request_id":"r1"`), desktopToken(), h.serve)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", response.Code, response.Body)
	}
	got := runs.request
	if got.ProjectID != 3 || got.ToolkitID != 61 || got.ActorUserID != 7 || got.ToolName != "create_issue" ||
		string(got.Arguments) != `{"title":"x"}` || got.RequestID != "r1" || got.SensitiveApproval != nil ||
		!got.EnforceSensitiveGate ||
		got.LLMModel != "version-model" || string(got.LLMSettings) != `{"temperature":0.2}` {
		t.Fatalf("run request = %+v", got)
	}
	// The turn is read for the caller's credential family (a native session's
	// anchor token and client), which must be the one that started it.
	if turns.credential != (localturn.Credential{TokenID: "70", NativeClientID: "ai.elitea.desktop"}) {
		t.Fatalf("turn read with credential %+v", turns.credential)
	}
	// The authorization is asked of the TURN's agent, for the version and
	// reference the call names.
	if turns.calls != 1 || authorizer.request != (storage.RemoteToolAuthorization{
		ProjectID: 3, ActorID: 7, TurnApplicationID: 11, TurnVersionID: 12, ApplicationID: 11, VersionID: 12,
		ToolkitID: 61, ToolkitRef: remoteRef, ToolName: "create_issue",
	}) {
		t.Fatalf("authorization = %+v (turn reads %d)", authorizer.request, turns.calls)
	}
	if len(recorder.events) != 1 {
		t.Fatalf("audit events = %d, want 1", len(recorder.events))
	}
	event := recorder.events[0]
	if event.EntityType != remoteToolkitAuditEntity || *event.EntityID != 61 || event.EntityName != "create_issue" ||
		*event.UserID != 7 || *event.ProjectID != 3 || *event.StatusCode != http.StatusOK ||
		!strings.Contains(event.Action, remoteExecution) || !strings.Contains(event.Action, `"github.create_issue": ok`) ||
		!strings.Contains(event.Action, "confirmation none") || !strings.Contains(event.Action, "arguments sha256 ") ||
		strings.Contains(event.Action, "title") {
		t.Fatalf("audit event = %+v", event)
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
	h, _, _, _ := remoteHandler(runs, "")
	session := &auth.User{ID: "7", UserID: "7", AuthType: "session"}
	response := serveWith(t, http.MethodPost, RemoteToolkitPath, remoteURL, remoteBody(`"tool_name":"t"`), session, h.serve)
	if response.Code != http.StatusForbidden || !strings.Contains(response.Body.String(), remoteToolkitRequiresToken) || runs.calls != 0 {
		t.Fatalf("status = %d, body %s, calls %d", response.Code, response.Body, runs.calls)
	}
}

func TestRemoteToolkitWithoutAWorkerAnswers501(t *testing.T) {
	for name, deps := range map[string]RemoteToolkitDependencies{
		"no worker":      {Authorizer: &fakeAuthorizer{}, Turns: &fakeTurns{}},
		"no agent plane": {Runs: &fakeToolRuns{}, Turns: &fakeTurns{}},
		"no local turns": {Runs: &fakeToolRuns{}, Authorizer: &fakeAuthorizer{}},
	} {
		deps.Audit = &recordedAudit{}
		h := newRemoteToolkitHandler(deps)
		response := serveWith(t, http.MethodPost, RemoteToolkitPath, remoteURL, remoteBody(`"tool_name":"t"`), desktopToken(), h.serve)
		if response.Code != http.StatusNotImplemented || !strings.Contains(response.Body.String(), "remote_toolkit_unavailable") {
			t.Fatalf("%s: status = %d, body %s", name, response.Code, response.Body)
		}
	}
}

func TestRemoteToolkitBoundsTheRequest(t *testing.T) {
	runs := &fakeToolRuns{}
	h, _, _, _ := remoteHandler(runs, "")
	for name, tc := range map[string]struct {
		body   string
		status int
	}{
		"no tool name":          {remoteBody(`"arguments":{}`), http.StatusBadRequest},
		"arguments not object":  {remoteBody(`"tool_name":"t","arguments":[1]`), http.StatusBadRequest},
		"two values":            {remoteBody(`"tool_name":"t"`) + ` {}`, http.StatusBadRequest},
		"too large":             {remoteBody(`"tool_name":"t","arguments":{"x":"` + strings.Repeat("a", 1<<20) + `"}`), http.StatusRequestEntityTooLarge},
		"no execution id":       {`{"application_id":11,"version_id":12,"toolkit_ref":"` + remoteRef + `","tool_name":"t"}`, http.StatusBadRequest},
		"no version":            {`{"execution_id":"` + remoteExecution + `","application_id":11,"toolkit_ref":"` + remoteRef + `","tool_name":"t"}`, http.StatusBadRequest},
		"no toolkit ref":        {`{"execution_id":"` + remoteExecution + `","application_id":11,"version_id":12,"tool_name":"t"}`, http.StatusBadRequest},
		"malformed ref":         {`{"execution_id":"` + remoteExecution + `","application_id":11,"version_id":12,"toolkit_ref":"tkr1_x","tool_name":"t"}`, http.StatusBadRequest},
		"refused confirmation":  {remoteBody(`"tool_name":"t","confirmation":{"approved":false,"approved_at":"2026-10-08T10:00:00Z"}`), http.StatusBadRequest},
		"undated confirmation":  {remoteBody(`"tool_name":"t","confirmation":{"approved":true}`), http.StatusBadRequest},
		"caller-chosen model":   {remoteBody(`"tool_name":"t","llm_model":"expensive-model"`), http.StatusBadRequest},
		"caller model settings": {remoteBody(`"tool_name":"t","llm_settings":{"temperature":1}`), http.StatusBadRequest},
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
		h, _, _, _ := remoteHandler(&fakeToolRuns{outcome: tc.outcome}, tc.worker)
		response := serveWith(t, http.MethodPost, RemoteToolkitPath, remoteURL, remoteBody(`"tool_name":"t"`), desktopToken(), h.serve)
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
		h, _, _, _ := remoteHandler(&fakeToolRuns{err: tc.err}, "")
		response := serveWith(t, http.MethodPost, RemoteToolkitPath, remoteURL, remoteBody(`"tool_name":"t"`), desktopToken(), h.serve)
		if response.Code != tc.status || !strings.Contains(response.Body.String(), `"`+tc.code+`"`) {
			t.Errorf("%s: status = %d, body %s", name, response.Code, response.Body)
		}
	}
}

func TestRemoteToolkitRouteRequiresTheCredentialPlane(t *testing.T) {
	if _, err := NewRemoteToolkitRoute(RemoteToolkitDependencies{Runs: &fakeToolRuns{}, Audit: &recordedAudit{}}, apimwZero(), nil); !errors.Is(err, ErrInvalidRoute) {
		t.Fatalf("err = %v", err)
	}
}

// The turn the call names must be live; each refusal is the local turn
// route's own answer, and nothing is authorized or run.
func TestRemoteToolkitRequiresALiveLocalTurn(t *testing.T) {
	for name, tc := range map[string]struct {
		err    error
		status int
		code   string
	}{
		"local work off":        {localturn.ErrLocalWorkDisabled, http.StatusForbidden, "local_work_disabled"},
		"not the caller's turn": {localturn.ErrNotFound, http.StatusNotFound, "local_turn_not_found"},
		"committed":             {localturn.ErrAlreadyCommitted, http.StatusConflict, "local_turn_already_committed"},
		"expired":               {localturn.ErrExpired, http.StatusGone, "local_turn_expired"},
		"policy unreadable":     {localturn.ErrUnavailable, http.StatusServiceUnavailable, "remote_toolkit_unavailable_now"},
	} {
		runs := &fakeToolRuns{}
		h, turns, authorizer, recorder := remoteHandler(runs, "python")
		turns.err = tc.err
		response := serveWith(t, http.MethodPost, RemoteToolkitPath, remoteURL, remoteBody(`"tool_name":"t"`), desktopToken(), h.serve)
		if response.Code != tc.status || !strings.Contains(response.Body.String(), `"`+tc.code+`"`) ||
			authorizer.calls != 0 || runs.calls != 0 {
			t.Errorf("%s: status = %d, body %s, authorizer %d, runs %d", name, response.Code, response.Body, authorizer.calls, runs.calls)
		}
		if len(recorder.events) != 1 || *recorder.events[0].StatusCode != int32(tc.status) {
			t.Errorf("%s: a refused call is audited too: %+v", name, recorder.events)
		}
	}
}

// What the agent cannot call in a chat turn is refused here, before any run.
func TestRemoteToolkitRefusesWhatTheAgentCannotCall(t *testing.T) {
	for name, tc := range map[string]struct {
		err    error
		status int
		code   string
	}{
		"not in the agent": {storage.ErrRemoteToolNotInAgent, http.StatusForbidden, "tool_not_in_agent"},
		"blocked":          {storage.ErrRemoteToolBlocked, http.StatusForbidden, "tool_blocked"},
		"ref mismatch":     {storage.ErrRemoteToolkitRefMismatch, http.StatusForbidden, "toolkit_ref_mismatch"},
		"unresolvable":     {storage.ErrClientApplicationVersionUnresolvable, http.StatusUnprocessableEntity, "application_version_unresolvable"},
		"dependency":       {storage.ErrContentUnavailable, http.StatusServiceUnavailable, "remote_toolkit_unavailable_now"},
	} {
		runs := &fakeToolRuns{}
		h, _, authorizer, _ := remoteHandler(runs, "python")
		authorizer.err = tc.err
		response := serveWith(t, http.MethodPost, RemoteToolkitPath, remoteURL, remoteBody(`"tool_name":"t"`), desktopToken(), h.serve)
		if response.Code != tc.status || !strings.Contains(response.Body.String(), `"`+tc.code+`"`) || runs.calls != 0 {
			t.Errorf("%s: status = %d, body %s, runs %d", name, response.Code, response.Body, runs.calls)
		}
	}
}

// A sensitive tool pauses exactly as a chat turn does: without a confirmation
// the answer is the HITL interrupt; with one, the approval travels to the
// worker and is audited.
func TestRemoteToolkitSensitiveToolNeedsAConfirmation(t *testing.T) {
	sensitive := &guardrails.SensitiveAction{ActionLabel: "gh.delete_file", PolicyMessage: "Acme requires approval before running the sensitive action 'gh.delete_file'."}
	runs := &fakeToolRuns{outcome: toolkitcalltoolapp.RunOutcome{Status: toolkitcalltoolapp.RunStatusOK, ResultJSON: `{}`}}
	h, _, authorizer, recorder := remoteHandler(runs, "python")
	authorizer.grant.Sensitive = sensitive
	fixed := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
	h.now = func() time.Time { return fixed }

	response := serveWith(t, http.MethodPost, RemoteToolkitPath, remoteURL, remoteBody(`"tool_name":"delete_file","arguments":{"path":"a"}`), desktopToken(), h.serve)
	if response.Code != http.StatusConflict || runs.calls != 0 {
		t.Fatalf("status = %d, runs %d, body %s", response.Code, runs.calls, response.Body)
	}
	var body struct {
		Error     string         `json:"error"`
		Interrupt map[string]any `json:"hitl_interrupt"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	interrupt := body.Interrupt
	if body.Error != "confirmation_required" || interrupt["guardrail_type"] != "sensitive_tool" ||
		interrupt["action_label"] != "gh.delete_file" || interrupt["policy_message"] != sensitive.PolicyMessage ||
		interrupt["tool_name"] != "delete_file" || interrupt["toolkit_name"] != "gh" || interrupt["toolkit_type"] != "github" ||
		!strings.HasPrefix(interrupt["interrupt_id"].(string), "hitl_") || interrupt["tool_args"] != nil {
		t.Fatalf("confirmation body = %s", response.Body)
	}

	response = serveWith(t, http.MethodPost, RemoteToolkitPath, remoteURL,
		remoteBody(`"tool_name":"delete_file","arguments":{"path":"a"},"confirmation":{"approved":true,"approved_at":"2026-10-08T11:59:30Z"}`),
		desktopToken(), h.serve)
	if response.Code != http.StatusOK || runs.calls != 1 {
		t.Fatalf("confirmed: status = %d, runs %d, body %s", response.Code, runs.calls, response.Body)
	}
	approval := runs.request.SensitiveApproval
	if approval == nil || approval.Source != toolkitcalltoolapp.ApprovalSourceUserConfirmation || approval.ApprovedAt != "2026-10-08T11:59:30Z" {
		t.Fatalf("approval = %+v", approval)
	}
	last := recorder.events[len(recorder.events)-1]
	if !strings.Contains(last.Action, "confirmation approved_at 2026-10-08T11:59:30Z") {
		t.Fatalf("audit action = %q", last.Action)
	}

	// An approval far in the past (older than a turn can live) or from the
	// future is not one.
	for _, at := range []string{"2026-10-06T11:59:30Z", "2026-10-08T13:00:00Z"} {
		response = serveWith(t, http.MethodPost, RemoteToolkitPath, remoteURL,
			remoteBody(`"tool_name":"delete_file","confirmation":{"approved":true,"approved_at":"`+at+`"}`), desktopToken(), h.serve)
		if response.Code != http.StatusBadRequest {
			t.Fatalf("approved_at %s: status = %d", at, response.Code)
		}
	}
}

func TestRemoteToolkitRateLimitsEachCaller(t *testing.T) {
	runs := &fakeToolRuns{outcome: toolkitcalltoolapp.RunOutcome{Status: toolkitcalltoolapp.RunStatusOK, ResultJSON: `{}`}}
	h, _, _, _ := remoteHandler(runs, "python")
	h.limiter = failurelimit.New(2, time.Minute)
	for i := 0; i < 2; i++ {
		if response := serveWith(t, http.MethodPost, RemoteToolkitPath, remoteURL, remoteBody(`"tool_name":"t"`), desktopToken(), h.serve); response.Code != http.StatusOK {
			t.Fatalf("call %d: status = %d", i, response.Code)
		}
	}
	response := serveWith(t, http.MethodPost, RemoteToolkitPath, remoteURL, remoteBody(`"tool_name":"t"`), desktopToken(), h.serve)
	if response.Code != http.StatusTooManyRequests || response.Header().Get("Retry-After") == "" || runs.calls != 2 {
		t.Fatalf("third call: status = %d, runs %d", response.Code, runs.calls)
	}
	other := &auth.User{ID: "8", UserID: "8", TokenID: "80", AuthType: "token"}
	if response := serveWith(t, http.MethodPost, RemoteToolkitPath, remoteURL, remoteBody(`"tool_name":"t"`), other, h.serve); response.Code != http.StatusOK {
		t.Fatalf("another caller: status = %d", response.Code)
	}
}

// A write may be retried after a timeout: the key is required, and a retry
// of a key whose run is still going answers 409 in progress, never a second
// run.
func TestRemoteToolkitRequiresAnIdempotencyKeyAndNeverRunsAReplayTwice(t *testing.T) {
	runs := &fakeToolRuns{}
	h, _, _, _ := remoteHandler(runs, "python")
	router := func(key string) *httptest.ResponseRecorder {
		request := httptest.NewRequest(http.MethodPost, remoteURL, strings.NewReader(remoteBody(`"tool_name":"t"`)))
		request.Header.Set("Content-Type", "application/json")
		if key != "" {
			request.Header.Set("Idempotency-Key", key)
		}
		request = request.WithContext(auth.ContextWithUser(request.Context(), *desktopToken()))
		mux := chi.NewRouter()
		mux.Post(RemoteToolkitPath, h.serve)
		recorder := httptest.NewRecorder()
		mux.ServeHTTP(recorder, request)
		return recorder
	}
	for _, key := range []string{"", "has space", strings.Repeat("k", 129)} {
		if response := router(key); response.Code != http.StatusBadRequest ||
			!strings.Contains(response.Body.String(), "idempotency_key_required") {
			t.Fatalf("key %q: status = %d, body %s", key, response.Code, response.Body)
		}
	}
	if runs.calls != 0 {
		t.Fatalf("a keyless call reached the use case %d times", runs.calls)
	}

	runs.err = &toolkitcalltoolapp.PendingRun{ExecutionID: "e1"}
	response := router("call-1")
	if response.Code != http.StatusGatewayTimeout || !strings.Contains(response.Body.String(), "SAME Idempotency-Key") {
		t.Fatalf("first wait: status = %d, body %s", response.Code, response.Body)
	}
	if runs.request.IdempotencyKey != "call-1" {
		t.Fatalf("the key did not reach the admission: %+v", runs.request)
	}
	runs.err = &toolkitcalltoolapp.PendingRun{ExecutionID: "e1", Replayed: true}
	response = router("call-1")
	if response.Code != http.StatusConflict || !strings.Contains(response.Body.String(), "remote_toolkit_in_progress") ||
		!strings.Contains(response.Body.String(), `"task_id":"e1"`) {
		t.Fatalf("replay: status = %d, body %s", response.Code, response.Body)
	}
}
