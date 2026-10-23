package output

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	runtimev1 "github.com/elitea-ng/elitea-platform/libs/proto/gen/go/elitea/runtime/v1"
)

// FU-244: the Worker's data-free composition refusal is admitted only as the
// exact registered text of the shared contract under UNSUPPORTED_CAPABILITY,
// and keeps its own public code.
func TestApplicationCompositionFailureAdmitsOnlyTheRegisteredMessage(t *testing.T) {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot locate the test file")
	}
	raw, err := os.ReadFile(filepath.Join(filepath.Dir(file), "../../../../../../libs/proto/contracts/application-composition-limits-v1.json"))
	if err != nil {
		t.Fatalf("read the composition contract: %v", err)
	}
	var contract struct {
		RuntimeMessage     string `json:"runtime_message"`
		SizeRuntimeMessage string `json:"size_runtime_message"`
		PublicCode         string `json:"public_code"`
	}
	if err := json.Unmarshal(raw, &contract); err != nil {
		t.Fatalf("decode the composition contract: %v", err)
	}
	if contract.RuntimeMessage != applicationCompositionInvalidSafeMessage {
		t.Fatal("the registered message drifted from the shared contract")
	}
	code := runtimev1.RuntimeErrorCodeV1_RUNTIME_ERROR_CODE_V1_UNSUPPORTED_CAPABILITY
	payload := &runtimev1.RuntimeErrorV1{Code: code, SafeMessage: applicationCompositionInvalidSafeMessage}
	policy, ok := runtimeFailurePolicyForError(payload)
	if !ok || policy.Code != contract.PublicCode || policy.SafeMessage != payload.GetSafeMessage() || policy.Retryable {
		t.Fatalf("composition policy not preserved: %+v", policy)
	}
	// The size terminal (a composition too large to load) has its own registered text.
	payload.SafeMessage = contract.SizeRuntimeMessage
	if policy, ok = runtimeFailurePolicyForError(payload); !ok || policy.Code != contract.PublicCode || policy.SafeMessage != contract.SizeRuntimeMessage {
		t.Fatalf("size policy not preserved: %+v", policy)
	}
	payload.SafeMessage = applicationCompositionInvalidSafeMessage + ` Agent "x".`
	if policy, _ = runtimeFailurePolicyForError(payload); policy.SafeMessage == payload.GetSafeMessage() {
		t.Fatal("unregistered worker text admitted")
	}
	payload.SafeMessage = applicationCompositionInvalidSafeMessage
	payload.Code = runtimev1.RuntimeErrorCodeV1_RUNTIME_ERROR_CODE_V1_INVALID_INPUT
	if policy, _ = runtimeFailurePolicyForError(payload); policy.SafeMessage == payload.GetSafeMessage() {
		t.Fatal("composition message admitted under the wrong code")
	}
}
