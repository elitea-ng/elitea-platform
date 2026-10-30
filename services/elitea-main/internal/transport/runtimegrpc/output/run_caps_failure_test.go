package output

import (
	"testing"

	runtimev1 "github.com/elitea-ng/elitea-platform/libs/proto/gen/go/elitea/runtime/v1"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/runcaps"
)

// ADR-0041 decision 3: the worker stops a run at a cap under
// RESOURCE_EXHAUSTED with the cap's registered sentence (the Rust worker's
// protocol/output.rs CAP_REACHED_*_MESSAGE). Main shows it as CAP_REACHED
// with the cap's name; any other RESOURCE_EXHAUSTED text keeps its old code.
func TestWorkerCapReachedMapsToCapReached(t *testing.T) {
	t.Parallel()
	exhausted := runtimev1.RuntimeErrorCodeV1_RUNTIME_ERROR_CODE_V1_RESOURCE_EXHAUSTED
	for _, tc := range []struct {
		cap     string
		message string
	}{
		{runcaps.MaxToolCalls, "The run stopped because it reached the maximum number of tool calls an administrator set. The task may be incomplete. Check what it completed before you continue."},
		{runcaps.MaxModelTurns, "The run stopped because it reached the maximum number of model turns an administrator set. The task may be incomplete. Check what it completed before you continue."},
		{runcaps.MaxRecordsRead, "The run stopped because it reached the maximum number of records read an administrator set. The task may be incomplete. Check what it completed before you continue."},
	} {
		policy, ok := runtimeFailurePolicyForError(&runtimev1.RuntimeErrorV1{Code: exhausted, SafeMessage: tc.message})
		if !ok || policy.Code != runcaps.Code || policy.Cap != tc.cap || policy.SafeMessage != tc.message || policy.Retryable {
			t.Fatalf("%s: policy = %+v, ok = %t", tc.cap, policy, ok)
		}
		if runcaps.Message(tc.cap) != tc.message {
			t.Fatalf("%s: Main's sentence drifted from the worker's", tc.cap)
		}
	}
	policy, ok := runtimeFailurePolicyForError(&runtimev1.RuntimeErrorV1{Code: exhausted, SafeMessage: "The execution exceeded an approved resource limit."})
	if !ok || policy.Code != "RESOURCE_EXHAUSTED" || policy.Cap != "" {
		t.Fatalf("plain RESOURCE_EXHAUSTED = %+v", policy)
	}
	// Main-side caps never come from the worker.
	for _, cap := range []string{runcaps.MaxWallSeconds, runcaps.MaxSpend} {
		if policy, _ := runtimeFailurePolicyForError(&runtimev1.RuntimeErrorV1{Code: exhausted, SafeMessage: runcaps.Message(cap)}); policy.Code == runcaps.Code {
			t.Fatalf("the worker reported the Main-side cap %s", cap)
		}
	}
}
