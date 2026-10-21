package api

// forkConversation (client contract 1.11) creates a conversation from
// another one, so its gate names two permissions: the create permission and
// the conversation read permission. The single-permission table in
// router_elitea_core_project_scope_test.go cannot state that, so this file
// checks each half and the pair.

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

const conversationForkPath = "/api/v2/elitea_core/conversation_fork/prompt_lib/7/1"

func TestConversationForkNeedsTheCreateAndTheReadPermission(t *testing.T) {
	for _, withheld := range []string{"models.chat.conversations.create", "models.chat.conversation.details"} {
		t.Run("without "+withheld, func(t *testing.T) {
			granted := []string{}
			for _, permission := range []string{"models.chat.conversations.create", "models.chat.conversation.details"} {
				if permission != withheld {
					granted = append(granted, permission)
				}
			}
			router := newEliteaCoreProjectScopeRouter(&memberOfProject{project: "7"},
				fakePermissionResolver{granted: granted, forProject: "7"})
			recorder := httptest.NewRecorder()
			router.ServeHTTP(recorder, testAuthHeader(httptest.NewRequest(http.MethodPost, conversationForkPath, nil)))
			if recorder.Code != http.StatusForbidden {
				t.Fatalf("status = %d, want 403 for a member without %q; body=%s", recorder.Code, withheld, recorder.Body.String())
			}
		})
	}

	t.Run("with both", func(t *testing.T) {
		router := newEliteaCoreProjectScopeRouter(&memberOfProject{project: "7"},
			fakePermissionResolver{granted: []string{"models.chat.conversations.create", "models.chat.conversation.details"}, forProject: "7"})
		recorder := httptest.NewRecorder()
		router.ServeHTTP(recorder, testAuthHeader(httptest.NewRequest(http.MethodPost, conversationForkPath, nil)))
		// The empty repository does not fork, so the handler answers 501:
		// the gate let the request through to a registered route.
		if recorder.Code != http.StatusNotImplemented {
			t.Fatalf("status = %d, want the handler's 501; body=%s", recorder.Code, recorder.Body.String())
		}
	})

	t.Run("another project", func(t *testing.T) {
		router := newEliteaCoreProjectScopeRouter(&memberOfProject{project: "7"}, entitledForProjectSeven())
		recorder := httptest.NewRecorder()
		router.ServeHTTP(recorder, testAuthHeader(httptest.NewRequest(http.MethodPost,
			"/api/v2/elitea_core/conversation_fork/prompt_lib/8/1", nil)))
		if recorder.Code != http.StatusForbidden {
			t.Fatalf("status = %d, want 403; body=%s", recorder.Code, recorder.Body.String())
		}
	})
}
