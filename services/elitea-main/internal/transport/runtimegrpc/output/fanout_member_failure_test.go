package output

import (
	"testing"

	runtimev1 "github.com/elitea-ng/elitea-platform/libs/proto/gen/go/elitea/runtime/v1"
)

// FU-288: a Map item or Parallel branch that failed under On Error `stop`
// reaches the chat with its own readable text, and only that exact text.
func TestFanoutMemberFailureShowsItsReadableText(t *testing.T) {
	code := runtimev1.RuntimeErrorCodeV1_RUNTIME_ERROR_CODE_V1_PIPELINE_TOOL_FAILED
	payload := &runtimev1.RuntimeErrorV1{Code: code, SafeMessage: fanoutMemberFailedSafeMessage}
	policy, ok := runtimeFailurePolicyForError(payload)
	if !ok || policy.Code != "PIPELINE_TOOL_FAILED" || policy.SafeMessage != fanoutMemberFailedSafeMessage || policy.Retryable {
		t.Fatalf("fan-out member failure policy: %+v", policy)
	}
	payload.SafeMessage += " untrusted detail"
	policy, _ = runtimeFailurePolicyForError(payload)
	if policy.Code != "PIPELINE_TOOL_FAILED" || policy.SafeMessage == payload.GetSafeMessage() {
		t.Fatalf("unregistered worker text admitted: %+v", policy)
	}
	payload.SafeMessage = fanoutMemberFailedSafeMessage
	payload.Code = runtimev1.RuntimeErrorCodeV1_RUNTIME_ERROR_CODE_V1_INTERNAL
	policy, _ = runtimeFailurePolicyForError(payload)
	if policy.SafeMessage == fanoutMemberFailedSafeMessage {
		t.Fatal("fan-out text admitted under the wrong code")
	}
}
