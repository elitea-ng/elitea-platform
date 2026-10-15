package codeplatform

import (
	"testing"

	toolkit "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/toolkitcalltool"
)

// #1292 round 2: a tool the worker refuses is classified on its typed status
// (and, for an older worker, its runtime failure code), never read as a
// dependency being down.
func TestToolOutcomeClassifiesWorkerRefusals(t *testing.T) {
	for _, tc := range []struct {
		name   string
		result toolkit.RunOutcome
		want   string
	}{
		{"effect refused", toolkit.RunOutcome{Status: toolkit.RunStatusEffectRefused}, "unsupported_operation"},
		{"policy refused", toolkit.RunOutcome{Status: toolkit.RunStatusPolicyRefused}, "authorization_denied"},
		{"unknown tool", toolkit.RunOutcome{Status: toolkit.RunStatusUnknownTool}, "unsupported_operation"},
		{"older worker effect", toolkit.RunOutcome{Status: toolkit.RunStatusRuntimeFailure, FailureCode: toolkit.FailureCodeUnsupportedCapability}, "unsupported_operation"},
		{"older worker authorization", toolkit.RunOutcome{Status: toolkit.RunStatusRuntimeFailure, FailureCode: toolkit.FailureCodeAuthorizationFailed, ErrorMessage: "Execution authorization failed."}, "unsupported_operation"},
		{"provider down", toolkit.RunOutcome{Status: toolkit.RunStatusRuntimeFailure, FailureCode: "RUNTIME_ERROR_CODE_V1_DEPENDENCY_UNAVAILABLE"}, "dependency_unavailable"},
		{"tool error", toolkit.RunOutcome{Status: toolkit.RunStatusToolError}, "dependency_unavailable"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := toolOutcome(tc.result, "owner")
			if err != nil || got.Status != tc.want {
				t.Fatalf("status %q (%v), want %q", got.Status, err, tc.want)
			}
		})
	}
}
