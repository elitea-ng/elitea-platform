package conversations_test

// #3077: a create's client_request_id is its idempotency key. The Postgres
// behaviour (one conversation per id, also under a race) is in
// repos/conversation_client_request_id_postgres_integration_test.go.

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/v2/conversations"
)

func postCreate(t *testing.T, body string, createFn func(context.Context, string, conversations.Conversation) (conversations.Conversation, error)) *httptest.ResponseRecorder {
	t.Helper()
	router := newRouter(conversations.NewHandler(&mockRepo{createFn: createFn}))
	req := httptest.NewRequest(http.MethodPost, "/projects/proj-1/conversations/", bytes.NewReader([]byte(body)))
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	return w
}

func TestCreate_HandsTheClientRequestIDToTheRepository(t *testing.T) {
	var seen conversations.Conversation
	w := postCreate(t, `{"name":"c","client_request_id":"0B6F6D1E-4F2A-4C3B-9D1E-3077AAAAAAAA","meta":{"client_request_id":"forged","steps_limit":3}}`,
		func(_ context.Context, projectID string, conv conversations.Conversation) (conversations.Conversation, error) {
			seen = conv
			conv.ID = "1"
			conv.ProjectID = projectID
			return conv, nil
		})
	if w.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", w.Code, w.Body.String())
	}
	if seen.ClientRequestID != "0b6f6d1e-4f2a-4c3b-9d1e-3077aaaaaaaa" {
		t.Fatalf("ClientRequestID = %q, want the canonical UUID", seen.ClientRequestID)
	}
	if _, forged := seen.Meta[conversations.ClientRequestIDMetaKey]; forged || seen.Meta["steps_limit"] != float64(3) {
		t.Fatalf("meta handed to the repository = %v", seen.Meta)
	}
}

func TestCreate_RefusesAClientRequestIDThatIsNotAUUID(t *testing.T) {
	called := false
	w := postCreate(t, `{"name":"c","client_request_id":"three"}`, func(context.Context, string, conversations.Conversation) (conversations.Conversation, error) {
		called = true
		return conversations.Conversation{}, nil
	})
	if w.Code != http.StatusBadRequest || called {
		t.Fatalf("expected 400 without a create, got %d (created %v)", w.Code, called)
	}
}

func TestCreate_AReplayAnswersTheEarlierConversation(t *testing.T) {
	w := postCreate(t, `{"name":"c","client_request_id":"0b6f6d1e-4f2a-4c3b-9d1e-3077aaaaaaaa"}`,
		func(_ context.Context, projectID string, _ conversations.Conversation) (conversations.Conversation, error) {
			return conversations.Conversation{ID: "41", UUID: "0b6f6d1e-0000-4000-8000-000000000041", ProjectID: projectID, Name: "first", Replayed: true}, nil
		})
	if w.Code != http.StatusCreated || !bytes.Contains(w.Body.Bytes(), []byte(`"id":"41"`)) {
		t.Fatalf("expected the earlier conversation, got %d: %s", w.Code, w.Body.String())
	}
}
