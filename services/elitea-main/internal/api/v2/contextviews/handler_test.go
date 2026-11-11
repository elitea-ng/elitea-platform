package contextviews

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/db/repos"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/pkg/apierr"
)

const answerID = "00000000-0000-4000-8000-0000000000aa"

type fakeChat struct {
	visible map[string]bool
	asked   []string
}

func (c *fakeChat) AuthorizeChatResource(_ context.Context, projectID, kind, id string) error {
	c.asked = append(c.asked, projectID+"/"+kind+"/"+id)
	if kind == "message" && c.visible[id] {
		return nil
	}
	return apierr.NotFound("chat resource not found")
}

type fakeStore struct {
	views map[string]repos.ContextView
	reads int
}

func (s *fakeStore) MessageContextView(_ context.Context, _, id string) (repos.ContextView, error) {
	s.reads++
	view, ok := s.views[id]
	if !ok {
		return repos.ContextView{}, apierr.NotFound("context view not found")
	}
	return view, nil
}

func serve(h *Handler, projectID, messageID string) *httptest.ResponseRecorder {
	router := chi.NewRouter()
	router.Get(Path, h.Get)
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet,
		"/message_context_view/prompt_lib/"+projectID+"/"+messageID, nil))
	return recorder
}

func TestGetAnswersTheRecordedView(t *testing.T) {
	reason := "before_joined"
	store := &fakeStore{views: map[string]repos.ContextView{answerID: {
		MessageID: answerID, ReaderParticipantID: 5, History: "since_joined", IncludeSideTalk: "party",
		WindowTokens: 128000, Digest: strings.Repeat("a", 64),
		Entries: []repos.ContextViewEntry{{MessageID: "m1", Reason: &reason}, {MessageID: "m2"}},
	}}}
	chat := &fakeChat{visible: map[string]bool{answerID: true}}
	recorder := serve(NewHandler(store, chat), "7", answerID)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d; body=%s", recorder.Code, recorder.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body["history"] != "since_joined" || body["window_tokens"].(float64) != 128000 {
		t.Fatalf("body = %v", body)
	}
	entries := body["entries"].([]any)
	if len(entries) != 2 || entries[0].(map[string]any)["reason"] != "before_joined" || entries[1].(map[string]any)["reason"] != nil {
		t.Fatalf("entries = %v", entries)
	}
	if recorder.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("the view is cached")
	}
	if len(chat.asked) != 1 || chat.asked[0] != "7/message/"+answerID {
		t.Fatalf("authorised %v", chat.asked)
	}
}

// A message of a conversation the caller may not read is a 404 before the
// view is read: the inspector shows nothing the transcript would not.
func TestGetRefusesAMessageTheCallerCannotRead(t *testing.T) {
	store := &fakeStore{views: map[string]repos.ContextView{answerID: {MessageID: answerID}}}
	recorder := serve(NewHandler(store, &fakeChat{}), "7", answerID)
	if recorder.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body=%s", recorder.Code, recorder.Body.String())
	}
	if store.reads != 0 {
		t.Fatal("the view was read before the conversation rule")
	}
}

func TestGetAnswersNotFoundWithoutARecord(t *testing.T) {
	chat := &fakeChat{visible: map[string]bool{answerID: true}}
	recorder := serve(NewHandler(&fakeStore{}, chat), "7", answerID)
	if recorder.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", recorder.Code)
	}
}

func TestGetWithoutStorageIsUnavailable(t *testing.T) {
	recorder := serve(NewHandler((*repos.ContextViews)(nil), &fakeChat{}), "7", answerID)
	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", recorder.Code)
	}
}
