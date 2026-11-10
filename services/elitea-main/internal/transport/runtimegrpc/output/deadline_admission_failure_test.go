package output

import (
	"context"
	"testing"

	"google.golang.org/protobuf/proto"

	"github.com/elitea-ng/elitea-platform/libs/go/faults"
	runtimev1 "github.com/elitea-ng/elitea-platform/libs/proto/gen/go/elitea/runtime/v1"
	outputapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/output"
)

// W3-1: the two deadline-admission failures reach the chat with their own
// readable text, under their wire code only, never retryable.
func TestDeadlineAdmissionFailuresShowTheirReadableText(t *testing.T) {
	for _, tc := range []struct {
		message string
		code    runtimev1.RuntimeErrorCodeV1
		public  string
	}{
		{codeDeadlineInsufficientSafeMessage, runtimev1.RuntimeErrorCodeV1_RUNTIME_ERROR_CODE_V1_PIPELINE_CODE_FAILED, "PIPELINE_CODE_FAILED"},
		{fanoutDeadlineInsufficientSafeMessage, runtimev1.RuntimeErrorCodeV1_RUNTIME_ERROR_CODE_V1_PIPELINE_TOOL_FAILED, "PIPELINE_TOOL_FAILED"},
	} {
		if len(tc.message) > outputapp.MaxRuntimeFailureSafeMessageBytes {
			t.Fatalf("%q is %d bytes, over the frame bound", tc.message, len(tc.message))
		}
		payload := &runtimev1.RuntimeErrorV1{Code: tc.code, SafeMessage: tc.message}
		policy, ok := runtimeFailurePolicyForError(payload)
		if !ok || policy.Code != tc.public || policy.SafeMessage != tc.message || policy.Retryable {
			t.Fatalf("policy for %q: %+v", tc.message, policy)
		}
		payload.SafeMessage += " untrusted detail"
		if policy, _ = runtimeFailurePolicyForError(payload); policy.SafeMessage == payload.GetSafeMessage() {
			t.Fatalf("unregistered worker text admitted: %+v", policy)
		}
		payload.SafeMessage = tc.message
		payload.Code = runtimev1.RuntimeErrorCodeV1_RUNTIME_ERROR_CODE_V1_INTERNAL
		if policy, _ = runtimeFailurePolicyForError(payload); policy.SafeMessage == tc.message {
			t.Fatalf("%q admitted under the wrong code", tc.message)
		}
	}
}

// Main's texts are the shared registry's, byte for byte; the Worker's parity
// test pins the same entries to its own table.
func TestDeadlineAdmissionTextsMatchTheFaultRegistry(t *testing.T) {
	want := map[string]string{
		"CodeDeadlineInsufficient":   codeDeadlineInsufficientSafeMessage,
		"FanoutDeadlineInsufficient": fanoutDeadlineInsufficientSafeMessage,
		"FanoutAgentCallsPipeline":   fanoutAgentCallsPipelineSafeMessage,
	}
	for _, entry := range faults.Registry() {
		message, ok := want[entry.RuntimeKind]
		if !ok {
			continue
		}
		if entry.Sentence != message || entry.Retryable != "no" {
			t.Errorf("%s: registry %q (retryable %s), Main %q", entry.Code, entry.Sentence, entry.Retryable, message)
		}
		delete(want, entry.RuntimeKind)
	}
	if len(want) != 0 {
		t.Fatalf("not registered: %v", want)
	}
}

// W3-1: both frames are accepted end to end and settle typed and readable.
func TestOutputServerAcceptsDeadlineAdmissionFailureFrames(t *testing.T) {
	for _, tc := range []struct {
		message string
		code    runtimev1.RuntimeErrorCodeV1
		public  string
	}{
		{codeDeadlineInsufficientSafeMessage, runtimev1.RuntimeErrorCodeV1_RUNTIME_ERROR_CODE_V1_PIPELINE_CODE_FAILED, "PIPELINE_CODE_FAILED"},
		{fanoutDeadlineInsufficientSafeMessage, runtimev1.RuntimeErrorCodeV1_RUNTIME_ERROR_CODE_V1_PIPELINE_TOOL_FAILED, "PIPELINE_TOOL_FAILED"},
	} {
		frame := proto.Clone(readCorpusFrame(t, "unsupported")).(*runtimev1.ExecutionOutputFrameV1)
		frame.GetRuntimeError().Code = tc.code
		frame.GetRuntimeError().SafeMessage = tc.message
		frame.GetRuntimeError().Retryable = false
		rebindFramePayload(t, frame, frame.GetRuntimeError())
		failures := &failureIngestorStub{}
		server := newOutputTestServer(t, &validationIngestorStub{}, failures)
		stream := &outputStreamStub{context: context.Background(), frames: []*runtimev1.ExecutionOutputFrameV1{frame}}
		if err := server.Publish(stream); err != nil {
			t.Fatal(err)
		}
		if len(stream.acks) != 2 || stream.acks[1].GetRejection() != nil || len(failures.frames) != 1 {
			t.Fatalf("%q rejected: acks=%v", tc.message, stream.acks)
		}
		ingested := failures.frames[0]
		if ingested.Failure.Code != tc.public || ingested.Failure.SafeMessage != tc.message || ingested.Failure.Retryable {
			t.Fatalf("failure not preserved: %+v", ingested.Failure)
		}
		if err := ingested.Validate(); err != nil {
			t.Fatalf("ingested frame is invalid: %v", err)
		}
	}
}

// C3, version skew. A Main that does not know a failure the Worker sends
// (a text it never registered, or a wire code it has no policy for) refuses
// the terminal frame with a bound, non-retryable PROTOCOL_VIOLATION and
// projects nothing: the same bytes are never accepted, so the Worker never
// resends them (no loop). The Worker then replaces the terminal once with
// the older kind of the same wire code (protocol/output.rs
// refused_terminal_replacement), which this Main accepts as a typed,
// readable terminal failure, so the run ends instead of hanging.
func TestAnUnknownFailureIsRefusedOnceAndItsSameCodeFallbackEndsTheRunTyped(t *testing.T) {
	for _, unknown := range []*runtimev1.RuntimeErrorV1{
		// A newer Worker's text under a code this Main knows.
		{Code: runtimev1.RuntimeErrorCodeV1_RUNTIME_ERROR_CODE_V1_PIPELINE_CODE_FAILED, SafeMessage: "A newer Worker's readable Code failure this Main never registered."},
		// A newer Worker's wire code this Main has no policy for.
		{Code: runtimev1.RuntimeErrorCodeV1(1 << 20), SafeMessage: "A newer Worker's failure under a code this Main does not know."},
	} {
		if _, ok := runtimeFailurePolicyForError(unknown); ok && unknown.GetCode() != runtimev1.RuntimeErrorCodeV1_RUNTIME_ERROR_CODE_V1_PIPELINE_CODE_FAILED {
			t.Fatalf("code %v has a policy", unknown.GetCode())
		}
		failures := &failureIngestorStub{}
		server := newOutputTestServer(t, &validationIngestorStub{}, failures)
		refused := proto.Clone(readCorpusFrame(t, "unsupported")).(*runtimev1.ExecutionOutputFrameV1)
		refused.GetRuntimeError().Code = unknown.GetCode()
		refused.GetRuntimeError().SafeMessage = unknown.GetSafeMessage()
		refused.GetRuntimeError().Retryable = false
		rebindFramePayload(t, refused, refused.GetRuntimeError())
		first := &outputStreamStub{context: context.Background(), frames: []*runtimev1.ExecutionOutputFrameV1{refused}}
		if err := server.Publish(first); err != nil {
			t.Fatal(err)
		}
		if len(failures.frames) != 0 || len(first.acks) != 2 {
			t.Fatalf("the unknown failure must project nothing: frames=%d acks=%d", len(failures.frames), len(first.acks))
		}
		ack := first.acks[1]
		if ack.GetRejection().GetCode() != runtimev1.RuntimeErrorCodeV1_RUNTIME_ERROR_CODE_V1_PROTOCOL_VIOLATION ||
			ack.GetRejection().GetRetryable() ||
			ack.GetDesiredState() != runtimev1.DesiredExecutionStateV1_DESIRED_EXECUTION_STATE_V1_UNSPECIFIED {
			t.Fatalf("the refusal must be bound and final: %v", ack)
		}

		replacement := proto.Clone(refused).(*runtimev1.ExecutionOutputFrameV1)
		replacement.GetRuntimeError().Code = runtimev1.RuntimeErrorCodeV1_RUNTIME_ERROR_CODE_V1_PIPELINE_CODE_FAILED
		replacement.GetRuntimeError().SafeMessage = "The pipeline stopped at a Code node. Check its source, selected input, and sandbox limits. Later nodes did not run. Share the support reference with your administrator before retrying."
		rebindFramePayload(t, replacement, replacement.GetRuntimeError())
		second := &outputStreamStub{context: context.Background(), frames: []*runtimev1.ExecutionOutputFrameV1{replacement}}
		if err := server.Publish(second); err != nil {
			t.Fatal(err)
		}
		if len(failures.frames) != 1 || second.acks[1].GetRejection() != nil {
			t.Fatalf("the same-code fallback must be accepted: frames=%d acks=%v", len(failures.frames), second.acks)
		}
		got := failures.frames[0].Failure
		if got.Code != "PIPELINE_CODE_FAILED" || got.Retryable || got.SafeMessage != replacement.GetRuntimeError().GetSafeMessage() {
			t.Fatalf("the run did not end typed: %+v", got)
		}
	}
}
