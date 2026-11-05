package output

import (
	"context"
	"testing"

	"google.golang.org/protobuf/proto"

	runtimev1 "github.com/elitea-ng/elitea-platform/libs/proto/gen/go/elitea/runtime/v1"
	outputapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/output"
)

// FU-288: a Map item or Parallel branch that failed under On Error `stop`
// reaches the chat with its own readable text, and only that exact text.
func TestFanoutMemberFailureShowsItsReadableText(t *testing.T) {
	if len(fanoutMemberFailedSafeMessage) > outputapp.MaxRuntimeFailureSafeMessageBytes {
		t.Fatalf("fan-out failure text is %d bytes, over the frame bound", len(fanoutMemberFailedSafeMessage))
	}
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

// FU-288: the Worker's fan-out failure frame is accepted end to end and the
// ingested frame passes the application's own validation (its 256-byte safe
// message bound), so the run settles with the readable text.
func TestOutputServerAcceptsFanoutMemberFailureFrame(t *testing.T) {
	frame := proto.Clone(readCorpusFrame(t, "unsupported")).(*runtimev1.ExecutionOutputFrameV1)
	frame.GetRuntimeError().Code = runtimev1.RuntimeErrorCodeV1_RUNTIME_ERROR_CODE_V1_PIPELINE_TOOL_FAILED
	frame.GetRuntimeError().SafeMessage = fanoutMemberFailedSafeMessage
	frame.GetRuntimeError().Retryable = false
	rebindFramePayload(t, frame, frame.GetRuntimeError())
	failures := &failureIngestorStub{}
	server := newOutputTestServer(t, &validationIngestorStub{}, failures)
	stream := &outputStreamStub{context: context.Background(), frames: []*runtimev1.ExecutionOutputFrameV1{frame}}
	if err := server.Publish(stream); err != nil {
		t.Fatal(err)
	}
	if len(stream.acks) != 2 || stream.acks[1].GetRejection() != nil || len(failures.frames) != 1 {
		t.Fatalf("fan-out failure frame rejected: acks=%v", stream.acks)
	}
	ingested := failures.frames[0]
	if ingested.Failure.Code != "PIPELINE_TOOL_FAILED" || ingested.Failure.SafeMessage != fanoutMemberFailedSafeMessage || ingested.Failure.Retryable {
		t.Fatalf("fan-out failure not preserved: %+v", ingested.Failure)
	}
	if err := ingested.Validate(); err != nil {
		t.Fatalf("ingested fan-out failure frame is invalid: %v", err)
	}
}
