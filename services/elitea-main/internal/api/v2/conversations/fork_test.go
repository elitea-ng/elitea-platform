package conversations_test

// Client contract 1.11: `meta.forked_from` is the server's. Only
// forkConversation writes it; the Postgres behaviour of the fork is in
// repos/conversation_fork_postgres_integration_test.go.

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/conversations"
)

// A create cannot forge a fork marker (a client-side Duplicate copies meta
// as it is, and the copy is not a fork).
func TestCreate_DropsAClientForkMarker(t *testing.T) {
	var seen conversations.Conversation
	repo := &mockRepo{
		createFn: func(_ context.Context, projectID string, conv conversations.Conversation) (conversations.Conversation, error) {
			seen = conv
			conv.ID = "1"
			conv.ProjectID = projectID
			return conv, nil
		},
	}
	router := newRouter(conversations.NewHandler(repo))
	req := httptest.NewRequest(http.MethodPost, "/projects/proj-1/conversations/",
		bytes.NewReader([]byte(`{"name":"c","meta":{"steps_limit":3,"forked_from":{"conversation_id":5}}}`)))
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	if w.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", w.Code, w.Body.String())
	}
	if _, forged := seen.Meta[conversations.ForkedFromMetaKey]; forged || seen.Meta["steps_limit"] != float64(3) {
		t.Fatalf("meta handed to the repository = %v", seen.Meta)
	}
}

func TestPreserveForkedFrom(t *testing.T) {
	marker := map[string]any{"conversation_id": float64(5)}
	for name, tc := range map[string]struct {
		current, update, want map[string]any
	}{
		"kept through a write without it":  {map[string]any{"forked_from": marker}, map[string]any{"a": 1}, map[string]any{"a": 1, "forked_from": marker}},
		"a client value cannot replace it": {map[string]any{"forked_from": marker}, map[string]any{"forked_from": "x"}, map[string]any{"forked_from": marker}},
		"a client value cannot add one":    {map[string]any{}, map[string]any{"forked_from": marker, "a": 1}, map[string]any{"a": 1}},
		"no meta write, nothing changes":   {map[string]any{"forked_from": marker}, nil, nil},
	} {
		t.Run(name, func(t *testing.T) {
			update := conversations.Conversation{Meta: tc.update}
			conversations.PreserveForkedFrom(conversations.Conversation{Meta: tc.current}, &update)
			if !reflect.DeepEqual(update.Meta, tc.want) {
				t.Fatalf("meta = %v, want %v", update.Meta, tc.want)
			}
		})
	}
}
