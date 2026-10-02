package output

import (
	runtimev1 "github.com/EliteaAI/elitea-platform/libs/proto/gen/go/elitea/runtime/v1"
	"testing"
)

func TestChildInputFailureAdmitsOnlyRegisteredMessage(t *testing.T) {
	code := runtimev1.RuntimeErrorCodeV1_RUNTIME_ERROR_CODE_V1_PIPELINE_INPUT_INVALID
	payload := &runtimev1.RuntimeErrorV1{Code: code, SafeMessage: childInputTypeSafeMessage}
	policy, ok := runtimeFailurePolicyForError(payload)
	if !ok || policy.Code != "PIPELINE_INPUT_INVALID" || policy.SafeMessage != payload.GetSafeMessage() || policy.Retryable {
		t.Fatalf("child input policy not preserved: %+v", policy)
	}
	payload.SafeMessage += " untrusted detail"
	policy, _ = runtimeFailurePolicyForError(payload)
	if policy.SafeMessage == payload.GetSafeMessage() {
		t.Fatal("unregistered worker text admitted")
	}
	payload.SafeMessage = childInputTypeSafeMessage
	payload.Code = runtimev1.RuntimeErrorCodeV1_RUNTIME_ERROR_CODE_V1_INTERNAL
	policy, _ = runtimeFailurePolicyForError(payload)
	if policy.SafeMessage == payload.GetSafeMessage() {
		t.Fatal("child message admitted under wrong code")
	}
}
