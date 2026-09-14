package mcp

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth"
)

func TestInternalChatSendTargetPreservesApplicationModelAndIdentity(t *testing.T) {
	ctx := auth.ContextWithUser(context.Background(), auth.User{ID: "token-identity", UserID: "3"})
	conversation := func(w http.ResponseWriter, r *http.Request) {
		user, _ := auth.UserFromContext(r.Context())
		if user.ID != "token-identity" || user.UserID != "3" {
			t.Fatal("principal changed")
		}
		_, _ = w.Write([]byte(`{"id":"9","uuid":"chat-uuid","project_id":"2","participants":[{"id":7,"entity_name":"application"},{"id":8,"entity_name":"dummy"}]}`))
	}
	models := func(http.ResponseWriter, *http.Request) { t.Fatal("application must not resolve ordinary model") }
	target, err := resolveInternalChatSendTarget(ctx, 2, "chat-uuid", 7, nil, conversation, models)
	if err != nil || !target.application || target.participantID != 7 || len(target.llmSettings) != 0 {
		t.Fatalf("target=%+v err=%v", target, err)
	}
	if _, err = resolveInternalChatSendTarget(ctx, 2, "chat-uuid", 7, json.RawMessage(`{"model_name":"override"}`), conversation, models); err == nil {
		t.Fatal("application override accepted")
	}
}

func TestInternalChatSendTargetDefaultRequiresAuthorizedCatalogEntry(t *testing.T) {
	conversation := func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"uuid":"chat-uuid","project_id":"2","participants":[{"id":8,"entity_name":"dummy"}]}`))
	}
	for _, listed := range []bool{false, true} {
		t.Run(map[bool]string{false: "missing", true: "listed"}[listed], func(t *testing.T) {
			models := func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Query().Get("include_shared") != "true" || r.URL.Query().Get("section") != "llm" {
					t.Fatal("default lookup lost shared scope")
				}
				items := []any{}
				if listed {
					items = append(items, map[string]any{"name": "shared-model", "project_id": 1})
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"default_model_name": "shared-model", "default_model_project_id": 1, "items": items})
			}
			target, err := resolveInternalChatSendTarget(context.Background(), 2, "chat-uuid", 0, nil, conversation, models)
			if !listed {
				if err == nil {
					t.Fatal("unlisted default accepted")
				}
				return
			}
			if err != nil || target.application || target.participantID != 8 {
				t.Fatalf("target=%+v err=%v", target, err)
			}
			var settings map[string]any
			_ = json.Unmarshal(target.llmSettings, &settings)
			if settings["model_name"] != "shared-model" {
				t.Fatal("default model missing")
			}
		})
	}
}

func TestInternalChatSendTargetRefusesForeignAndAmbiguousTargets(t *testing.T) {
	for _, body := range []string{
		`{"uuid":"other","project_id":"2","participants":[{"id":8,"entity_name":"dummy"}]}`,
		`{"uuid":"chat-uuid","project_id":"3","participants":[{"id":8,"entity_name":"dummy"}]}`,
		`{"uuid":"chat-uuid","project_id":"2","participants":[]}`,
		`{"uuid":"chat-uuid","project_id":"2","participants":[{"id":8,"entity_name":"dummy"},{"id":9,"entity_name":"dummy"}]}`,
	} {
		handler := func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(body)) }
		if _, err := resolveInternalChatSendTarget(context.Background(), 2, "chat-uuid", 0, nil, handler, nil); err == nil {
			t.Fatalf("accepted %s", body)
		}
	}
	denied := func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{}`))
	}
	if _, err := resolveInternalChatSendTarget(context.Background(), 2, "chat-uuid", 0, nil, denied, nil); err == nil {
		t.Fatal("denied read accepted")
	}
}
