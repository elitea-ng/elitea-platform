package toolkitrun

import (
	"net/http"
	"net/http/httptest"
	"testing"

	toolkitcalltoolapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/toolkitcalltool"
)

// UI-DC-1: the 422 `error` is the capability reason as one sentence. It used
// to be err.Error(), which put the sentinel prefix and the type in front:
// "toolkit type cannot be run by this deployment: ado_boards: This deployment's…".
func TestWriteErrorAnswersTheUnsupportedTypeAsASentence(t *testing.T) {
	const reason = "This deployment's agent worker does not support the ado_boards toolkit."
	for name, testCase := range map[string]struct {
		err  error
		want string
	}{
		"a capability refusal": {
			&toolkitcalltoolapp.UnsupportedToolkitTypeError{ToolkitType: "ado_boards", Reason: reason}, reason,
		},
		"a bare sentinel": {
			toolkitcalltoolapp.ErrUnsupportedToolkitType, "This deployment cannot run this toolkit type.",
		},
	} {
		t.Run(name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			WriteError(recorder, testCase.err)
			if recorder.Code != http.StatusUnprocessableEntity {
				t.Fatalf("status %d, want 422", recorder.Code)
			}
			body := decodeBody(t, recorder)
			if body["error"] != testCase.want || body["reason"] != "unsupported_toolkit" {
				t.Fatalf("body = %v, want error %q", body, testCase.want)
			}
		})
	}
}
