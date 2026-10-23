package predict

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/auth"
)

// recordingCompleter captures the CompletionRequest the handler builds and
// answers with a fixed result.
type recordingCompleter struct {
	got     CompletionRequest
	calls   int
	content string
	err     error
}

func (c *recordingCompleter) Complete(_ context.Context, req CompletionRequest) (string, error) {
	c.calls++
	c.got = req
	return c.content, c.err
}

// serve drives one request through a chi route carrying the {projectID}
// parameter, so chi.URLParam resolves exactly as it does in the router.
func serve(t *testing.T, handler *Handler, body string, opts ...func(*http.Request)) *httptest.ResponseRecorder {
	t.Helper()
	router := chi.NewRouter()
	router.Post("/predict_llm/prompt_lib/{projectID}", handler.PredictLLM)

	req := httptest.NewRequest(http.MethodPost, "/predict_llm/prompt_lib/7", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	for _, opt := range opts {
		opt(req)
	}
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, req)
	return recorder
}

func decodeBody(t *testing.T, recorder *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &out); err != nil {
		t.Fatalf("response body is not JSON (%s): %v", recorder.Body.String(), err)
	}
	return out
}

func TestPredictLLMAnswersWithTheGeneratedContent(t *testing.T) {
	t.Parallel()

	completer := &recordingCompleter{content: "a generated instruction"}
	recorder := serve(t, NewHandler(completer), `{
		"user_input": "make it shorter",
		"instructions": "You are terse.",
		"chat_history": [{"role": "user", "content": "earlier"}, {"role": "assistant", "content": "reply"}],
		"llm_settings": {"model_name": "gpt-4o", "temperature": 0.4, "max_tokens": 900},
		"await_task_timeout": 60
	}`, func(r *http.Request) {
		*r = *r.WithContext(auth.ContextWithUser(r.Context(), auth.User{UserID: "42"}))
	})

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d (%s), want 200", recorder.Code, recorder.Body.String())
	}
	body := decodeBody(t, recorder)
	// `content` is the field every current caller reads first.
	if body["content"] != "a generated instruction" {
		t.Errorf("content = %v, want the generated text", body["content"])
	}
	if _, present := body["chat_history"]; present {
		t.Error("chat_history was returned without return_chat_history being asked for")
	}

	if completer.got.ProjectID != "7" {
		t.Errorf("project id = %q, want the {projectID} path parameter 7", completer.got.ProjectID)
	}
	if completer.got.UserID != "42" {
		t.Errorf("user id = %q, want the authenticated principal 42", completer.got.UserID)
	}
	if completer.got.Model != "gpt-4o" {
		t.Errorf("model = %q, want gpt-4o", completer.got.Model)
	}
	// instructions first as a system message, then the history in order, then
	// user_input LAST — the turn being asked about must be the final message.
	wantRoles := []string{"system", "user", "assistant", "user"}
	if len(completer.got.Messages) != len(wantRoles) {
		t.Fatalf("messages = %+v, want %d entries", completer.got.Messages, len(wantRoles))
	}
	for i, role := range wantRoles {
		if completer.got.Messages[i].Role != role {
			t.Errorf("message %d role = %q, want %q", i, completer.got.Messages[i].Role, role)
		}
	}
	if last := completer.got.Messages[len(completer.got.Messages)-1]; last.Content != "make it shorter" {
		t.Errorf("last message = %q, want the user_input", last.Content)
	}
}

func TestPredictLLMRepeatsTheTurnListWhenTheCallerAsksForIt(t *testing.T) {
	t.Parallel()

	recorder := serve(t, NewHandler(&recordingCompleter{content: "answer"}), `{
		"user_input": "question",
		"return_chat_history": true
	}`)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", recorder.Code)
	}
	history, ok := decodeBody(t, recorder)["chat_history"].([]any)
	if !ok {
		t.Fatalf("chat_history missing: %s", recorder.Body.String())
	}
	if len(history) != 2 {
		t.Fatalf("chat_history has %d entries, want the user turn plus the reply", len(history))
	}
	last, _ := history[1].(map[string]any)
	if last["role"] != "assistant" || last["content"] != "answer" {
		t.Errorf("last history entry = %v, want the assistant reply", last)
	}
}

// Legacy's socket.io fields are accepted and inert. A client that still sends
// them must not get a 400 for fields this port simply cannot honour.
func TestPredictLLMAcceptsTheRetiredAsyncFieldsWithoutHonouringThem(t *testing.T) {
	t.Parallel()

	completer := &recordingCompleter{content: "answer"}
	recorder := serve(t, NewHandler(completer), `{
		"user_input": "question",
		"sid": "socket-123",
		"stream_id": "stream-9",
		"thread_id": "thread-4",
		"await_task_timeout": 0
	}`)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d (%s), want 200 — the request is answered synchronously", recorder.Code, recorder.Body.String())
	}
	if completer.calls != 1 {
		t.Errorf("completer calls = %d, want 1: await_task_timeout 0 must not select an async path that does not exist", completer.calls)
	}
	if decodeBody(t, recorder)["content"] != "answer" {
		t.Error("the content did not come back in the HTTP response")
	}
}

func TestPredictLLMRefusesAMalformedBody(t *testing.T) {
	t.Parallel()

	completer := &recordingCompleter{content: "never"}
	recorder := serve(t, NewHandler(completer), `{"user_input": `)
	if recorder.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", recorder.Code)
	}
	if completer.calls != 0 {
		t.Error("a malformed body reached the LLM plane")
	}
}

func TestPredictLLMRefusesABlankUserInput(t *testing.T) {
	t.Parallel()

	completer := &recordingCompleter{content: "never"}
	for _, body := range []string{`{}`, `{"user_input": ""}`, `{"user_input": "   "}`} {
		recorder := serve(t, NewHandler(completer), body)
		if recorder.Code != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400", body, recorder.Code)
		}
	}
	if completer.calls != 0 {
		t.Error("a request with no prompt reached the LLM plane")
	}
}

// The anti-#126 property, at the handler level: with no completer the request
// is REFUSED and says why, naming the variable an operator has to set. It is
// never a 404, and never a fabricated answer.
func TestPredictLLMReportsAnUnconfiguredGatewayAsUnavailable(t *testing.T) {
	t.Parallel()

	recorder := serve(t, NewHandler(nil), `{"user_input": "question"}`)
	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", recorder.Code)
	}
	body := decodeBody(t, recorder)
	if body["code"] != NotConfiguredCode {
		t.Errorf("code = %v, want %q", body["code"], NotConfiguredCode)
	}
	message, _ := body["error"].(string)
	if !strings.Contains(message, "LLM_GATEWAY_URL") {
		t.Errorf("the message does not name the missing configuration: %q", message)
	}
}

func TestPredictLLMReportsAFailedGatewayHopAsABadGateway(t *testing.T) {
	t.Parallel()

	recorder := serve(t, NewHandler(&recordingCompleter{
		err: errors.New("call gateway: dial tcp 10.0.0.5:8443: connection refused"),
	}), `{"user_input": "question"}`)

	if recorder.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", recorder.Code)
	}
	body := decodeBody(t, recorder)
	if _, present := body["content"]; present {
		t.Error("a failed hop produced a content field; callers render that straight into a document")
	}
	// The internal cause is logged, never returned.
	if message, _ := body["error"].(string); strings.Contains(message, "10.0.0.5") {
		t.Errorf("the response echoes internal detail: %q", message)
	}
}

// A history entry the gateway would reject outright must not fail a turn that
// is otherwise complete.
func TestPredictLLMDropsIncompleteHistoryEntries(t *testing.T) {
	t.Parallel()

	completer := &recordingCompleter{content: "answer"}
	recorder := serve(t, NewHandler(completer), `{
		"user_input": "question",
		"chat_history": [{"role": "", "content": "orphan"}, {"role": "user", "content": ""}, {"role": "user", "content": "kept"}]
	}`)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", recorder.Code)
	}
	if len(completer.got.Messages) != 2 {
		t.Fatalf("messages = %+v, want the one usable history entry plus the user input", completer.got.Messages)
	}
	if completer.got.Messages[0].Content != "kept" {
		t.Errorf("first message = %q, want the usable history entry", completer.got.Messages[0].Content)
	}
}

// The caller cannot steer which project's credentials are spent: only the
// path parameter, which the router bound to the caller's membership, is used.
func TestPredictLLMIgnoresACallerSuppliedModelProjectID(t *testing.T) {
	t.Parallel()

	completer := &recordingCompleter{content: "answer"}
	serve(t, NewHandler(completer), `{
		"user_input": "question",
		"llm_settings": {"model_name": "gpt-4o", "model_project_id": 999, "integration_uid": "someone-elses"}
	}`)
	if completer.got.ProjectID != "7" {
		t.Errorf("project id = %q, want the path's 7 and not the body's 999", completer.got.ProjectID)
	}
}
