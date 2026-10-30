package output

import (
	"testing"

	runtimev1 "github.com/elitea-ng/elitea-platform/libs/proto/gen/go/elitea/runtime/v1"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/outputcontract"
)

// ADR-0043 decision 2: the worker ends a run whose final answer did not meet
// its output contract after one retry under MODEL_RESPONSE_INVALID with the
// registered sentence (the Rust worker's protocol/output.rs
// OUTPUT_CONTRACT_VIOLATION_MESSAGE), not retryable. Main shows it as
// OUTPUT_CONTRACT_VIOLATION; any other MODEL_RESPONSE_INVALID keeps its code.
func TestWorkerOutputContractViolationMapsToItsCode(t *testing.T) {
	t.Parallel()
	invalid := runtimev1.RuntimeErrorCodeV1_RUNTIME_ERROR_CODE_V1_MODEL_RESPONSE_INVALID
	policy, ok := runtimeFailurePolicyForError(&runtimev1.RuntimeErrorV1{Code: invalid, SafeMessage: outputcontract.ViolationSafeMessage})
	if !ok || policy.Code != outputcontract.ViolationCode || policy.SafeMessage != outputcontract.ViolationSafeMessage || policy.Retryable {
		t.Fatalf("violation policy = %+v, ok = %t", policy, ok)
	}
	plain, ok := runtimeFailurePolicyForError(&runtimev1.RuntimeErrorV1{Code: invalid, SafeMessage: "The model returned an incomplete or invalid response. Try again or select another model. Report repeated failures to an administrator.", Retryable: true})
	if !ok || plain.Code != "MODEL_RESPONSE_INVALID" || !plain.Retryable {
		t.Fatalf("plain MODEL_RESPONSE_INVALID = %+v", plain)
	}
	// The sentence under another code is not the violation.
	other, _ := runtimeFailurePolicyForError(&runtimev1.RuntimeErrorV1{Code: runtimev1.RuntimeErrorCodeV1_RUNTIME_ERROR_CODE_V1_INTERNAL, SafeMessage: outputcontract.ViolationSafeMessage})
	if other.Code == outputcontract.ViolationCode {
		t.Fatal("the violation sentence was mapped under INTERNAL")
	}
}
