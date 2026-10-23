package output

import (
	"context"
	"strings"
	"testing"
	"time"

	runtimev1 "github.com/elitea-ng/elitea-platform/libs/proto/gen/go/elitea/runtime/v1"
	outputapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/output"
	runtimedomain "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/runtime"
	"google.golang.org/protobuf/proto"
)

type indexIngestorStub struct {
	frames []outputapp.IndexIngestFrame
	err    error
}

func (s *indexIngestorStub) IngestIndex(_ context.Context, frame outputapp.IndexIngestFrame) (outputapp.ProjectionOutcome, error) {
	s.frames = append(s.frames, frame)
	return outputapp.ProjectionOutcome{Inserted: true, Cursor: 1, CommittedSequence: frame.Sequence}, s.err
}

func TestOutputServerMapsTypedIndexIngestResultWithoutArtifactBytes(t *testing.T) {
	frame := validIndexWireFrame(t)
	validations := &validationIngestorStub{}
	failures := &failureIngestorStub{}
	indexes := &indexIngestorStub{}
	server := newIndexOutputTestServer(t, 64*1024, validations, failures, indexes)
	stream := &outputStreamStub{context: context.Background(), frames: []*runtimev1.ExecutionOutputFrameV1{frame}}

	if err := server.Publish(stream); err != nil {
		t.Fatal(err)
	}
	if len(stream.acks) != 2 || stream.acks[1].GetRejection() != nil || stream.acks[1].GetCommittedContiguousSequence() != 1 {
		t.Fatalf("typed index result was not accepted: %v", stream.acks)
	}
	if len(validations.frames) != 0 || len(failures.frames) != 0 || len(indexes.frames) != 1 {
		t.Fatalf("index output was routed incorrectly: validations=%d failures=%d indexes=%d", len(validations.frames), len(failures.frames), len(indexes.frames))
	}
	mapped := indexes.frames[0]
	if mapped.Result.InputBundleID != "input-bundle-index-1" || mapped.Result.Bindings.ToolkitConfiguration.EntryID != "toolkit-configuration" || mapped.Result.Bindings.ToolParameters.EntryID != "tool-parameters" || !mapped.Result.Bindings.LLMModel.Present || mapped.Result.Bindings.LLMConfiguration.Present || !mapped.Result.Bindings.MCPTokens.Present || !mapped.Result.Bindings.EmbeddingBinding.Present {
		t.Fatalf("index input bindings changed during mapping: %+v", mapped.Result.Bindings)
	}
	if mapped.Result.ResultArtifact.ArtifactID != "artifact-index-1" || mapped.Result.ResultArtifact.ByteLength != uint64(len("durable artifact bytes stay on the data plane")) || mapped.Result.ResultArtifact.MediaType != "application/json" || mapped.Result.ResultArtifact.Classification != "project-confidential" {
		t.Fatalf("artifact reference changed during mapping: %+v", mapped.Result.ResultArtifact)
	}
	if len(mapped.EncodedResult) == 0 || len(mapped.EncodedResult) >= 64*1024 {
		t.Fatalf("control/output frame does not contain bounded metadata: %d", len(mapped.EncodedResult))
	}
}

func TestOutputServerMapsOnlyBoundedCurrentIndexSummary(t *testing.T) {
	frame := validIndexWireFrame(t)
	payload := frame.GetIndexIngest()
	payload.ResultArtifact = nil
	reindex := true
	payload.ResultSummary = &runtimev1.IndexIngestSummaryV1{
		Status:        runtimev1.IndexIngestStatusV1_INDEX_INGEST_STATUS_V1_PARTLY_INDEXED,
		Message:       "Successfully indexed 3 documents (7 chunks). Failed to index 1 chunks.",
		TerminalState: runtimev1.IndexIngestTerminalStateV1_INDEX_INGEST_TERMINAL_STATE_V1_PARTLY_INDEXED,
		Indexed:       3,
		Updated:       1,
		Reindex:       &reindex,
	}
	rebindFramePayload(t, frame, payload)
	indexes := &indexIngestorStub{}
	server := newIndexOutputTestServer(t, 64*1024, &validationIngestorStub{}, &failureIngestorStub{}, indexes)
	stream := &outputStreamStub{context: context.Background(), frames: []*runtimev1.ExecutionOutputFrameV1{frame}}

	if err := server.Publish(stream); err != nil {
		t.Fatal(err)
	}
	if len(stream.acks) != 2 || stream.acks[1].GetRejection() != nil || len(indexes.frames) != 1 {
		t.Fatalf("typed inline summary was not accepted: acks=%v frames=%d", stream.acks, len(indexes.frames))
	}
	result := indexes.frames[0].Result
	if result.ResultArtifact != (outputapp.IndexArtifactReference{}) ||
		result.ResultSummary.Status != outputapp.IndexIngestStatusPartlyIndexed ||
		result.ResultSummary.Message != payload.ResultSummary.Message ||
		result.ResultSummary.TerminalState != outputapp.IndexIngestTerminalPartlyIndexed ||
		result.ResultSummary.Indexed != 3 ||
		result.ResultSummary.Updated != 1 ||
		!result.ResultSummary.ReindexPresent ||
		!result.ResultSummary.Reindex {
		t.Fatalf("inline summary mapping changed the current result: %+v", result)
	}
	if len(indexes.frames[0].EncodedResult) == 0 || len(indexes.frames[0].EncodedResult) >= 64*1024 {
		t.Fatalf("inline summary escaped the output frame bound: %d", len(indexes.frames[0].EncodedResult))
	}
}

func TestOutputServerMapsIndexErrorSummaryToFailedSettlement(t *testing.T) {
	frame := validIndexWireFrame(t)
	payload := frame.GetIndexIngest()
	payload.ResultArtifact = nil
	payload.ResultSummary = &runtimev1.IndexIngestSummaryV1{
		Status:        runtimev1.IndexIngestStatusV1_INDEX_INGEST_STATUS_V1_ERROR,
		Message:       "Indexing failed before completion.",
		TerminalState: runtimev1.IndexIngestTerminalStateV1_INDEX_INGEST_TERMINAL_STATE_V1_FAILED,
	}
	frame.GetSettlementProposal().RequestedOutcome =
		runtimev1.ExecutionOutcomeV1_EXECUTION_OUTCOME_V1_FAILED
	rebindFramePayload(t, frame, payload)
	indexes := &indexIngestorStub{}
	server := newIndexOutputTestServer(t, 64*1024, &validationIngestorStub{}, &failureIngestorStub{}, indexes)
	stream := &outputStreamStub{context: context.Background(), frames: []*runtimev1.ExecutionOutputFrameV1{frame}}

	if err := server.Publish(stream); err != nil {
		t.Fatal(err)
	}
	if len(stream.acks) != 2 || stream.acks[1].GetRejection() != nil || len(indexes.frames) != 1 {
		t.Fatalf("typed index failure was not accepted: acks=%v frames=%d", stream.acks, len(indexes.frames))
	}
	if indexes.frames[0].Settlement.Outcome != "FAILED" ||
		indexes.frames[0].Result.ResultSummary.Status != outputapp.IndexIngestStatusError {
		t.Fatalf("typed index failure lost its terminal outcome: %+v", indexes.frames[0])
	}
}

func TestOutputServerRejectsWrongIndexEventOrPayloadPairing(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*runtimev1.ExecutionOutputFrameV1)
	}{
		{
			name: "index payload with validation event",
			mutate: func(frame *runtimev1.ExecutionOutputFrameV1) {
				frame.EventType = runtimev1.ExecutionOutputEventTypeV1_EXECUTION_OUTPUT_EVENT_TYPE_V1_CONFIGURATION_VALIDATION_RESULT
			},
		},
		{
			name: "validation payload with index event",
			mutate: func(frame *runtimev1.ExecutionOutputFrameV1) {
				validation := proto.Clone(readCorpusFrame(t, "valid").GetConfigurationValidation()).(*runtimev1.ConfigurationValidationResultV1)
				frame.Payload = &runtimev1.ExecutionOutputFrameV1_ConfigurationValidation{ConfigurationValidation: validation}
				rebindFramePayload(t, frame, validation)
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			frame := validIndexWireFrame(t)
			test.mutate(frame)
			validations := &validationIngestorStub{}
			failures := &failureIngestorStub{}
			indexes := &indexIngestorStub{}
			server := newIndexOutputTestServer(t, 64*1024, validations, failures, indexes)
			stream := &outputStreamStub{context: context.Background(), frames: []*runtimev1.ExecutionOutputFrameV1{frame}}
			if err := server.Publish(stream); err != nil {
				t.Fatal(err)
			}
			if len(stream.acks) != 2 || stream.acks[1].GetRejection().GetCode() != runtimev1.RuntimeErrorCodeV1_RUNTIME_ERROR_CODE_V1_PROTOCOL_VIOLATION {
				t.Fatalf("wrong event/payload pairing was not rejected: %v", stream.acks)
			}
			if len(validations.frames) != 0 || len(failures.frames) != 0 || len(indexes.frames) != 0 {
				t.Fatal("wrong event/payload pairing reached an ingestor")
			}
		})
	}
}

func TestOutputServerRejectsOversizedUnknownAndIncompleteIndexFrames(t *testing.T) {
	tests := []struct {
		name          string
		maxFrameBytes int
		mutate        func(*runtimev1.ExecutionOutputFrameV1)
	}{
		{name: "oversized frame", maxFrameBytes: 1},
		{
			name:          "unknown nested protobuf field",
			maxFrameBytes: 64 * 1024,
			mutate: func(frame *runtimev1.ExecutionOutputFrameV1) {
				frame.GetIndexIngest().GetToolParameters().ProtoReflect().SetUnknown([]byte{0x20, 0x01})
				rebindFramePayload(t, frame, frame.GetIndexIngest())
			},
		},
		{
			name:          "missing required binding",
			maxFrameBytes: 64 * 1024,
			mutate: func(frame *runtimev1.ExecutionOutputFrameV1) {
				frame.GetIndexIngest().ToolParameters = nil
				rebindFramePayload(t, frame, frame.GetIndexIngest())
			},
		},
		{
			name:          "missing terminal result",
			maxFrameBytes: 64 * 1024,
			mutate: func(frame *runtimev1.ExecutionOutputFrameV1) {
				frame.GetIndexIngest().ResultArtifact = nil
				rebindFramePayload(t, frame, frame.GetIndexIngest())
			},
		},
		{
			name:          "artifact and inline summary together",
			maxFrameBytes: 64 * 1024,
			mutate: func(frame *runtimev1.ExecutionOutputFrameV1) {
				frame.GetIndexIngest().ResultSummary = &runtimev1.IndexIngestSummaryV1{
					Status:  runtimev1.IndexIngestStatusV1_INDEX_INGEST_STATUS_V1_OK,
					Message: "No new documents to index.",
				}
				rebindFramePayload(t, frame, frame.GetIndexIngest())
			},
		},
		{
			name:          "unknown inline status",
			maxFrameBytes: 64 * 1024,
			mutate: func(frame *runtimev1.ExecutionOutputFrameV1) {
				frame.GetIndexIngest().ResultArtifact = nil
				frame.GetIndexIngest().ResultSummary = &runtimev1.IndexIngestSummaryV1{
					Status:  runtimev1.IndexIngestStatusV1(99),
					Message: "No new documents to index.",
				}
				rebindFramePayload(t, frame, frame.GetIndexIngest())
			},
		},
		{
			name:          "oversized inline message",
			maxFrameBytes: 64 * 1024,
			mutate: func(frame *runtimev1.ExecutionOutputFrameV1) {
				frame.GetIndexIngest().ResultArtifact = nil
				frame.GetIndexIngest().ResultSummary = &runtimev1.IndexIngestSummaryV1{
					Status:  runtimev1.IndexIngestStatusV1_INDEX_INGEST_STATUS_V1_OK,
					Message: strings.Repeat("x", outputapp.MaxIndexIngestSummaryMessageBytes+1),
				}
				rebindFramePayload(t, frame, frame.GetIndexIngest())
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			frame := validIndexWireFrame(t)
			if test.mutate != nil {
				test.mutate(frame)
			}
			indexes := &indexIngestorStub{}
			server := newIndexOutputTestServer(t, test.maxFrameBytes, &validationIngestorStub{}, &failureIngestorStub{}, indexes)
			stream := &outputStreamStub{context: context.Background(), frames: []*runtimev1.ExecutionOutputFrameV1{frame}}
			if err := server.Publish(stream); err != nil {
				t.Fatal(err)
			}
			if len(stream.acks) != 2 || stream.acks[1].GetRejection().GetCode() != runtimev1.RuntimeErrorCodeV1_RUNTIME_ERROR_CODE_V1_PROTOCOL_VIOLATION || len(indexes.frames) != 0 {
				t.Fatalf("invalid index frame reached ingestion: acks=%v frames=%d", stream.acks, len(indexes.frames))
			}
		})
	}
}

func TestOutputServerMapsIndexArtifactAvailabilityToRetryableDependencyFailure(t *testing.T) {
	indexes := &indexIngestorStub{err: outputapp.ErrIndexIngestArtifactUnavailable}
	server := newIndexOutputTestServer(t, 64*1024, &validationIngestorStub{}, &failureIngestorStub{}, indexes)
	stream := &outputStreamStub{context: context.Background(), frames: []*runtimev1.ExecutionOutputFrameV1{validIndexWireFrame(t)}}
	if err := server.Publish(stream); err != nil {
		t.Fatal(err)
	}
	rejection := stream.acks[1].GetRejection()
	if rejection.GetCode() != runtimev1.RuntimeErrorCodeV1_RUNTIME_ERROR_CODE_V1_DEPENDENCY_UNAVAILABLE || !rejection.GetRetryable() {
		t.Fatalf("absent durable artifact was not mapped to a retryable dependency failure: %v", rejection)
	}
}

func TestOutputServerValidationRouteRegressesNeitherWithIndexIngestor(t *testing.T) {
	validations := &validationIngestorStub{}
	indexes := &indexIngestorStub{}
	server := newIndexOutputTestServer(t, 64*1024, validations, &failureIngestorStub{}, indexes)
	stream := &outputStreamStub{context: context.Background(), frames: []*runtimev1.ExecutionOutputFrameV1{readCorpusFrame(t, "valid")}}
	if err := server.Publish(stream); err != nil {
		t.Fatal(err)
	}
	if len(stream.acks) != 2 || stream.acks[1].GetRejection() != nil || len(validations.frames) != 1 || len(indexes.frames) != 0 {
		t.Fatalf("existing validation route regressed: acks=%v validations=%d indexes=%d", stream.acks, len(validations.frames), len(indexes.frames))
	}
}

func newIndexOutputTestServer(t *testing.T, maxFrameBytes int, validations ValidationIngestor, failures RuntimeFailureIngestor, indexes IndexIngestIngestor) *Server {
	t.Helper()
	server, err := NewServerWithIndexIngest(ServerConfig{
		OutputSchemaRevision: "elitea.runtime.execution-output.v1",
		MaxFrameBytes:        maxFrameBytes,
		CreditFrames:         8,
		CreditBytes:          64 * 1024,
	}, &outputAuthorizerStub{}, validations, failures, indexes)
	if err != nil {
		t.Fatal(err)
	}
	return server
}

func validIndexWireFrame(t *testing.T) *runtimev1.ExecutionOutputFrameV1 {
	t.Helper()
	toolkit := indexWireBinding("toolkit-configuration", "toolkit-settings")
	parameters := indexWireBinding("tool-parameters", "tool-parameters")
	llmModel := indexWireBinding("llm-model", "llm-model")
	mcpTokens := indexWireBinding("mcp-credential-references", "mcp-references")
	embeddingBinding := indexWireBinding("embedding-binding", "embedding")
	artifactContent := []byte("durable artifact bytes stay on the data plane")
	artifactDigest := testOutputDigest(artifactContent)
	fenceDigest := runtimedomain.SHA256([]byte("unpredictable-index-fence-token"))
	payload := &runtimev1.IndexIngestResultV1{
		InputBundleId:        "input-bundle-index-1",
		InputBundleDigest:    testOutputDigest([]byte("index-input-manifest")),
		ToolkitConfiguration: toolkit,
		ToolParameters:       parameters,
		LlmModel:             llmModel,
		McpTokens:            mcpTokens,
		EmbeddingBinding:     embeddingBinding,
		ResultArtifact: &runtimev1.IndexIngestArtifactReferenceV1{
			ArtifactId:       "artifact-index-1",
			ImmutableVersion: runtimedomain.SHA256(artifactContent).String(),
			MediaType:        "application/json",
			ByteLength:       uint64(len(artifactContent)),
			Digest:           artifactDigest,
			Classification:   "project-confidential",
		},
	}
	encodedPayload, err := proto.MarshalOptions{Deterministic: true}.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	payloadDigest := testOutputDigest(encodedPayload)
	identity := &runtimev1.ExecutionIdentityV1{
		TenantId:            "tenant-1",
		ResourceProjectId:   "project-1",
		ProjectionProjectId: "project-1",
		CommandId:           "command-index-1",
		ExecutionId:         "execution-index-1",
		Generation:          1,
	}
	logicalOutputID := "index-ingest:execution-index-1"
	return &runtimev1.ExecutionOutputFrameV1{
		OutputSchemaRevision: "elitea.runtime.execution-output.v1",
		StreamId:             "execution-index-1:1",
		Identity:             identity,
		Fence: &runtimev1.ExecutionFenceV1{
			WorkloadSessionId: "workload-index-1",
			ClaimAttempt:      1,
			LeaseEpoch:        1,
			ProducerId:        "indexer-1",
			FenceToken:        append([]byte(nil), fenceDigest[:]...),
		},
		LogicalOutputId:       logicalOutputID,
		EventId:               "command-index-1:1",
		Sequence:              1,
		ClaimHandoffWatermark: 7,
		EventType:             runtimev1.ExecutionOutputEventTypeV1_EXECUTION_OUTPUT_EVENT_TYPE_V1_INDEX_INGEST_RESULT,
		OccurredAtUnixMillis:  time.Date(2026, time.July, 22, 12, 0, 0, 0, time.UTC).UnixMilli(),
		PayloadDigest:         payloadDigest,
		Terminal:              true,
		SettlementProposal: &runtimev1.SettlementProposalV1{
			ProposalId:              "command-index-1:settlement",
			RequestedOutcome:        runtimev1.ExecutionOutcomeV1_EXECUTION_OUTCOME_V1_SUCCEEDED,
			TerminalLogicalOutputId: logicalOutputID,
			TerminalEventId:         "command-index-1:1",
			TerminalSequence:        1,
			TerminalPayloadDigest:   proto.Clone(payloadDigest).(*runtimev1.DigestV1),
			PrepareIdempotencyKey:   "command-index-1:prepare-settlement",
		},
		Payload: &runtimev1.ExecutionOutputFrameV1_IndexIngest{IndexIngest: payload},
	}
}

func indexWireBinding(entryID, content string) *runtimev1.IndexIngestInputBindingV1 {
	digest := runtimedomain.SHA256([]byte(content))
	return &runtimev1.IndexIngestInputBindingV1{
		EntryId:          entryID,
		ImmutableVersion: digest.String(),
		ContentDigest:    testOutputDigest([]byte(content)),
	}
}
