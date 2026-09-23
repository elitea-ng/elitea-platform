package output

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	runtimev1 "github.com/EliteaAI/elitea-platform/libs/proto/gen/go/elitea/runtime/v1"
	outputapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/output"
	runtimedomain "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/runtime"
	"google.golang.org/grpc/metadata"
	"google.golang.org/protobuf/proto"
)

type outputAuthorizerStub struct {
	calls int
	err   error
}

func (s *outputAuthorizerStub) AuthorizeOutput(_ context.Context, _, _ string) (string, error) {
	s.calls++
	if s.err != nil {
		return "", s.err
	}
	return "spiffe://elitea.test/workload/worker-1", nil
}

type validationIngestorStub struct {
	frames            []outputapp.ConfigurationValidationFrame
	committedSequence *uint64
	err               error
}

func (s *validationIngestorStub) Ingest(_ context.Context, frame outputapp.ConfigurationValidationFrame) (outputapp.ProjectionOutcome, error) {
	s.frames = append(s.frames, frame)
	committed := frame.Sequence
	if s.committedSequence != nil {
		committed = *s.committedSequence
	}
	return outputapp.ProjectionOutcome{Inserted: true, Cursor: 1, CommittedSequence: committed}, s.err
}

type failureIngestorStub struct {
	frames []outputapp.RuntimeFailureFrame
	err    error
}

func (s *failureIngestorStub) IngestFailure(_ context.Context, frame outputapp.RuntimeFailureFrame) (outputapp.ProjectionOutcome, error) {
	s.frames = append(s.frames, frame)
	return outputapp.ProjectionOutcome{Inserted: true, Cursor: 1, CommittedSequence: frame.Sequence}, s.err
}

type outputStreamStub struct {
	context context.Context
	frames  []*runtimev1.ExecutionOutputFrameV1
	acks    []*runtimev1.ExecutionOutputAckV1
	actions []string
	index   int
}

func TestAgentExecutionResultMapsDelegatedAuthorizationPause(t *testing.T) {
	digest := &runtimev1.DigestV1{
		Algorithm: runtimev1.DigestAlgorithmV1_DIGEST_ALGORITHM_V1_SHA256,
		Value:     bytes.Repeat([]byte{1}, 32),
	}
	result, err := agentExecutionResultDomain(&runtimev1.AgentExecutionResultV1{
		InputBundleId:           "bundle-1",
		InputBundleDigest:       digest,
		RequestEntryId:          "request-1",
		RequestImmutableVersion: "version-1",
		RequestContentDigest:    digest,
		TerminalState:           runtimev1.AgentExecutionTerminalStateV1_AGENT_EXECUTION_TERMINAL_STATE_V1_PAUSED_MCP_AUTH,
		ResultArtifact: &runtimev1.AgentExecutionArtifactReferenceV1{
			ArtifactId:       "artifact-1",
			ImmutableVersion: "artifact-version-1",
			MediaType:        outputapp.AgentResultMediaType,
			ByteLength:       1,
			Digest:           digest,
			Classification:   outputapp.AgentResultClassification,
		},
	})
	if err != nil || result.TerminalState != outputapp.AgentExecutionTerminalPausedAuthorization {
		t.Fatalf("result=%+v error=%v", result, err)
	}
}

func (s *outputStreamStub) Recv() (*runtimev1.ExecutionOutputFrameV1, error) {
	s.actions = append(s.actions, "recv")
	if s.index >= len(s.frames) {
		return nil, io.EOF
	}
	frame := s.frames[s.index]
	s.index++
	return frame, nil
}

func (s *outputStreamStub) Send(ack *runtimev1.ExecutionOutputAckV1) error {
	s.actions = append(s.actions, "send")
	s.acks = append(s.acks, ack)
	return nil
}

func (s *outputStreamStub) SetHeader(metadata.MD) error  { return nil }
func (s *outputStreamStub) SendHeader(metadata.MD) error { return nil }
func (s *outputStreamStub) SetTrailer(metadata.MD)       {}
func (s *outputStreamStub) Context() context.Context     { return s.context }
func (s *outputStreamStub) SendMsg(any) error            { return nil }
func (s *outputStreamStub) RecvMsg(any) error            { return nil }

func TestOutputServerMapsExactCrossLanguageCorpusOnDedicatedStream(t *testing.T) {
	for _, name := range []string{"valid", "invalid", "unsupported"} {
		t.Run(name, func(t *testing.T) {
			frame := readCorpusFrame(t, name)
			authorizer := &outputAuthorizerStub{}
			validations := &validationIngestorStub{}
			failures := &failureIngestorStub{}
			server, err := NewServer(ServerConfig{
				OutputSchemaRevision: "elitea.runtime.execution-output.v1",
				MaxFrameBytes:        64 * 1024,
				CreditFrames:         8,
				CreditBytes:          64 * 1024,
			}, authorizer, validations, failures)
			if err != nil {
				t.Fatal(err)
			}
			stream := &outputStreamStub{context: context.Background(), frames: []*runtimev1.ExecutionOutputFrameV1{frame}}
			if err := server.Publish(stream); err != nil {
				t.Fatal(err)
			}
			if authorizer.calls != 1 || len(stream.acks) != 2 || stream.acks[0].GetCreditFrames() != 8 || stream.acks[0].GetCreditBytes() != 64*1024 || stream.acks[0].GetIdentity() != nil || stream.acks[1].GetRejection() != nil || stream.acks[1].GetCommittedContiguousSequence() != frame.GetSequence() {
				t.Fatalf("unexpected output acceptance: auth=%d acks=%v", authorizer.calls, stream.acks)
			}
			if len(stream.actions) < 2 || stream.actions[0] != "send" || stream.actions[1] != "recv" {
				t.Fatalf("initial output credit was not sent before receive: %v", stream.actions)
			}
			if name == "unsupported" {
				if len(validations.frames) != 0 || len(failures.frames) != 1 || failures.frames[0].Failure.Code != "UNSUPPORTED_CAPABILITY" {
					t.Fatalf("runtime failure was encoded as validation result: validations=%v failures=%v", validations.frames, failures.frames)
				}
			} else {
				if len(validations.frames) != 1 || len(failures.frames) != 0 {
					t.Fatalf("validation result routed incorrectly: validations=%v failures=%v", validations.frames, failures.frames)
				}
				wantValid := name == "valid"
				if validations.frames[0].Result.Valid != wantValid {
					t.Fatalf("legacy validation result changed: got valid=%v", validations.frames[0].Result.Valid)
				}
			}
		})
	}
}

func TestOutputServerReturnsExactBoundCancellationLinearization(t *testing.T) {
	frame := readCorpusFrame(t, "valid")
	validations := &validationIngestorStub{err: outputapp.ErrOutputCancelled}
	server := newOutputTestServer(t, validations, &failureIngestorStub{})
	stream := &outputStreamStub{context: context.Background(), frames: []*runtimev1.ExecutionOutputFrameV1{frame}}
	if err := server.Publish(stream); err != nil {
		t.Fatal(err)
	}
	if len(stream.acks) != 2 {
		t.Fatalf("unexpected ACK count: %v", stream.acks)
	}
	ack := stream.acks[1]
	if ack.GetRejection().GetCode() != runtimev1.RuntimeErrorCodeV1_RUNTIME_ERROR_CODE_V1_CANCELLED || ack.GetDesiredState() != runtimev1.DesiredExecutionStateV1_DESIRED_EXECUTION_STATE_V1_CANCELLED {
		t.Fatalf("cancellation did not carry its typed winner result: %v", ack)
	}
	if ack.GetStreamId() != frame.GetStreamId() || !proto.Equal(ack.GetIdentity(), frame.GetIdentity()) || !proto.Equal(ack.GetFence(), frame.GetFence()) || ack.GetClaimHandoffWatermark() != frame.GetClaimHandoffWatermark() || ack.GetCommittedContiguousSequence() != 0 {
		t.Fatalf("cancellation winner response is not exactly frame-bound: %v", ack)
	}
}

func TestOutputServerReturnsExactBoundDeadlineLinearization(t *testing.T) {
	frame := readCorpusFrame(t, "valid")
	validations := &validationIngestorStub{err: outputapp.ErrOutputDeadlineExceeded}
	server := newOutputTestServer(t, validations, &failureIngestorStub{})
	stream := &outputStreamStub{context: context.Background(), frames: []*runtimev1.ExecutionOutputFrameV1{frame}}
	if err := server.Publish(stream); err != nil {
		t.Fatal(err)
	}
	if len(stream.acks) != 2 {
		t.Fatalf("unexpected ACK count: %v", stream.acks)
	}
	ack := stream.acks[1]
	if ack.GetRejection().GetCode() != runtimev1.RuntimeErrorCodeV1_RUNTIME_ERROR_CODE_V1_DEADLINE_EXCEEDED || ack.GetRejection().GetSafeMessage() != outputapp.DeadlineExceededSafeMessage || !ack.GetRejection().GetRetryable() || ack.GetDesiredState() != runtimev1.DesiredExecutionStateV1_DESIRED_EXECUTION_STATE_V1_RUNNING {
		t.Fatalf("deadline did not carry its typed winner result: %v", ack)
	}
	if ack.GetStreamId() != frame.GetStreamId() || !proto.Equal(ack.GetIdentity(), frame.GetIdentity()) || !proto.Equal(ack.GetFence(), frame.GetFence()) || ack.GetClaimHandoffWatermark() != frame.GetClaimHandoffWatermark() || ack.GetCommittedContiguousSequence() != 0 || ack.GetCreditFrames() != 0 || ack.GetCreditBytes() != 0 {
		t.Fatalf("deadline winner response is not exactly frame-bound: %v", ack)
	}
}

func TestOutputServerNeverLabelsGenericStaleFenceAsCancellationWinner(t *testing.T) {
	frame := readCorpusFrame(t, "valid")
	validations := &validationIngestorStub{err: runtimedomain.ErrStaleFence}
	server := newOutputTestServer(t, validations, &failureIngestorStub{})
	stream := &outputStreamStub{context: context.Background(), frames: []*runtimev1.ExecutionOutputFrameV1{frame}}
	if err := server.Publish(stream); err != nil {
		t.Fatal(err)
	}
	ack := stream.acks[1]
	if ack.GetRejection().GetCode() != runtimev1.RuntimeErrorCodeV1_RUNTIME_ERROR_CODE_V1_STALE_FENCE || ack.GetDesiredState() != runtimev1.DesiredExecutionStateV1_DESIRED_EXECUTION_STATE_V1_UNSPECIFIED {
		t.Fatalf("generic stale fence became an ambiguous cancellation signal: %v", ack)
	}
}

func TestOutputServerRejectsPayloadDigestMismatchBeforeIngest(t *testing.T) {
	frame := proto.Clone(readCorpusFrame(t, "invalid")).(*runtimev1.ExecutionOutputFrameV1)
	frame.PayloadDigest.Value[0] ^= 0xff
	validations := &validationIngestorStub{}
	failures := &failureIngestorStub{}
	server, err := NewServer(ServerConfig{
		OutputSchemaRevision: "elitea.runtime.execution-output.v1",
		MaxFrameBytes:        64 * 1024,
		CreditFrames:         8,
		CreditBytes:          64 * 1024,
	}, &outputAuthorizerStub{}, validations, failures)
	if err != nil {
		t.Fatal(err)
	}
	stream := &outputStreamStub{context: context.Background(), frames: []*runtimev1.ExecutionOutputFrameV1{frame}}
	if err := server.Publish(stream); err != nil {
		t.Fatal(err)
	}
	if len(stream.acks) != 2 || stream.acks[1].GetRejection().GetCode() != runtimev1.RuntimeErrorCodeV1_RUNTIME_ERROR_CODE_V1_PROTOCOL_VIOLATION {
		t.Fatalf("digest mismatch did not receive protocol rejection: %v", stream.acks)
	}
	if len(validations.frames) != 0 || len(failures.frames) != 0 {
		t.Fatal("digest mismatch reached durable ingestion")
	}
}

func TestOutputServerRejectsUnregisteredWorkerControlledSafeText(t *testing.T) {
	tests := []struct {
		name   string
		frame  string
		mutate func(*runtimev1.ExecutionOutputFrameV1)
	}{
		{name: "validation issue message", frame: "invalid", mutate: func(frame *runtimev1.ExecutionOutputFrameV1) {
			frame.GetConfigurationValidation().Issues[0].SafeMessage = "Injected worker text"
			rebindFramePayload(t, frame, frame.GetConfigurationValidation())
		}},
		{name: "runtime error message", frame: "unsupported", mutate: func(frame *runtimev1.ExecutionOutputFrameV1) {
			frame.GetRuntimeError().SafeMessage = "Injected worker text"
			rebindFramePayload(t, frame, frame.GetRuntimeError())
		}},
		{name: "runtime retryability", frame: "unsupported", mutate: func(frame *runtimev1.ExecutionOutputFrameV1) {
			frame.GetRuntimeError().Retryable = true
			rebindFramePayload(t, frame, frame.GetRuntimeError())
		}},
		{name: "noncanonical internal error text", frame: "unsupported", mutate: func(frame *runtimev1.ExecutionOutputFrameV1) {
			frame.GetRuntimeError().Code = runtimev1.RuntimeErrorCodeV1_RUNTIME_ERROR_CODE_V1_INTERNAL
			frame.GetRuntimeError().SafeMessage = "The execution failed."
			rebindFramePayload(t, frame, frame.GetRuntimeError())
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			frame := proto.Clone(readCorpusFrame(t, test.frame)).(*runtimev1.ExecutionOutputFrameV1)
			test.mutate(frame)
			validations := &validationIngestorStub{}
			failures := &failureIngestorStub{}
			server := newOutputTestServer(t, validations, failures)
			stream := &outputStreamStub{context: context.Background(), frames: []*runtimev1.ExecutionOutputFrameV1{frame}}
			if err := server.Publish(stream); err != nil {
				t.Fatal(err)
			}
			if len(stream.acks) != 2 || stream.acks[1].GetRejection().GetCode() != runtimev1.RuntimeErrorCodeV1_RUNTIME_ERROR_CODE_V1_PROTOCOL_VIOLATION || len(validations.frames) != 0 || len(failures.frames) != 0 {
				t.Fatalf("unregistered safe text reached projection: acks=%v validations=%d failures=%d", stream.acks, len(validations.frames), len(failures.frames))
			}
		})
	}
}

func TestOutputServerAcceptsCanonicalInternalFailureWithoutLeakingCause(t *testing.T) {
	frame := proto.Clone(readCorpusFrame(t, "unsupported")).(*runtimev1.ExecutionOutputFrameV1)
	frame.GetRuntimeError().Code = runtimev1.RuntimeErrorCodeV1_RUNTIME_ERROR_CODE_V1_INTERNAL
	frame.GetRuntimeError().SafeMessage = "The runtime operation failed."
	frame.GetRuntimeError().Retryable = false
	rebindFramePayload(t, frame, frame.GetRuntimeError())
	validations := &validationIngestorStub{}
	failures := &failureIngestorStub{}
	server := newOutputTestServer(t, validations, failures)
	stream := &outputStreamStub{context: context.Background(), frames: []*runtimev1.ExecutionOutputFrameV1{frame}}
	if err := server.Publish(stream); err != nil {
		t.Fatal(err)
	}
	if len(stream.acks) != 2 || stream.acks[1].GetRejection() != nil || len(failures.frames) != 1 || failures.frames[0].Failure.Code != "INTERNAL" || failures.frames[0].Failure.SafeMessage != "The runtime operation failed." {
		t.Fatalf("canonical internal failure was rejected or leaked detail: acks=%v failures=%v", stream.acks, failures.frames)
	}
}

func TestOutputServerAcceptsIncompleteContinuationFailure(t *testing.T) {
	frame := proto.Clone(readCorpusFrame(t, "unsupported")).(*runtimev1.ExecutionOutputFrameV1)
	frame.GetRuntimeError().Code = runtimev1.RuntimeErrorCodeV1_RUNTIME_ERROR_CODE_V1_OUTPUT_CONTINUATION_EXHAUSTED
	frame.GetRuntimeError().SafeMessage = "Automatic continuation could not finish. The model response is incomplete."
	frame.GetRuntimeError().Retryable = false
	rebindFramePayload(t, frame, frame.GetRuntimeError())
	validations := &validationIngestorStub{}
	failures := &failureIngestorStub{}
	server := newOutputTestServer(t, validations, failures)
	stream := &outputStreamStub{context: context.Background(), frames: []*runtimev1.ExecutionOutputFrameV1{frame}}
	if err := server.Publish(stream); err != nil {
		t.Fatal(err)
	}
	if len(stream.acks) != 2 || stream.acks[1].GetRejection() != nil || len(failures.frames) != 1 || failures.frames[0].Failure.Code != "OUTPUT_CONTINUATION_EXHAUSTED" || failures.frames[0].Failure.SafeMessage != "Automatic continuation could not finish. The model response is incomplete." {
		t.Fatalf("incomplete continuation failure was rejected or leaked detail: acks=%v failures=%v", stream.acks, failures.frames)
	}
}

func TestOutputServerAcceptsCanonicalDeadlineFailure(t *testing.T) {
	frame := proto.Clone(readCorpusFrame(t, "unsupported")).(*runtimev1.ExecutionOutputFrameV1)
	frame.GetRuntimeError().Code = runtimev1.RuntimeErrorCodeV1_RUNTIME_ERROR_CODE_V1_DEADLINE_EXCEEDED
	frame.GetRuntimeError().SafeMessage = outputapp.DeadlineExceededSafeMessage
	frame.GetRuntimeError().Retryable = true
	rebindFramePayload(t, frame, frame.GetRuntimeError())
	validations := &validationIngestorStub{}
	failures := &failureIngestorStub{}
	server := newOutputTestServer(t, validations, failures)
	stream := &outputStreamStub{context: context.Background(), frames: []*runtimev1.ExecutionOutputFrameV1{frame}}
	if err := server.Publish(stream); err != nil {
		t.Fatal(err)
	}
	if len(stream.acks) != 2 || stream.acks[1].GetRejection() != nil || len(failures.frames) != 1 || failures.frames[0].Failure.Code != "DEADLINE_EXCEEDED" || !failures.frames[0].Failure.Retryable {
		t.Fatalf("canonical deadline failure was rejected or misrouted: acks=%v failures=%v", stream.acks, failures.frames)
	}
}

func TestOutputServerAcceptsCanonicalCancellationWithCancelledSettlement(t *testing.T) {
	frame := proto.Clone(readCorpusFrame(t, "unsupported")).(*runtimev1.ExecutionOutputFrameV1)
	frame.GetRuntimeError().Code = runtimev1.RuntimeErrorCodeV1_RUNTIME_ERROR_CODE_V1_CANCELLED
	frame.GetRuntimeError().SafeMessage = "Execution was cancelled."
	frame.GetRuntimeError().Retryable = false
	frame.GetSettlementProposal().RequestedOutcome = runtimev1.ExecutionOutcomeV1_EXECUTION_OUTCOME_V1_CANCELLED
	rebindFramePayload(t, frame, frame.GetRuntimeError())
	validations := &validationIngestorStub{}
	failures := &failureIngestorStub{}
	server := newOutputTestServer(t, validations, failures)
	stream := &outputStreamStub{context: context.Background(), frames: []*runtimev1.ExecutionOutputFrameV1{frame}}
	if err := server.Publish(stream); err != nil {
		t.Fatal(err)
	}
	if len(stream.acks) != 2 || stream.acks[1].GetRejection() != nil || len(failures.frames) != 1 || failures.frames[0].Failure.Code != "CANCELLED" {
		t.Fatalf("canonical cancellation was rejected or misrouted: acks=%v failures=%v", stream.acks, failures.frames)
	}
}

func TestOutputServerRejectsInvalidTimeStreamCoherenceAndFutureAck(t *testing.T) {
	tests := []struct {
		name      string
		mutate    func(*runtimev1.ExecutionOutputFrameV1)
		committed *uint64
	}{
		{name: "zero occurred-at", mutate: func(frame *runtimev1.ExecutionOutputFrameV1) { frame.OccurredAtUnixMillis = 0 }},
		{name: "stream identity mismatch", mutate: func(frame *runtimev1.ExecutionOutputFrameV1) { frame.StreamId = "another-execution:1" }},
		{name: "future committed sequence", committed: uint64Pointer(2)},
		{name: "committed sequence gap", committed: uint64Pointer(0)},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			frame := proto.Clone(readCorpusFrame(t, "valid")).(*runtimev1.ExecutionOutputFrameV1)
			if test.mutate != nil {
				test.mutate(frame)
			}
			validations := &validationIngestorStub{committedSequence: test.committed}
			server := newOutputTestServer(t, validations, &failureIngestorStub{})
			stream := &outputStreamStub{context: context.Background(), frames: []*runtimev1.ExecutionOutputFrameV1{frame}}
			if err := server.Publish(stream); err != nil {
				t.Fatal(err)
			}
			if len(stream.acks) != 2 || stream.acks[1].GetRejection().GetCode() != runtimev1.RuntimeErrorCodeV1_RUNTIME_ERROR_CODE_V1_PROTOCOL_VIOLATION {
				t.Fatalf("invalid output did not receive protocol rejection: %v", stream.acks)
			}
			if test.committed == nil && len(validations.frames) != 0 {
				t.Fatal("invalid wire frame reached ingestion")
			}
		})
	}
}

func TestOutputServerAcceptsOnlyOneTerminalFramePerStream(t *testing.T) {
	frame := readCorpusFrame(t, "valid")
	validations := &validationIngestorStub{}
	server := newOutputTestServer(t, validations, &failureIngestorStub{})
	stream := &outputStreamStub{context: context.Background(), frames: []*runtimev1.ExecutionOutputFrameV1{frame, proto.Clone(frame).(*runtimev1.ExecutionOutputFrameV1)}}
	if err := server.Publish(stream); err != nil {
		t.Fatal(err)
	}
	if len(validations.frames) != 1 || stream.index != 1 || len(stream.acks) != 2 {
		t.Fatalf("server consumed more than one terminal frame: frames=%d recv=%d acks=%d", len(validations.frames), stream.index, len(stream.acks))
	}
}

func uint64Pointer(value uint64) *uint64 { return &value }

func newOutputTestServer(t *testing.T, validations ValidationIngestor, failures RuntimeFailureIngestor) *Server {
	t.Helper()
	server, err := NewServer(ServerConfig{
		OutputSchemaRevision: "elitea.runtime.execution-output.v1",
		MaxFrameBytes:        64 * 1024,
		CreditFrames:         8,
		CreditBytes:          64 * 1024,
	}, &outputAuthorizerStub{}, validations, failures)
	if err != nil {
		t.Fatal(err)
	}
	return server
}

func rebindFramePayload(t *testing.T, frame *runtimev1.ExecutionOutputFrameV1, payload proto.Message) {
	t.Helper()
	encoded, err := proto.MarshalOptions{Deterministic: true}.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	digest := testOutputDigest(encoded)
	frame.PayloadDigest = digest
	frame.SettlementProposal.TerminalPayloadDigest = proto.Clone(digest).(*runtimev1.DigestV1)
}

func testOutputDigest(content []byte) *runtimev1.DigestV1 {
	digest := runtimedomain.SHA256(content)
	return &runtimev1.DigestV1{Algorithm: runtimev1.DigestAlgorithmV1_DIGEST_ALGORITHM_V1_SHA256, Value: append([]byte(nil), digest[:]...)}
}

func readCorpusFrame(t *testing.T, name string) *runtimev1.ExecutionOutputFrameV1 {
	t.Helper()
	_, sourceFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot locate test source")
	}
	path := filepath.Clean(filepath.Join(filepath.Dir(sourceFile), "../../../../../../testdata/proto/runtime/v1/configuration-validation", name, "expected-output.pb"))
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	frame := &runtimev1.ExecutionOutputFrameV1{}
	if err := proto.Unmarshal(raw, frame); err != nil {
		t.Fatal(err)
	}
	return frame
}

// #607: field 16 reaches the domain, and a malformed entry costs its own entry
// rather than the whole result -- the answer this frame carries must land.
func TestAgentExecutionResultMapsAttachmentContentsAndDropsMalformedEntries(t *testing.T) {
	digest := &runtimev1.DigestV1{
		Algorithm: runtimev1.DigestAlgorithmV1_DIGEST_ALGORITHM_V1_SHA256,
		Value:     bytes.Repeat([]byte{1}, 32),
	}
	good := []byte(`[{"type":"text","text":"Bucket: b","elitea_attachment":{"item_id":"x"}},{"type":"text","text":"EXTRACTED"}]`)
	result, err := agentExecutionResultDomain(&runtimev1.AgentExecutionResultV1{
		InputBundleId:           "bundle-1",
		InputBundleDigest:       digest,
		RequestEntryId:          "request-1",
		RequestImmutableVersion: "version-1",
		RequestContentDigest:    digest,
		TerminalState:           runtimev1.AgentExecutionTerminalStateV1_AGENT_EXECUTION_TERMINAL_STATE_V1_COMPLETED,
		ResultArtifact: &runtimev1.AgentExecutionArtifactReferenceV1{
			ArtifactId:       "artifact-1",
			ImmutableVersion: "artifact-version-1",
			MediaType:        outputapp.AgentResultMediaType,
			ByteLength:       1,
			Digest:           digest,
			Classification:   outputapp.AgentResultClassification,
		},
		AttachmentContents: []*runtimev1.AgentExecutionAttachmentContentV1{
			{ItemId: "50000000-0000-4000-8000-000000000001", Content: good},
			// Not a uuid, and a nil entry the repeated field can legally carry.
			{ItemId: "../../etc/passwd", Content: good},
			nil,
		},
	})
	if err != nil {
		t.Fatalf("a malformed attachment entry failed the whole result: %v", err)
	}
	if len(result.AttachmentContents) != 1 ||
		result.AttachmentContents[0].ItemID != "50000000-0000-4000-8000-000000000001" ||
		string(result.AttachmentContents[0].Content) != string(good) {
		t.Fatalf("attachment contents=%+v", result.AttachmentContents)
	}
}

func TestAgentExecutionResultCarriesNoAttachmentContentsOnAnOrdinaryTurn(t *testing.T) {
	digest := &runtimev1.DigestV1{
		Algorithm: runtimev1.DigestAlgorithmV1_DIGEST_ALGORITHM_V1_SHA256,
		Value:     bytes.Repeat([]byte{1}, 32),
	}
	result, err := agentExecutionResultDomain(&runtimev1.AgentExecutionResultV1{
		InputBundleId:           "bundle-1",
		InputBundleDigest:       digest,
		RequestEntryId:          "request-1",
		RequestImmutableVersion: "version-1",
		RequestContentDigest:    digest,
		TerminalState:           runtimev1.AgentExecutionTerminalStateV1_AGENT_EXECUTION_TERMINAL_STATE_V1_COMPLETED,
		ResultArtifact: &runtimev1.AgentExecutionArtifactReferenceV1{
			ArtifactId:       "artifact-1",
			ImmutableVersion: "artifact-version-1",
			MediaType:        outputapp.AgentResultMediaType,
			ByteLength:       1,
			Digest:           digest,
			Classification:   outputapp.AgentResultClassification,
		},
	})
	if err != nil || result.AttachmentContents != nil {
		t.Fatalf("result=%+v error=%v", result, err)
	}
}
