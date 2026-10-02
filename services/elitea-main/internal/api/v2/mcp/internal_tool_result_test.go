package mcp

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth"
)

func TestInternalToolResultPreservesNoContentSuccess(t *testing.T) {
	for _, test := range []struct {
		name   string
		status int
		body   string
		failed bool
		text   string
	}{
		{"no content", http.StatusNoContent, "", false, "{}"},
		{"no content whitespace", http.StatusNoContent, " \n", false, "{}"},
		{"json success", http.StatusOK, `{"ok":true}`, false, `{"ok":true}`},
		{"missing json", http.StatusOK, "", true, ""},
		{"malformed no content", http.StatusNoContent, "invalid", true, ""},
		{"empty server failure", http.StatusInternalServerError, "", true, ""},
		{"json server failure", http.StatusInternalServerError, `{"private":"details"}`, true, ""},
		{"safe validation failure", http.StatusBadRequest, `{"message":"invalid input"}`, true, `{"message":"invalid input"}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			permission := "models.chat.participant.delete"
			resolver := &recordingInternalPermissionResolver{resolution: auth.PermissionResolution{UserID: 41, Permissions: []string{permission}}}
			handler := NewHandler(nil, nil, nil, resolver)
			request := httptest.NewRequest(http.MethodPost, "/app/7/mcp/elitea_core/chat", nil)
			request = request.WithContext(auth.ContextWithUser(request.Context(), auth.User{ID: "41", UserID: "41"}))
			calls := 0
			result := handler.callInternalTool(request, 7, Tool{Name: "delete_elitea_core_participant", permission: permission}, nil, "chat", func(int64) (internalApplicationExecution, error) {
				calls++
				return internalApplicationExecution{status: test.status, body: []byte(test.body)}, nil
			})
			if (result["isError"] == true) != test.failed || calls != 1 {
				t.Fatalf("result=%+v calls=%d", result, calls)
			}
			content := result["content"].([]map[string]any)
			want := test.text
			if want == "" {
				want = "the internal chat operation failed; nothing else was disclosed"
			}
			if len(content) != 1 || content[0]["text"] != want {
				t.Fatalf("content=%+v want=%q", content, want)
			}
		})
	}
}
