package conversations_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/v2/conversations"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/applicationcomposition"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/pkg/apierr"
)

// A participant whose composition breaks a bound is refused with the
// registered code next to the named text, as the attach route answers it: the
// pipeline editor's test chat shows Main's text only for that code (FU-249).
func TestCompositionRefusalCarriesItsCode(t *testing.T) {
	const message = `Pipeline "FP inner parallel" has Parallel node "inner", so it cannot run as a tool of an agent or inside a Parallel branch.`
	refusal := &apierr.APIError{Status: http.StatusBadRequest, Code: applicationcomposition.Code, Message: message}
	repo := &mockRepo{
		createFn: func(context.Context, string, conversations.Conversation) (conversations.Conversation, error) {
			return conversations.Conversation{}, refusal
		},
		addParticipantFn:       func(context.Context, string, string, map[string]any) error { return refusal },
		updateEntitySettingsFn: func(context.Context, string, string, string, map[string]any) error { return refusal },
	}
	router := newRouter(conversations.NewHandler(repo))
	create, _ := json.Marshal(conversations.Conversation{Name: "conv"})
	participants, _ := json.Marshal([]map[string]any{{"entity_name": "application", "entity_meta": map[string]any{"id": 1, "project_id": 1}}})
	for _, route := range []struct {
		name, method, path string
		body               []byte
	}{
		{"create", http.MethodPost, "/projects/proj-1/conversations/", create},
		{"add participant", http.MethodPost, "/projects/proj-1/conversations/conv-1/participants", participants},
		{"participant settings", http.MethodPut, "/projects/proj-1/conversations/conv-1/participants/part-1/settings", []byte("{}")},
	} {
		req := httptest.NewRequest(route.method, route.path, bytes.NewReader(route.body))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)

		if w.Code != http.StatusBadRequest {
			t.Fatalf("%s: expected 400, got %d: %s", route.name, w.Code, w.Body.String())
		}
		var body map[string]any
		if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
			t.Fatalf("%s: body: %v", route.name, err)
		}
		if body["code"] != applicationcomposition.Code || body["error"] != message {
			t.Fatalf("%s: expected the coded refusal, got %v", route.name, body)
		}
	}
}

// Any other repository refusal keeps the plain `{"error"}` body.
func TestOtherRefusalsKeepThePlainBody(t *testing.T) {
	repo := &mockRepo{
		createFn: func(context.Context, string, conversations.Conversation) (conversations.Conversation, error) {
			return conversations.Conversation{}, apierr.BadRequest("invalid participant")
		},
	}
	router := newRouter(conversations.NewHandler(repo))
	create, _ := json.Marshal(conversations.Conversation{Name: "conv"})
	req := httptest.NewRequest(http.MethodPost, "/projects/proj-1/conversations/", bytes.NewReader(create))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest || w.Body.String() != "{\"error\":\"invalid participant\"}\n" {
		t.Fatalf("expected the plain 400 body, got %d %q", w.Code, w.Body.String())
	}
}
