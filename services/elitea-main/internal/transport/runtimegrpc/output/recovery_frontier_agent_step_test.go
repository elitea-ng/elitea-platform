package output

import (
	"context"
	"testing"

	"google.golang.org/protobuf/proto"

	"github.com/elitea-ng/elitea-platform/libs/go/faults"
	runtimev1 "github.com/elitea-ng/elitea-platform/libs/proto/gen/go/elitea/runtime/v1"
	outputapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/output"
)

// A reclaim refused at a root saved-Agent frontier reaches the chat as
// RECOVERY_FRONTIER_UNSUPPORTED with its own readable text, only under that
// wire code, never retryable; its text is the registry's byte for byte.
func TestRecoveryFrontierAgentStepShowsItsReadableText(t *testing.T) {
	message := recoveryFrontierAgentStepSafeMessage
	if len(message) > outputapp.MaxRuntimeFailureSafeMessageBytes {
		t.Fatalf("text is %d bytes, over the frame bound", len(message))
	}
	code := runtimev1.RuntimeErrorCodeV1_RUNTIME_ERROR_CODE_V1_RECOVERY_FRONTIER_UNSUPPORTED
	payload := &runtimev1.RuntimeErrorV1{Code: code, SafeMessage: message}
	policy, ok := runtimeFailurePolicyForError(payload)
	if !ok || policy.Code != "RECOVERY_FRONTIER_UNSUPPORTED" || policy.SafeMessage != message || policy.Retryable {
		t.Fatalf("policy: %+v", policy)
	}
	payload.SafeMessage += " untrusted detail"
	if policy, _ = runtimeFailurePolicyForError(payload); policy.SafeMessage == payload.GetSafeMessage() {
		t.Fatalf("unregistered worker text admitted: %+v", policy)
	}
	payload.SafeMessage = message
	payload.Code = runtimev1.RuntimeErrorCodeV1_RUNTIME_ERROR_CODE_V1_INTERNAL
	if policy, _ = runtimeFailurePolicyForError(payload); policy.SafeMessage == message {
		t.Fatal("the text was admitted under the wrong code")
	}
	found := false
	for _, entry := range faults.Registry() {
		if entry.RuntimeKind != "RecoveryFrontierAgentStep" {
			continue
		}
		found = true
		if entry.Sentence != message || entry.Retryable != "no" || entry.WireCode != code.String() {
			t.Fatalf("registry %+v, Main %q", entry, message)
		}
	}
	if !found {
		t.Fatal("RecoveryFrontierAgentStep is not registered")
	}
}

// The frame is accepted end to end and settles typed and readable.
func TestOutputServerAcceptsTheRecoveryFrontierAgentStepFrame(t *testing.T) {
	frame := proto.Clone(readCorpusFrame(t, "unsupported")).(*runtimev1.ExecutionOutputFrameV1)
	frame.GetRuntimeError().Code = runtimev1.RuntimeErrorCodeV1_RUNTIME_ERROR_CODE_V1_RECOVERY_FRONTIER_UNSUPPORTED
	frame.GetRuntimeError().SafeMessage = recoveryFrontierAgentStepSafeMessage
	frame.GetRuntimeError().Retryable = false
	rebindFramePayload(t, frame, frame.GetRuntimeError())
	failures := &failureIngestorStub{}
	server := newOutputTestServer(t, &validationIngestorStub{}, failures)
	stream := &outputStreamStub{context: context.Background(), frames: []*runtimev1.ExecutionOutputFrameV1{frame}}
	if err := server.Publish(stream); err != nil {
		t.Fatal(err)
	}
	if len(stream.acks) != 2 || stream.acks[1].GetRejection() != nil || len(failures.frames) != 1 {
		t.Fatalf("rejected: acks=%v", stream.acks)
	}
	got := failures.frames[0].Failure
	if got.Code != "RECOVERY_FRONTIER_UNSUPPORTED" || got.SafeMessage != recoveryFrontierAgentStepSafeMessage || got.Retryable {
		t.Fatalf("failure not preserved: %+v", got)
	}
	if err := failures.frames[0].Validate(); err != nil {
		t.Fatalf("ingested frame is invalid: %v", err)
	}
}
