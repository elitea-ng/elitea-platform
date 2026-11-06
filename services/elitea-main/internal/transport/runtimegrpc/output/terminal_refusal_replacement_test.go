package output

import (
	"context"
	"strings"
	"testing"

	runtimev1 "github.com/elitea-ng/elitea-platform/libs/proto/gen/go/elitea/runtime/v1"
	"google.golang.org/protobuf/proto"
)

// FU-290: Main refuses a terminal runtime-failure frame whose text is not
// registered (here 257 bytes, one over the bound) with a non-retryable
// PROTOCOL_VIOLATION and no desired state, projecting nothing. The Worker then
// replaces that exact frame with the canonical INTERNAL failure at the same
// sequence on a new stream, and Main accepts it. The Worker-side replacement is
// proven in the Worker tests; this test pins the Main contract it depends on.
func TestRefusedTerminalFailureIsFollowedByAcceptedCanonicalInternalFailure(t *testing.T) {
	failures := &failureIngestorStub{}
	server := newOutputTestServer(t, &validationIngestorStub{}, failures)

	refused := proto.Clone(readCorpusFrame(t, "unsupported")).(*runtimev1.ExecutionOutputFrameV1)
	refused.GetRuntimeError().Code = runtimev1.RuntimeErrorCodeV1_RUNTIME_ERROR_CODE_V1_INTERNAL
	refused.GetRuntimeError().SafeMessage = strings.Repeat("x", 257)
	refused.GetRuntimeError().Retryable = false
	rebindFramePayload(t, refused, refused.GetRuntimeError())
	first := &outputStreamStub{context: context.Background(), frames: []*runtimev1.ExecutionOutputFrameV1{refused}}
	if err := server.Publish(first); err != nil {
		t.Fatal(err)
	}
	if len(failures.frames) != 0 || len(first.acks) != 2 {
		t.Fatalf("the refused terminal must project nothing: frames=%d acks=%d", len(failures.frames), len(first.acks))
	}
	ack := first.acks[1]
	if ack.GetRejection().GetCode() != runtimev1.RuntimeErrorCodeV1_RUNTIME_ERROR_CODE_V1_PROTOCOL_VIOLATION ||
		ack.GetRejection().GetRetryable() ||
		ack.GetDesiredState() != runtimev1.DesiredExecutionStateV1_DESIRED_EXECUTION_STATE_V1_UNSPECIFIED ||
		ack.GetCommittedContiguousSequence() != 0 {
		t.Fatalf("unexpected refusal ack: %v", ack)
	}

	replacement := proto.Clone(refused).(*runtimev1.ExecutionOutputFrameV1)
	replacement.GetRuntimeError().SafeMessage = "The runtime operation failed."
	rebindFramePayload(t, replacement, replacement.GetRuntimeError())
	second := &outputStreamStub{context: context.Background(), frames: []*runtimev1.ExecutionOutputFrameV1{replacement}}
	if err := server.Publish(second); err != nil {
		t.Fatal(err)
	}
	if len(failures.frames) != 1 || len(second.acks) != 2 {
		t.Fatalf("the canonical replacement must be accepted: frames=%d acks=%d", len(failures.frames), len(second.acks))
	}
	if second.acks[1].GetRejection() != nil || second.acks[1].GetCommittedContiguousSequence() != replacement.GetSequence() {
		t.Fatalf("unexpected replacement ack: %v", second.acks[1])
	}
	if failures.frames[0].Failure.Code != "INTERNAL" || failures.frames[0].Failure.SafeMessage != "The runtime operation failed." {
		t.Fatalf("projected failure = %+v", failures.frames[0].Failure)
	}
}
