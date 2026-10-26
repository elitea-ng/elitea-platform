package desktopops

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	agentexecutionapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/agentexecution"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/localturn"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/auth"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/failurelimit"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/storage"
)

const childStartURL = "/api/v2/elitea_core/local_child_turn/prompt_lib/3/" + remoteExecution

type fakeChildAuthorizer struct {
	err     error
	request storage.ChildAgentAuthorization
}

func (f *fakeChildAuthorizer) AuthorizeChildAgent(_ context.Context, request storage.ChildAgentAuthorization) (storage.ChildAgentGrant, error) {
	f.request = request
	return storage.ChildAgentGrant{Name: "Jira reader", AgentType: "agent"}, f.err
}

type fakeChildStarts struct {
	mu       sync.Mutex
	requests []agentexecutionapp.CurrentApplicationStartRequest
}

// StartCurrentApplication admits once per question id and replays after, as
// the start use case does.
func (f *fakeChildStarts) StartCurrentApplication(_ context.Context, request agentexecutionapp.CurrentApplicationStartRequest) (agentexecutionapp.CurrentApplicationStartOutcome, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	created := true
	for _, seen := range f.requests {
		if seen.QuestionID == request.QuestionID {
			created = false
		}
	}
	f.requests = append(f.requests, request)
	return agentexecutionapp.CurrentApplicationStartOutcome{
		ExecutionID: "child-exec-1", ResponseMessageID: "6f1c2b9e-0000-4000-8000-000000000001", Created: created,
	}, nil
}

// fakeChildStore keeps conversations by (parent, key) in memory.
type fakeChildStore struct {
	mu            sync.Mutex
	conversations map[string]*localturn.ChildConversation
	requests      []localturn.ChildConversationRequest
	recordErr     error
	state         localturn.ChildTurnState
	reads         []string
	discarded     []string
}

func (f *fakeChildStore) PrepareChildConversation(_ context.Context, request localturn.ChildConversationRequest) (localturn.ChildConversation, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests = append(f.requests, request)
	if f.conversations == nil {
		f.conversations = map[string]*localturn.ChildConversation{}
	}
	key := request.ParentExecutionID + "/" + request.IdempotencyKey
	if existing, ok := f.conversations[key]; ok {
		answer := *existing
		answer.Created = false
		return answer, nil
	}
	created := &localturn.ChildConversation{
		ConversationUUID: "0c0c0c0c-0000-4000-8000-000000000001", ParticipantID: 44,
		Created: true, InputSHA256: request.InputSHA256,
	}
	f.conversations[key] = created
	return *created, nil
}

func (f *fakeChildStore) RecordChildStart(_ context.Context, _ int64, conversationUUID, executionID, responseMessageID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.recordErr != nil {
		return f.recordErr
	}
	for _, conversation := range f.conversations {
		if conversation.ConversationUUID == conversationUUID {
			conversation.ExecutionID, conversation.ResponseMessageID = executionID, responseMessageID
		}
	}
	return nil
}

func (f *fakeChildStore) DiscardChildConversation(_ context.Context, _ int64, conversationUUID string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.discarded = append(f.discarded, conversationUUID)
}

func (f *fakeChildStore) ReadChildTurn(_ context.Context, _, _ int64, parent, child string) (localturn.ChildTurnState, error) {
	f.reads = append(f.reads, parent+"/"+child)
	if child != f.state.ExecutionID {
		return localturn.ChildTurnState{}, localturn.ErrChildTurnNotFound
	}
	return f.state, nil
}

func childHandler() (*childTurnHandler, *fakeTurns, *fakeChildAuthorizer, *fakeChildStarts, *fakeChildStore, *recordedAudit) {
	turns := &fakeTurns{turn: localturn.LiveTurn{ApplicationID: 11, VersionID: 12}}
	authorizer := &fakeChildAuthorizer{}
	starts := &fakeChildStarts{}
	store := &fakeChildStore{}
	recorder := &recordedAudit{}
	h := newChildTurnHandler(ChildTurnDependencies{
		Start: starts, Authorizer: authorizer, Turns: turns, Store: store, Audit: recorder,
	})
	return h, turns, authorizer, starts, store, recorder
}

func callChild(h *childTurnHandler, method, url, body string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(method, url, strings.NewReader(body))
	if body != "" {
		request.Header.Set("Content-Type", "application/json")
	}
	request = request.WithContext(auth.ContextWithUser(request.Context(), *desktopToken()))
	mux := chi.NewRouter()
	mux.Post(ChildTurnStartPath, h.start)
	mux.Get(ChildTurnReadPath, h.read)
	recorder := httptest.NewRecorder()
	mux.ServeHTTP(recorder, request)
	return recorder
}

func childBody(input, key string) string {
	return `{"application_id":21,"version_id":22,"input":"` + input + `","idempotency_key":"` + key + `"}`
}

func decodeChild(t *testing.T, response *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var body map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatalf("body %q: %v", response.Body.String(), err)
	}
	return body
}

// A child runs only under a live local turn of the caller: a committed,
// expired or foreign turn starts nothing.
func TestChildTurnRequiresLiveParentExecution(t *testing.T) {
	for name, tc := range map[string]struct {
		err    error
		status int
		code   string
	}{
		"committed": {localturn.ErrAlreadyCommitted, http.StatusConflict, "local_turn_already_committed"},
		"expired":   {localturn.ErrExpired, http.StatusGone, "local_turn_expired"},
		"foreign":   {localturn.ErrNotFound, http.StatusNotFound, "local_turn_not_found"},
		"disabled":  {localturn.ErrLocalWorkDisabled, http.StatusForbidden, "local_work_disabled"},
	} {
		h, turns, _, starts, store, _ := childHandler()
		turns.err = tc.err
		response := callChild(h, http.MethodPost, childStartURL, childBody("read PROJ-1", "k1"))
		if response.Code != tc.status || decodeChild(t, response)["error"] != tc.code {
			t.Fatalf("%s: %d %s", name, response.Code, response.Body.String())
		}
		if len(starts.requests) != 0 || len(store.requests) != 0 {
			t.Fatalf("%s: a refused child prepared or started something", name)
		}
	}

	// A browser session is refused before anything is read.
	h, turns, _, _, _, _ := childHandler()
	request := httptest.NewRequest(http.MethodPost, childStartURL, strings.NewReader(childBody("x", "k1")))
	request.Header.Set("Content-Type", "application/json")
	request = request.WithContext(auth.ContextWithUser(request.Context(), auth.User{ID: "7", UserID: "7", AuthType: "session"}))
	mux := chi.NewRouter()
	mux.Post(ChildTurnStartPath, h.start)
	recorder := httptest.NewRecorder()
	mux.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusForbidden || turns.calls != 0 {
		t.Fatalf("session caller: %d, turn reads %d", recorder.Code, turns.calls)
	}
}

// Only an agent the parent's version nests may run: the authorizer is asked
// with the turn's own agent, and its refusal is a 403 that starts nothing.
func TestChildTurnRefusesUnreferencedAgent(t *testing.T) {
	h, _, authorizer, starts, store, _ := childHandler()
	authorizer.err = storage.ErrChildAgentNotInAgent
	response := callChild(h, http.MethodPost, childStartURL, childBody("read PROJ-1", "k1"))
	if response.Code != http.StatusForbidden || decodeChild(t, response)["error"] != "child_agent_not_in_agent" {
		t.Fatalf("%d %s", response.Code, response.Body.String())
	}
	if authorizer.request.TurnApplicationID != 11 || authorizer.request.TurnVersionID != 12 ||
		authorizer.request.ApplicationID != 21 || authorizer.request.VersionID != 22 {
		t.Fatalf("authorization = %+v", authorizer.request)
	}
	if len(starts.requests) != 0 || len(store.requests) != 0 {
		t.Fatal("a refused agent prepared or started something")
	}
}

// One key is one child: a retry replays the admitted turn (no second start),
// and the same key with another input is a conflict.
func TestChildTurnIsIdempotentByKey(t *testing.T) {
	h, _, _, starts, _, _ := childHandler()
	first := callChild(h, http.MethodPost, childStartURL, childBody("read PROJ-1", "k1"))
	if first.Code != http.StatusAccepted {
		t.Fatalf("%d %s", first.Code, first.Body.String())
	}
	body := decodeChild(t, first)
	if body["child_execution_id"] != "child-exec-1" || body["created"] != true ||
		body["conversation_uuid"] != "0c0c0c0c-0000-4000-8000-000000000001" {
		t.Fatalf("body = %v", body)
	}
	if got := starts.requests[0]; got.TargetParticipantID != 44 || got.UserInput != "read PROJ-1" || got.QuestionID == "" {
		t.Fatalf("start = %+v", got)
	}

	retry := callChild(h, http.MethodPost, childStartURL, childBody("read PROJ-1", "k1"))
	if retry.Code != http.StatusAccepted || decodeChild(t, retry)["child_execution_id"] != "child-exec-1" {
		t.Fatalf("retry: %d %s", retry.Code, retry.Body.String())
	}
	if len(starts.requests) != 1 {
		t.Fatalf("a retry started again: %d starts", len(starts.requests))
	}

	conflict := callChild(h, http.MethodPost, childStartURL, childBody("delete PROJ-1", "k1"))
	if conflict.Code != http.StatusConflict || decodeChild(t, conflict)["error"] != "idempotency_conflict" {
		t.Fatalf("conflict: %d %s", conflict.Code, conflict.Body.String())
	}

	// The stamp was lost after the turn was admitted: the retry starts again
	// with the SAME question id, which the start use case replays.
	h2, _, _, starts2, store2, _ := childHandler()
	store2.recordErr = context.DeadlineExceeded
	if lost := callChild(h2, http.MethodPost, childStartURL, childBody("x", "k2")); lost.Code != http.StatusServiceUnavailable {
		t.Fatalf("lost stamp: %d", lost.Code)
	}
	store2.recordErr = nil
	if again := callChild(h2, http.MethodPost, childStartURL, childBody("x", "k2")); again.Code != http.StatusAccepted {
		t.Fatalf("after a lost stamp: %d %s", again.Code, again.Body.String())
	}
	if len(starts2.requests) != 2 || starts2.requests[0].QuestionID != starts2.requests[1].QuestionID {
		t.Fatalf("starts = %+v", starts2.requests)
	}
	if childQuestionID(3, remoteExecution, "k1") == childQuestionID(3, remoteExecution, "k2") {
		t.Fatal("two keys share a question id")
	}
}

// The child's usage is its own execution's (read back by its id), and the
// audit event of its start links that execution to the parent turn.
func TestChildUsageLinksToParent(t *testing.T) {
	h, _, _, _, store, recorder := childHandler()
	if response := callChild(h, http.MethodPost, childStartURL, childBody("read PROJ-1", "k1")); response.Code != http.StatusAccepted {
		t.Fatalf("%d %s", response.Code, response.Body.String())
	}
	if len(recorder.events) != 1 {
		t.Fatalf("audit events = %d", len(recorder.events))
	}
	action := recorder.events[0].Action
	if !strings.Contains(action, "parent execution "+remoteExecution) || !strings.Contains(action, "child execution child-exec-1") ||
		recorder.events[0].EntityType != childTurnAuditEntity {
		t.Fatalf("audit = %+v", recorder.events[0])
	}

	store.state = localturn.ChildTurnState{
		ExecutionID: "child-exec-1", ConversationUUID: "0c0c0c0c-0000-4000-8000-000000000001",
		Text: "PROJ-1 is open.", ToolCalls: []localturn.ChildToolCall{{Name: "get_issue"}},
		Usage: &localturn.ChildUsage{LLMCalls: 2, PromptTokens: 300, CompletionTokens: 40},
	}
	read := callChild(h, http.MethodGet, childStartURL+"/child-exec-1", "")
	if read.Code != http.StatusOK {
		t.Fatalf("%d %s", read.Code, read.Body.String())
	}
	body := decodeChild(t, read)
	usage, _ := body["usage"].(map[string]any)
	if body["status"] != "completed" || body["output"] != "PROJ-1 is open." || usage["prompt_tokens"] != float64(300) {
		t.Fatalf("read = %v", body)
	}
	if store.reads[0] != remoteExecution+"/child-exec-1" {
		t.Fatalf("reads = %v", store.reads)
	}
	if missing := callChild(h, http.MethodGet, childStartURL+"/other", ""); missing.Code != http.StatusNotFound {
		t.Fatalf("another child: %d", missing.Code)
	}
}

// A paused child relays what the person decides on, never the tool's
// arguments.
func TestChildTurnViewRelaysThePauseOnly(t *testing.T) {
	view := childTurnView(localturn.ChildTurnState{
		ExecutionID: "e", Interrupt: json.RawMessage(`{"interrupt_id":"i1","message":"Delete?","available_actions":["approve","reject"],"tool_args":{"token":"secret"}}`),
	})
	interrupt, _ := view["interrupt"].(map[string]any)
	if view["status"] != "interrupted" || interrupt["interrupt_id"] != "i1" || interrupt["tool_args"] != nil {
		t.Fatalf("view = %v", view)
	}
	for state, want := range map[*localturn.ChildTurnState]string{
		{Streaming: true}:             "running",
		{Removed: true}:               "cancelled",
		{IsError: true, Failure: "x"}: "failed",
		{AuthorizationPending: true}:  "authorization_required",
		{OutputLimit: true}:           "output_limit",
	} {
		if got := childTurnView(*state)["status"]; got != want {
			t.Fatalf("%+v: %v", *state, got)
		}
	}
}

// gatedChildStarts holds every start until released, so concurrent requests
// overlap inside the start.
type gatedChildStarts struct {
	entered atomic.Int32
	release chan struct{}
}

func (g *gatedChildStarts) StartCurrentApplication(_ context.Context, request agentexecutionapp.CurrentApplicationStartRequest) (agentexecutionapp.CurrentApplicationStartOutcome, error) {
	g.entered.Add(1)
	<-g.release
	return agentexecutionapp.CurrentApplicationStartOutcome{
		ExecutionID: "child-" + request.QuestionID, ResponseMessageID: "6f1c2b9e-0000-4000-8000-000000000001", Created: true,
	}, nil
}

// concurrentTurns and concurrentAuthorizer answer without recording, so
// concurrent requests share them safely.
type concurrentTurns struct{}

func (concurrentTurns) Live(_ context.Context, _, _ int64, _ localturn.Credential, executionID string) (localturn.LiveTurn, error) {
	return localturn.LiveTurn{ApplicationID: 11, VersionID: 12, ExecutionID: executionID}, nil
}

type concurrentAuthorizer struct{}

func (concurrentAuthorizer) AuthorizeChildAgent(context.Context, storage.ChildAgentAuthorization) (storage.ChildAgentGrant, error) {
	return storage.ChildAgentGrant{Name: "Jira reader", AgentType: "agent"}, nil
}

// The rate is charged before the start, atomically: of many concurrent
// starts, only the limit reach the start use case, and a refused one discards
// the conversation it created.
func TestChildTurnRateLimitHoldsUnderConcurrentStarts(t *testing.T) {
	const limit, callers = 2, 8
	starts := &gatedChildStarts{release: make(chan struct{})}
	store := &fakeChildStore{}
	h := newChildTurnHandler(ChildTurnDependencies{
		Start: starts, Authorizer: concurrentAuthorizer{}, Store: store, Audit: &recordedAudit{},
		Turns:   concurrentTurns{},
		Limiter: failurelimit.New(limit, time.Minute),
	})
	var refused atomic.Int32
	var wg sync.WaitGroup
	codes := make([]int, callers)
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			response := callChild(h, http.MethodPost, childStartURL, childBody("x", "key"+strconv.Itoa(i)))
			codes[i] = response.Code
			if response.Code == http.StatusTooManyRequests {
				refused.Add(1)
			}
		}(i)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && starts.entered.Load()+refused.Load() < callers {
		time.Sleep(5 * time.Millisecond)
	}
	close(starts.release)
	wg.Wait()
	if got := starts.entered.Load(); got != limit {
		t.Fatalf("%d starts reached the start use case, want %d (codes %v)", got, limit, codes)
	}
	if refused.Load() != callers-limit {
		t.Fatalf("refused %d, want %d (codes %v)", refused.Load(), callers-limit, codes)
	}
	if len(store.discarded) != callers-limit {
		t.Fatalf("discarded %d conversations, want %d", len(store.discarded), callers-limit)
	}
}

// A restart the start use case answers as a replay admitted nothing, so it
// gives its charge back: the limit counts only admitted starts.
func TestChildTurnReplayIsNotCharged(t *testing.T) {
	h, _, _, _, store, _ := childHandler()
	h.limiter = failurelimit.New(2, time.Minute)
	// k1 is admitted but its stamp is lost (charge 1 of 2).
	store.recordErr = context.DeadlineExceeded
	if response := callChild(h, http.MethodPost, childStartURL, childBody("x", "k1")); response.Code != http.StatusServiceUnavailable {
		t.Fatalf("k1: %d %s", response.Code, response.Body.String())
	}
	store.recordErr = nil
	// The retry restarts with the same question id: a replay, refunded.
	if response := callChild(h, http.MethodPost, childStartURL, childBody("x", "k1")); response.Code != http.StatusAccepted {
		t.Fatalf("k1 retry: %d %s", response.Code, response.Body.String())
	}
	if response := callChild(h, http.MethodPost, childStartURL, childBody("y", "k2")); response.Code != http.StatusAccepted {
		t.Fatalf("k2 was charged for k1's replay: %d %s", response.Code, response.Body.String())
	}
	if response := callChild(h, http.MethodPost, childStartURL, childBody("z", "k3")); response.Code != http.StatusTooManyRequests {
		t.Fatalf("k3: %d %s", response.Code, response.Body.String())
	}
}

// A nested pipeline is refused as the contract's 422 and starts nothing.
func TestChildTurnRefusesANestedPipeline(t *testing.T) {
	h, _, authorizer, starts, store, _ := childHandler()
	authorizer.err = storage.ErrChildAgentPipeline
	response := callChild(h, http.MethodPost, childStartURL, childBody("run it", "k1"))
	if response.Code != http.StatusUnprocessableEntity || decodeChild(t, response)["error"] != "child_agent_unsupported" {
		t.Fatalf("%d %s", response.Code, response.Body.String())
	}
	if len(starts.requests) != 0 || len(store.requests) != 0 {
		t.Fatal("a refused pipeline prepared or started something")
	}
}
