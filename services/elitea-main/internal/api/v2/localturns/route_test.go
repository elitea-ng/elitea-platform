package localturns

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/localturn"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth"
)

type fakeUseCase struct {
	startErr  error
	commitErr error
	started   localturn.StartRequest
	committed localturn.CommitRequest
}

func (f *fakeUseCase) Start(_ context.Context, request localturn.StartRequest) (localturn.StartOutcome, error) {
	f.started = request
	return localturn.StartOutcome{
		ExecutionID: "0123456789abcdef0123456789abcdef", QuestionID: request.QuestionID,
		ExpiresAt: time.Unix(0, 0), Created: true,
		Recall: localturn.MemoryRecall{Text: "recall", Count: 1, IDs: []string{"9"}},
	}, f.startErr
}

func (f *fakeUseCase) Commit(_ context.Context, request localturn.CommitRequest) (localturn.CommittedTurn, error) {
	f.committed = request
	return localturn.CommittedTurn{ExecutionID: request.ExecutionID, Created: true}, f.commitErr
}

// serve runs one handler with the principal on the context, as apimw.Auth
// leaves it, and the chi route params the router would set.
func serve(t *testing.T, h http.HandlerFunc, user *auth.User, path, pattern, body string) *httptest.ResponseRecorder {
	t.Helper()
	router := chi.NewRouter()
	router.Post(pattern, func(w http.ResponseWriter, r *http.Request) {
		if user != nil {
			r = r.WithContext(auth.ContextWithUser(r.Context(), *user))
		}
		h(w, r)
	})
	request := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	return recorder
}

func tokenUser() *auth.User {
	return &auth.User{ID: "7", UserID: "7", TokenID: "70", AuthType: "token", NativeClientID: "ai.elitea.desktop"}
}

const startURL = "/api/v2/elitea_core/local_turn/prompt_lib/1/6f1c2d3e-4a5b-4c6d-8e7f-901234567890"

func TestStartPassesTheTokenPrincipalAndAnswersTheRecall(t *testing.T) {
	useCase := &fakeUseCase{}
	h := &handler{useCase: useCase}
	response := serve(t, h.start, tokenUser(), startURL, StartPath,
		`{"question_id":"11111111-2222-4333-8444-555555555555","user_input":"hi","participant_id":3}`)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", response.Code, response.Body)
	}
	if useCase.started.TokenID != "70" || useCase.started.NativeClientID != "ai.elitea.desktop" ||
		useCase.started.ActorUserID != 7 || useCase.started.ParticipantID != 3 ||
		useCase.started.ConversationUUID != "6f1c2d3e-4a5b-4c6d-8e7f-901234567890" {
		t.Fatalf("start request = %+v", useCase.started)
	}
	var body map[string]any
	_ = json.Unmarshal(response.Body.Bytes(), &body)
	recall, _ := body["memory_recall"].(map[string]any)
	if recall["text"] != "recall" || recall["count"] != float64(1) {
		t.Fatalf("memory_recall = %v", body["memory_recall"])
	}
}

func TestStartRefusesABrowserSession(t *testing.T) {
	h := &handler{useCase: &fakeUseCase{}}
	session := &auth.User{ID: "7", UserID: "7", AuthType: "session"}
	response := serve(t, h.start, session, startURL, StartPath, `{}`)
	if response.Code != http.StatusForbidden || !strings.Contains(response.Body.String(), "local_turn_requires_token") {
		t.Fatalf("status = %d, body %s", response.Code, response.Body)
	}
}

func TestUseCaseErrorsMapToTypedAnswers(t *testing.T) {
	for _, tc := range []struct {
		err    error
		status int
		code   string
	}{
		{localturn.ErrLocalWorkDisabled, http.StatusForbidden, "local_work_disabled"},
		{localturn.ErrInvalid, http.StatusBadRequest, "invalid_local_turn"},
		{localturn.ErrNotFound, http.StatusNotFound, "local_turn_not_found"},
		{localturn.ErrParticipant, http.StatusUnprocessableEntity, "local_turn_participant"},
		{localturn.ErrConflict, http.StatusConflict, "local_turn_conflict"},
		{localturn.ErrAlreadyCommitted, http.StatusConflict, "local_turn_already_committed"},
		{localturn.ErrExpired, http.StatusGone, "local_turn_expired"},
		{localturn.ErrUnavailable, http.StatusServiceUnavailable, "local_turn_unavailable"},
	} {
		h := &handler{useCase: &fakeUseCase{startErr: tc.err, commitErr: tc.err}}
		response := serve(t, h.start, tokenUser(), startURL, StartPath, `{}`)
		if response.Code != tc.status || !strings.Contains(response.Body.String(), `"`+tc.code+`"`) {
			t.Errorf("%v: status = %d, body %s", tc.err, response.Code, response.Body)
		}
	}
}

func TestCommitDecodesTheBody(t *testing.T) {
	useCase := &fakeUseCase{}
	h := &handler{useCase: useCase}
	response := serve(t, h.commit, tokenUser(),
		"/api/v2/elitea_core/local_turn_commit/prompt_lib/1/0123456789abcdef0123456789abcdef", CommitPath,
		`{"user_message":{"content":"q"},"assistant_message":{"content":"a","is_error":false},
		  "tool_calls":{"r":{"tool_name":"shell"}},"thinking_steps":[{"type":"thinking"}],
		  "hitl_exchanges":[{"interrupt_id":"i","kind":"k","prompt":"p","decision":"approve"}],
		  "local_work":{"sandbox_mode":"read-only","commands":[{"command":"ls"}],"paths_touched":["a"]}}`)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", response.Code, response.Body)
	}
	got := useCase.committed
	if got.ExecutionID != "0123456789abcdef0123456789abcdef" || got.UserMessage != "q" || got.AssistantMessage != "a" ||
		len(got.ThinkingSteps) != 1 || len(got.HITLExchanges) != 1 || got.Report.SandboxMode != "read-only" ||
		len(got.Report.Commands) != 1 || len(got.Report.Paths) != 1 || !strings.Contains(string(got.ToolCalls), "shell") {
		t.Fatalf("commit request = %+v", got)
	}
	trailing := serve(t, h.commit, tokenUser(),
		"/api/v2/elitea_core/local_turn_commit/prompt_lib/1/0123456789abcdef0123456789abcdef", CommitPath, `{} {}`)
	if trailing.Code != http.StatusBadRequest {
		t.Fatalf("two JSON values: status = %d", trailing.Code)
	}
}
