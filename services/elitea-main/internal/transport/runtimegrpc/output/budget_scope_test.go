package output

import (
	"testing"

	runtimev1 "github.com/EliteaAI/elitea-platform/libs/proto/gen/go/elitea/runtime/v1"
)

// TestBudgetRefusalScopeMapsToPublicCode pins #6732: the scope that refused a
// model call reaches the chat as its own public code, so the copy and the
// Usage link can differ for a member and for the whole project.
func TestBudgetRefusalScopeMapsToPublicCode(t *testing.T) {
	budget := runtimev1.RuntimeErrorCodeV1_RUNTIME_ERROR_CODE_V1_MODEL_BUDGET_EXHAUSTED
	unscoped, ok := runtimeFailurePolicyFor(budget)
	if !ok {
		t.Fatal("the unscoped budget refusal must stay registered")
	}
	for _, tc := range []struct {
		name    string
		message string
		code    string
		ok      bool
	}{
		{"project", projectBudgetExhaustedSafeMessage, "PROJECT_BUDGET_EXHAUSTED", true},
		{"member", memberBudgetExhaustedSafeMessage, "MEMBER_BUDGET_EXHAUSTED", true},
		{"unscoped", unscoped.SafeMessage, "MODEL_BUDGET_EXHAUSTED", true},
		{"provider text", "insufficient_quota: raw provider body", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			policy, ok := runtimeFailurePolicyForError(&runtimev1.RuntimeErrorV1{Code: budget, SafeMessage: tc.message})
			if !ok {
				t.Fatal("policy lookup failed")
			}
			// The caller rejects a frame whose message differs from the policy's,
			// which is how a raw provider text is refused.
			accepted := policy.SafeMessage == tc.message
			if accepted != tc.ok {
				t.Fatalf("accepted = %v, want %v", accepted, tc.ok)
			}
			if tc.ok && policy.Code != tc.code {
				t.Fatalf("code = %q, want %q", policy.Code, tc.code)
			}
			if policy.Retryable {
				t.Fatal("a budget refusal is never retryable")
			}
		})
	}

	// A scoped message under another code is not a budget refusal.
	other := runtimev1.RuntimeErrorCodeV1_RUNTIME_ERROR_CODE_V1_MODEL_ACCESS_DENIED
	if policy, _ := runtimeFailurePolicyForError(&runtimev1.RuntimeErrorV1{Code: other, SafeMessage: memberBudgetExhaustedSafeMessage}); policy.SafeMessage == memberBudgetExhaustedSafeMessage {
		t.Fatal("the member message must not be accepted for another code")
	}
}
