package toolkitrun

import (
	"net/http"
	"net/http/httptest"
	"strings"
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

// A worker declining a tool is a 422 naming the tool, never the generic
// "Configuration type is not supported." 500 (live defect D2).
func TestWriteOutcomeWorkerRefusalsAreTypedClientErrors(t *testing.T) {
	cases := []struct {
		code, reason string
	}{
		{"RUNTIME_ERROR_CODE_V1_UNSUPPORTED_CAPABILITY", ReasonEffectfulToolUnavailable},
		{"RUNTIME_ERROR_CODE_V1_AUTHORIZATION_FAILED", ReasonToolRefused},
	}
	for _, c := range cases {
		recorder := httptest.NewRecorder()
		WriteOutcome(recorder, toolkitcalltoolapp.RunOutcome{
			ExecutionID: "exec-9", Status: toolkitcalltoolapp.RunStatusRuntimeFailure,
			ErrorMessage: "Configuration type is not supported.", FailureCode: c.code,
			ToolName: "add_slide", ToolkitType: "pptx",
		})
		if recorder.Code != http.StatusUnprocessableEntity {
			t.Fatalf("%s: status %d, want 422", c.code, recorder.Code)
		}
		body := decodeBody(t, recorder)
		if body["reason"] != c.reason || body["ok"] != false {
			t.Fatalf("%s: body %v", c.code, body)
		}
		sentence, _ := body["error"].(string)
		if !strings.Contains(sentence, `"add_slide"`) || strings.Contains(sentence, "Configuration type") {
			t.Fatalf("%s: sentence %q", c.code, sentence)
		}
	}
	// Any other failure code stays a 500.
	recorder := httptest.NewRecorder()
	WriteOutcome(recorder, toolkitcalltoolapp.RunOutcome{
		Status: toolkitcalltoolapp.RunStatusRuntimeFailure, FailureCode: "RUNTIME_ERROR_CODE_V1_DEPENDENCY_UNAVAILABLE",
		ErrorMessage: "x",
	})
	if recorder.Code != http.StatusInternalServerError {
		t.Fatalf("status %d, want 500", recorder.Code)
	}
}
