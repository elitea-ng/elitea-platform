package deepwiki

// The two write paths of the wiki chat transcript, and the rules that decide
// whether a turn is recorded at all.
//
// Both are exercised through History rather than through the mounted route,
// because what is under test is the DECISION — is this a turn, whose is it,
// did the provider accept it, is this poll terminal — and a route test would
// spend most of its lines building authentication for a question already
// answered by route_test.go.

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/wikichat"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/providerhost/material"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/providerhost/proxy"
)

// recordingStore is a wikichat.Store that remembers what it was asked to
// write, and enforces the same idempotency rule the real one does — the
// answer of an invocation that already has one is not written twice. Without
// that rule here, a tee that recorded every poll would pass.
type recordingStore struct {
	mutex     sync.Mutex
	questions []wikichat.Question
	answers   []wikichat.Answer
	answered  map[string]bool
	questErr  error
	answerErr error
}

func newRecordingStore() *recordingStore {
	return &recordingStore{answered: map[string]bool{}}
}

func (s *recordingStore) RecordQuestion(_ context.Context, question wikichat.Question) error {
	s.mutex.Lock()
	defer s.mutex.Unlock()
	if s.questErr != nil {
		return s.questErr
	}
	s.questions = append(s.questions, question)
	return nil
}

func (s *recordingStore) RecordAnswer(_ context.Context, answer wikichat.Answer) (bool, error) {
	s.mutex.Lock()
	defer s.mutex.Unlock()
	if s.answerErr != nil {
		return false, s.answerErr
	}
	if s.answered[answer.InvocationID] {
		return false, nil
	}
	s.answered[answer.InvocationID] = true
	s.answers = append(s.answers, answer)
	return true, nil
}

func (s *recordingStore) recorded() ([]wikichat.Question, []wikichat.Answer) {
	s.mutex.Lock()
	defer s.mutex.Unlock()
	return append([]wikichat.Question(nil), s.questions...), append([]wikichat.Answer(nil), s.answers...)
}

func quietHistory(store wikichat.Store) *History {
	return NewHistory(store, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

// serveInvoke drives one invoke through WrapInvoke, standing in for
// material.Invocation.Serve: it reports what the handler saw and then calls
// the observer exactly where Serve does — after the hop, with the client's
// own body and the provider's answer.
func serveInvoke(
	history *History, request *http.Request, served material.Served,
) (seenHeaders http.Header) {
	handler := history.WrapInvoke(func(_ http.ResponseWriter, r *http.Request) {
		seenHeaders = r.Header.Clone()
		if observer := history.Observer(); observer != nil {
			observer(r.Context(), served)
		}
	})
	handler(httptest.NewRecorder(), request)
	return seenHeaders
}

func invokeRequest(chatKey, toolkit, body string) *http.Request {
	request := httptest.NewRequest(http.MethodPost, "/api/v2/deepwiki/tools/7/Wikis/ask/invoke",
		strings.NewReader(body))
	if chatKey != "" {
		request.Header.Set(ChatKeyHeader, chatKey)
	}
	if toolkit != "" {
		request.Header.Set(ToolkitHeader, toolkit)
	}
	return request
}

const askBody = `{"configuration":{"parameters":{}},"parameters":{"question":"Where do the pages live?"}}`

func TestObserveRecordsTheQuestionOfAnAcceptedInvoke(t *testing.T) {
	store := newRecordingStore()
	history := quietHistory(store)

	serveInvoke(history, invokeRequest("chat-1", "42", askBody), material.Served{
		ProjectID: 7, UserID: 3, ToolkitName: "Wikis", ToolName: "ask",
		Request:  []byte(askBody),
		Response: []byte(`{"invocation_id":"inv-1","status":"Started"}`),
		Status:   http.StatusOK,
	})

	questions, _ := store.recorded()
	if len(questions) != 1 {
		t.Fatalf("recorded %d questions, want 1", len(questions))
	}
	got := questions[0]
	if got.InvocationID != "inv-1" {
		t.Errorf("invocation = %q, want inv-1", got.InvocationID)
	}
	if got.Question != "Where do the pages live?" {
		t.Errorf("question = %q", got.Question)
	}
	if got.ChatKey != "chat-1" || got.ToolkitID != 42 {
		t.Errorf("conversation = %q toolkit = %d, want chat-1 and 42", got.ChatKey, got.ToolkitID)
	}
	if got.ProjectID != 7 || got.UserID != 3 {
		t.Errorf("project = %d user = %d, want 7 and 3", got.ProjectID, got.UserID)
	}
	if got.Capability != "ask" {
		t.Errorf("capability = %q, want ask", got.Capability)
	}
}

// The tool decides the label, not the drawer's toggle: the toggle can move
// while a question is in flight.
func TestObserveLabelsResearchByItsTool(t *testing.T) {
	store := newRecordingStore()
	serveInvoke(quietHistory(store), invokeRequest("chat-1", "42", askBody), material.Served{
		ProjectID: 7, UserID: 3, ToolkitName: "Wikis", ToolName: "deep_research",
		Request:  []byte(askBody),
		Response: []byte(`{"invocation_id":"inv-9"}`),
		Status:   http.StatusOK,
	})
	questions, _ := store.recorded()
	if len(questions) != 1 || questions[0].Capability != "research" {
		t.Fatalf("capability = %+v, want one question labelled research", questions)
	}
}

// The headers are the platform's bookkeeping. A provider that received them
// would be handed a conversation id it has no idea what to do with.
func TestWrapInvokeStripsTheDrawersHeadersBeforeTheHop(t *testing.T) {
	seen := serveInvoke(quietHistory(newRecordingStore()),
		invokeRequest("chat-1", "42", askBody), material.Served{Status: http.StatusOK})
	if value := seen.Get(ChatKeyHeader); value != "" {
		t.Errorf("%s survived the wrapper as %q", ChatKeyHeader, value)
	}
	if value := seen.Get(ToolkitHeader); value != "" {
		t.Errorf("%s survived the wrapper as %q", ToolkitHeader, value)
	}
}

// A generation is an invoke on the same route and carries no conversation.
// Recording one would put a wiki build into somebody's chat drawer.
func TestObserveRecordsNothingWithoutAConversationKey(t *testing.T) {
	store := newRecordingStore()
	serveInvoke(quietHistory(store), invokeRequest("", "42", askBody), material.Served{
		ProjectID: 7, UserID: 3, ToolName: "ask",
		Request:  []byte(askBody),
		Response: []byte(`{"invocation_id":"inv-1"}`),
		Status:   http.StatusOK,
	})
	if questions, _ := store.recorded(); len(questions) != 0 {
		t.Fatalf("recorded %d questions for a request with no conversation key", len(questions))
	}
}

func TestObserveRecordsNothingForARefusedOrUnfollowableInvoke(t *testing.T) {
	for name, served := range map[string]material.Served{
		"refused by the provider": {
			ProjectID: 7, UserID: 3, ToolName: "ask", Request: []byte(askBody),
			Response: []byte(`{"error":"nope"}`), Status: http.StatusBadRequest,
		},
		"accepted with no invocation id": {
			ProjectID: 7, UserID: 3, ToolName: "ask", Request: []byte(askBody),
			Response: []byte(`{"status":"Started"}`), Status: http.StatusOK,
		},
		"a body carrying no question": {
			ProjectID: 7, UserID: 3, ToolName: "ask", Request: []byte(`{"parameters":{}}`),
			Response: []byte(`{"invocation_id":"inv-1"}`), Status: http.StatusOK,
		},
		"a body too large to capture": {
			ProjectID: 7, UserID: 3, ToolName: "ask", Request: nil,
			Response: []byte(`{"invocation_id":"inv-1"}`), Status: http.StatusOK,
		},
	} {
		t.Run(name, func(t *testing.T) {
			store := newRecordingStore()
			serveInvoke(quietHistory(store), invokeRequest("chat-1", "42", askBody), served)
			if questions, _ := store.recorded(); len(questions) != 0 {
				t.Fatalf("recorded %d questions, want none", len(questions))
			}
		})
	}
}

// A nil History must not switch body capture on: material.Invocation decides
// on `Observe != nil`, and a method value of a nil pointer is not nil.
func TestObserverIsNilWhenNothingIsRecorded(t *testing.T) {
	var history *History
	if history.Observer() != nil {
		t.Fatal("a facade that records nothing handed out a non-nil observer")
	}
	if NewHistory(nil, nil) != nil {
		t.Fatal("NewHistory built a recorder with no store")
	}
}

// pollThrough drives one poll through the tee. `inner` stands in for the hop:
// it fills the Outcome the way providerhost/proxy's ModifyResponse does and
// writes the same bytes to the caller.
func pollThrough(history *History, status int, body string) *httptest.ResponseRecorder {
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet,
		"/api/v2/deepwiki/invocations/7/Wikis/ask/inv-1", nil)
	// The stand-in hop reports through the Outcome exactly as
	// providerhost/proxy's ModifyResponse does, and writes the same bytes to
	// the caller — so a tee that quietly consumed the body would be visible
	// in the recorder rather than only in the store.
	forward := history.Poll(func(
		w http.ResponseWriter, r *http.Request, _, _, _ string,
	) {
		if outcome := proxy.OutcomeFrom(r.Context()); outcome != nil {
			outcome.Status = status
			if outcome.CaptureLimit > 0 {
				outcome.Body = []byte(body)
			}
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	})
	forward(recorder, request, "/tools/Wikis/ask/invocations/inv-1", "7", "3")
	return recorder
}

func TestPollRecordsTheAnswerOfATerminalPoll(t *testing.T) {
	store := newRecordingStore()
	history := quietHistory(store)

	recorder := pollThrough(history, http.StatusOK,
		`{"invocation_id":"inv-1","status":"Completed","result":"[{\"object_type\":\"message\",\"data\":\"The pages live in wiki_pages/.\"}]"}`)

	if recorder.Code != http.StatusOK {
		t.Fatalf("the tee changed the caller's status to %d", recorder.Code)
	}
	if !strings.Contains(recorder.Body.String(), "wiki_pages") {
		t.Fatalf("the tee changed the caller's body: %s", recorder.Body.String())
	}
	_, answers := store.recorded()
	if len(answers) != 1 {
		t.Fatalf("recorded %d answers, want 1", len(answers))
	}
	if answers[0].Content != "The pages live in wiki_pages/." {
		t.Errorf("answer = %q", answers[0].Content)
	}
	if answers[0].IsError {
		t.Error("a completed answer was recorded as an error")
	}
	if answers[0].ProjectID != 7 || answers[0].UserID != 3 {
		t.Errorf("project = %d user = %d, want 7 and 3", answers[0].ProjectID, answers[0].UserID)
	}
}

// THE IDEMPOTENCY THE FEATURE TURNS ON. The browser polls on an interval and
// nothing stops it reaching the terminal payload again — a refocus, a second
// tab, a slow render. A second write would show the answer twice.
func TestPollWritesTheAnswerOnceUnderRepeatedPolls(t *testing.T) {
	store := newRecordingStore()
	history := quietHistory(store)
	terminal := `{"invocation_id":"inv-1","status":"Completed","result":"done"}`

	for range 5 {
		pollThrough(history, http.StatusOK, terminal)
	}

	if _, answers := store.recorded(); len(answers) != 1 {
		t.Fatalf("five terminal polls recorded %d answers, want 1", len(answers))
	}
}

func TestPollWritesTheAnswerOnceUnderConcurrentPolls(t *testing.T) {
	store := newRecordingStore()
	history := quietHistory(store)
	terminal := `{"invocation_id":"inv-1","status":"Completed","result":"done"}`

	var waiting sync.WaitGroup
	for range 16 {
		waiting.Add(1)
		go func() {
			defer waiting.Done()
			pollThrough(history, http.StatusOK, terminal)
		}()
	}
	waiting.Wait()

	if _, answers := store.recorded(); len(answers) != 1 {
		t.Fatalf("sixteen concurrent terminal polls recorded %d answers, want 1", len(answers))
	}
}

func TestPollRecordsNothingBeforeTheInvocationEnds(t *testing.T) {
	for name, body := range map[string]string{
		"started":                 `{"invocation_id":"inv-1","status":"Started"}`,
		"in progress":             `{"invocation_id":"inv-1","status":"InProgress"}`,
		"a status we do not know": `{"invocation_id":"inv-1","status":"Reticulating"}`,
		"no status at all":        `{"invocation_id":"inv-1"}`,
		"no invocation id":        `{"status":"Completed","result":"done"}`,
		"not json":                `<html>gateway</html>`,
	} {
		t.Run(name, func(t *testing.T) {
			store := newRecordingStore()
			pollThrough(quietHistory(store), http.StatusOK, body)
			if _, answers := store.recorded(); len(answers) != 0 {
				t.Fatalf("recorded %d answers for %s", len(answers), name)
			}
		})
	}
}

// A failed run is a turn too. Dropping it would make a conversation whose
// answer failed indistinguishable from one whose answer was never drained —
// the accepted gap this feature ships with.
func TestPollRecordsAFailedRunAsAnErrorTurn(t *testing.T) {
	store := newRecordingStore()
	pollThrough(quietHistory(store), http.StatusOK,
		`{"invocation_id":"inv-1","status":"Error","result":"the repository could not be cloned"}`)

	_, answers := store.recorded()
	if len(answers) != 1 || !answers[0].IsError {
		t.Fatalf("answers = %+v, want one error turn", answers)
	}
	if answers[0].Content != "the repository could not be cloned" {
		t.Errorf("answer = %q", answers[0].Content)
	}
}

func TestPollRecordsNothingWhenTheHopItselfFailed(t *testing.T) {
	store := newRecordingStore()
	pollThrough(quietHistory(store), http.StatusBadGateway,
		`{"invocation_id":"inv-1","status":"Completed","result":"done"}`)
	if _, answers := store.recorded(); len(answers) != 0 {
		t.Fatalf("recorded %d answers for a 502", len(answers))
	}
}

// A transcript that cannot be written must not cost the user their answer.
// The invocation is already running by the time the question is recorded, and
// the answer has already been written to the browser by the time the tee
// runs, so both failures are logged and swallowed.
func TestARecordingFailureDoesNotChangeWhatTheCallerReceives(t *testing.T) {
	store := newRecordingStore()
	store.questErr = errors.New("the database is down")
	store.answerErr = errors.New("the database is still down")
	history := quietHistory(store)

	serveInvoke(history, invokeRequest("chat-1", "42", askBody), material.Served{
		ProjectID: 7, UserID: 3, ToolName: "ask",
		Request: []byte(askBody), Response: []byte(`{"invocation_id":"inv-1"}`),
		Status: http.StatusOK,
	})
	recorder := pollThrough(history, http.StatusOK,
		`{"invocation_id":"inv-1","status":"Completed","result":"done"}`)

	if recorder.Code != http.StatusOK || recorder.Body.String() == "" {
		t.Fatalf("a failed recording changed the response: %d %q", recorder.Code, recorder.Body.String())
	}
}
