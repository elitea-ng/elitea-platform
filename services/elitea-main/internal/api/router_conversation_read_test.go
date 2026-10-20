package api

// markConversationRead (client contract 1.12) moves the caller's own read
// marker, so its gate is the conversation read permission alone.

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

const conversationReadPath = "/api/v2/elitea_core/conversation_read/prompt_lib/7/1"

func TestConversationReadNeedsTheReadPermission(t *testing.T) {
	t.Run("without it", func(t *testing.T) {
		router := newEliteaCoreProjectScopeRouter(&memberOfProject{project: "7"},
			fakePermissionResolver{granted: []string{"models.chat.conversations.list"}, forProject: "7"})
		recorder := httptest.NewRecorder()
		router.ServeHTTP(recorder, testAuthHeader(httptest.NewRequest(http.MethodPut, conversationReadPath, nil)))
		if recorder.Code != http.StatusForbidden {
			t.Fatalf("status = %d, want 403; body=%s", recorder.Code, recorder.Body.String())
		}
	})

	t.Run("with it", func(t *testing.T) {
		router := newEliteaCoreProjectScopeRouter(&memberOfProject{project: "7"},
			fakePermissionResolver{granted: []string{"models.chat.conversation.details"}, forProject: "7"})
		recorder := httptest.NewRecorder()
		router.ServeHTTP(recorder, testAuthHeader(httptest.NewRequest(http.MethodPut, conversationReadPath, nil)))
		// The empty repository keeps no markers, so the handler answers 501:
		// the gate let the request through to a registered route.
		if recorder.Code != http.StatusNotImplemented {
			t.Fatalf("status = %d, want the handler's 501; body=%s", recorder.Code, recorder.Body.String())
		}
	})

	t.Run("another project", func(t *testing.T) {
		router := newEliteaCoreProjectScopeRouter(&memberOfProject{project: "7"}, entitledForProjectSeven())
		recorder := httptest.NewRecorder()
		router.ServeHTTP(recorder, testAuthHeader(httptest.NewRequest(http.MethodPut,
			"/api/v2/elitea_core/conversation_read/prompt_lib/8/1", nil)))
		if recorder.Code != http.StatusForbidden {
			t.Fatalf("status = %d, want 403; body=%s", recorder.Code, recorder.Body.String())
		}
	})
}
