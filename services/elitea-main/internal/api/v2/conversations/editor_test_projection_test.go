package conversations_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/conversations"
)

type testHistoryRepo struct {
	*mockRepo
	reads         int
	limit, offset int
}

func (r *testHistoryRepo) EditorTestRuns(_ context.Context, projectID, conversationID string, limit, offset int) (conversations.EditorTestRunsPage, error) {
	if projectID != "7" || conversationID != "10" {
		panic("projection used wrong identity")
	}
	r.reads++
	r.limit = limit
	r.offset = offset
	return conversations.EditorTestRunsPage{Rows: []conversations.EditorTestRun{}, Limit: limit, Offset: offset}, nil
}
func TestEditorTestProjectionOptInAndBounds(t *testing.T) {
	repo := &testHistoryRepo{mockRepo: &mockRepo{getFn: func(context.Context, string, string) (conversations.Conversation, error) {
		return conversations.Conversation{ID: "10", Source: conversations.EditorTestSource}, nil
	}}}
	router := newRouter(conversations.NewHandler(repo))
	for _, query := range []string{"", "?editor_test_runs=true&runs_limit=50&runs_offset=12", "?editor_test_runs=true&runs_limit=51", "?editor_test_runs=true&runs_limit=0", "?editor_test_runs=true&runs_offset=10001", "?editor_test_runs=true&runs_offset=-1", "?editor_test_runs=true&runs_limit=oops"} {
		t.Run(query, func(t *testing.T) {
			before := repo.reads
			w := httptest.NewRecorder()
			router.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/projects/7/conversations/10"+query, nil))
			if query == "" {
				if w.Code != 200 || repo.reads != before {
					t.Fatal("ordinary detail changed")
				}
				var body map[string]any
				_ = json.Unmarshal(w.Body.Bytes(), &body)
				if _, present := body["editor_test_runs"]; present {
					t.Fatal("projection was not opt-in")
				}
				return
			}
			if query == "?editor_test_runs=true&runs_limit=50&runs_offset=12" {
				if w.Code != 200 || repo.reads != before+1 || repo.limit != 50 || repo.offset != 12 {
					t.Fatalf("bounded read failed: %d %s", w.Code, w.Body)
				}
				return
			}
			if w.Code != 400 || repo.reads != before {
				t.Fatalf("invalid page reached projection: %d", w.Code)
			}
		})
	}
}
