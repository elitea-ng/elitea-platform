package output

import (
	"context"
	"strings"
	"testing"

	"google.golang.org/protobuf/proto"

	"github.com/elitea-ng/elitea-platform/libs/go/faults"
	runtimev1 "github.com/elitea-ng/elitea-platform/libs/proto/gen/go/elitea/runtime/v1"
	outputapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/output"
)

// FU-335: a delegation refused at start reaches the chat as AUTHORIZATION_FAILED
// with its own readable text, only as that exact text under that wire code,
// never retryable; its text is the registry's byte for byte. The generic
// authorization text keeps its own meaning.
func TestDelegationGrantMissingShowsItsReadableText(t *testing.T) {
	message := delegationGrantMissingSafeMessage
	if len(message) > outputapp.MaxRuntimeFailureSafeMessageBytes {
		t.Fatalf("text is %d bytes, over the frame bound", len(message))
	}
	// Review 1 item 3: the sentence stands alone; a client or a path without
	// the notice must not be pointed at one.
	if strings.Contains(message, "notice") {
		t.Fatalf("the sentence points at a notice: %q", message)
	}
	code := runtimev1.RuntimeErrorCodeV1_RUNTIME_ERROR_CODE_V1_AUTHORIZATION_FAILED
	payload := &runtimev1.RuntimeErrorV1{Code: code, SafeMessage: message}
	policy, ok := runtimeFailurePolicyForError(payload)
	if !ok || policy.Code != "AUTHORIZATION_FAILED" || policy.SafeMessage != message || policy.Retryable {
		t.Fatalf("policy: %+v", policy)
	}
	payload.SafeMessage += " The toolkit is 'Team GitHub'."
	if policy, _ = runtimeFailurePolicyForError(payload); policy.SafeMessage == payload.GetSafeMessage() {
		t.Fatalf("unregistered worker text admitted: %+v", policy)
	}
	payload.SafeMessage = message
	payload.Code = runtimev1.RuntimeErrorCodeV1_RUNTIME_ERROR_CODE_V1_INTERNAL
	if policy, _ = runtimeFailurePolicyForError(payload); policy.SafeMessage == message {
		t.Fatal("the text was admitted under the wrong code")
	}
	generic, ok := runtimeFailurePolicyForError(&runtimev1.RuntimeErrorV1{Code: code, SafeMessage: "Execution authorization failed."})
	if !ok || generic.Code != "AUTHORIZATION_FAILED" || generic.SafeMessage != "Execution authorization failed." {
		t.Fatalf("generic policy changed: %+v", generic)
	}
	found := false
	for _, entry := range faults.Registry() {
		if entry.RuntimeKind != "DelegationGrantMissing" {
			continue
		}
		found = true
		if entry.Sentence != message || entry.Retryable != "no" || entry.WireCode != code.String() {
			t.Fatalf("registry %+v, Main %q", entry, message)
		}
	}
	if !found {
		t.Fatal("DelegationGrantMissing is not registered")
	}
}

// The frame is accepted end to end and settles typed and readable.
func TestOutputServerAcceptsTheDelegationGrantMissingFrame(t *testing.T) {
	frame := proto.Clone(readCorpusFrame(t, "unsupported")).(*runtimev1.ExecutionOutputFrameV1)
	frame.GetRuntimeError().Code = runtimev1.RuntimeErrorCodeV1_RUNTIME_ERROR_CODE_V1_AUTHORIZATION_FAILED
	frame.GetRuntimeError().SafeMessage = delegationGrantMissingSafeMessage
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
	if got.Code != "AUTHORIZATION_FAILED" || got.SafeMessage != delegationGrantMissingSafeMessage || got.Retryable {
		t.Fatalf("failure not preserved: %+v", got)
	}
	if err := failures.frames[0].Validate(); err != nil {
		t.Fatalf("ingested frame is invalid: %v", err)
	}
}
