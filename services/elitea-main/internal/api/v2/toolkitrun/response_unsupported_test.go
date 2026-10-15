package toolkitrun

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	toolkitcalltoolapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/toolkitcalltool"
	executiondomain "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/execution"
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

// Worker refusals are typed answers, never the generic
// "Configuration type is not supported." 500 (live defect D2).
func TestWriteOutcomeWorkerRefusalsAreTyped(t *testing.T) {
	refusal := func(status toolkitcalltoolapp.RunStatus, code, message, tool string) toolkitcalltoolapp.RunOutcome {
		return toolkitcalltoolapp.RunOutcome{
			ExecutionID: "exec-9", Status: status, FailureCode: code, ErrorMessage: message, ToolName: tool,
		}
	}
	cases := []struct {
		name    string
		outcome toolkitcalltoolapp.RunOutcome
		status  int
		reason  string
		has     string
	}{
		{"effectful (typed)", refusal(toolkitcalltoolapp.RunStatusEffectRefused, "", executiondomain.ToolkitEffectfulToolMessage, "add_slide"),
			http.StatusUnprocessableEntity, ReasonEffectfulToolUnavailable, `The tool "add_slide" changes data`},
		{"policy (typed)", refusal(toolkitcalltoolapp.RunStatusPolicyRefused, "", executiondomain.ToolkitPolicyRefusedMessage, "x"),
			http.StatusUnprocessableEntity, ReasonToolRefused, "blocked by this deployment's tool policy"},
		{"sensitive (typed)", refusal(toolkitcalltoolapp.RunStatusSensitiveRefused, "", executiondomain.ToolkitPolicyRefusedMessage, "x"),
			http.StatusUnprocessableEntity, ReasonToolRefused, "marked sensitive"},
		{"effectful, no tool name", refusal(toolkitcalltoolapp.RunStatusEffectRefused, "", executiondomain.ToolkitEffectfulToolMessage, " "),
			http.StatusUnprocessableEntity, ReasonEffectfulToolUnavailable, "This tool changes data"},
		// #1292 round 3: UNSUPPORTED_CAPABILITY is NOT "changes data".
		{"unsupported capability is neutral", refusal(toolkitcalltoolapp.RunStatusRuntimeFailure, "RUNTIME_ERROR_CODE_V1_UNSUPPORTED_CAPABILITY", "Configuration type is not supported.", "add_slide"),
			http.StatusUnprocessableEntity, ReasonUnsupportedTool, `worker cannot run the tool "add_slide"`},
		{"run credentials refused", refusal(toolkitcalltoolapp.RunStatusRuntimeFailure, "RUNTIME_ERROR_CODE_V1_AUTHORIZATION_FAILED", executiondomain.ToolkitRunCredentialsRefusedMessage, "add_slide"),
			http.StatusForbidden, ReasonAuthorizationFailed, "credentials or context"},
		// A Python-shaped AUTHORIZATION_FAILED is the credentials too.
		{"python worker authorization failed", refusal(toolkitcalltoolapp.RunStatusRuntimeFailure, "RUNTIME_ERROR_CODE_V1_AUTHORIZATION_FAILED", "Execution authorization failed.", "add_slide"),
			http.StatusForbidden, ReasonAuthorizationFailed, "Try again"},
		{"a genuinely unknown tool stays unknown", refusal(toolkitcalltoolapp.RunStatusUnknownTool, "", "no such tool", "nope"),
			http.StatusUnprocessableEntity, "unknown_tool", "no such tool"},
		{"other failures stay 500", refusal(toolkitcalltoolapp.RunStatusRuntimeFailure, "RUNTIME_ERROR_CODE_V1_DEPENDENCY_UNAVAILABLE", "x", "t"),
			http.StatusInternalServerError, "runtime_failure", "x"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			WriteOutcome(recorder, c.outcome)
			if recorder.Code != c.status {
				t.Fatalf("status %d, want %d", recorder.Code, c.status)
			}
			body := decodeBody(t, recorder)
			sentence, _ := body["error"].(string)
			if body["reason"] != c.reason || body["ok"] != false || !strings.Contains(sentence, c.has) {
				t.Fatalf("body %v", body)
			}
			if strings.Contains(sentence, "test panel") || strings.Contains(sentence, "Configuration type") {
				t.Fatalf("caller-specific or generic wording: %q", sentence)
			}
			if c.reason != ReasonAuthorizationFailed && strings.Contains(sentence, "credentials") {
				t.Fatalf("a tool refusal tells the person to retry credentials: %q", sentence)
			}
			if c.reason != ReasonEffectfulToolUnavailable && strings.Contains(sentence, "changes data") {
				t.Fatalf("only EFFECT_REFUSED says the tool changes data: %q", sentence)
			}
		})
	}
}

// The Rust worker (execution/toolkit_delivery_processor.rs) sends these exact
// literals; this pins Main's side of the pair.
func TestWorkerRefusalLiteralsMatchTheWorker(t *testing.T) {
	if executiondomain.ToolkitEffectfulToolMessage != "This tool changes data, and this path runs read-only tools only." ||
		executiondomain.ToolkitPolicyRefusedMessage != "This tool is blocked or sensitive under this deployment's policy and cannot run here." ||
		executiondomain.ToolkitRunCredentialsRefusedMessage != "The platform could not authorize this tool run's credentials or context." {
		t.Fatal("the worker's refusal literals changed")
	}
}
