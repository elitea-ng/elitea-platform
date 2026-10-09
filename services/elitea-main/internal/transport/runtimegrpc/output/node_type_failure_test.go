package output

import (
	"testing"

	runtimev1 "github.com/EliteaAI/elitea-platform/libs/proto/gen/go/elitea/runtime/v1"
)

// The Worker's data-free "node type not available on this deployment" refusal
// is admitted only as its exact registered text under UNSUPPORTED_CAPABILITY.
func TestNodeTypeNotAvailableFailureAdmitsOnlyRegisteredMessage(t *testing.T) {
	code := runtimev1.RuntimeErrorCodeV1_RUNTIME_ERROR_CODE_V1_UNSUPPORTED_CAPABILITY
	payload := &runtimev1.RuntimeErrorV1{Code: code, SafeMessage: nodeTypeNotAvailableSafeMessage}
	policy, ok := runtimeFailurePolicyForError(payload)
	if !ok || policy.Code != "PIPELINE_NODE_TYPE_NOT_AVAILABLE" || policy.SafeMessage != payload.GetSafeMessage() || policy.Retryable {
		t.Fatalf("node type policy not preserved: %+v", policy)
	}
	payload.SafeMessage = nodeTypeNotAvailableSafeMessage + ` Node "split".`
	if policy, _ = runtimeFailurePolicyForError(payload); policy.SafeMessage == payload.GetSafeMessage() {
		t.Fatal("unregistered worker text admitted")
	}
	payload.SafeMessage = nodeTypeNotAvailableSafeMessage
	payload.Code = runtimev1.RuntimeErrorCodeV1_RUNTIME_ERROR_CODE_V1_INVALID_INPUT
	if policy, _ = runtimeFailurePolicyForError(payload); policy.SafeMessage == payload.GetSafeMessage() {
		t.Fatal("node type message admitted under the wrong code")
	}
	// The generic unsupported text keeps its own public code.
	generic, ok := runtimeFailurePolicyForError(&runtimev1.RuntimeErrorV1{Code: code, SafeMessage: "Configuration type is not supported."})
	if !ok || generic.Code != "UNSUPPORTED_CAPABILITY" {
		t.Fatalf("generic policy changed: %+v", generic)
	}
}
