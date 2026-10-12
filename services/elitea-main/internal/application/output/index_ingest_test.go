package output

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	executionapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/execution"
	executiondomain "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/execution"
	runtimedomain "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/runtime"
)

type indexBindingRepositoryStub struct {
	expected ExpectedIndexIngest
	err      error
	calls    int
}

func (s *indexBindingRepositoryStub) ExpectedIndexIngest(_ context.Context, executionID string, generation uint64) (ExpectedIndexIngest, error) {
	s.calls++
	if s.err != nil {
		return ExpectedIndexIngest{}, s.err
	}
	if executionID != s.expected.ExecutionID || generation != s.expected.Generation {
		return ExpectedIndexIngest{}, runtimedomain.ErrStaleFence
	}
	return s.expected, nil
}

type indexFenceVerifierStub struct {
	expected runtimedomain.Fence
	err      error
	calls    int
}

func (s *indexFenceVerifierStub) VerifyActive(_ context.Context, fence runtimedomain.Fence) error {
	s.calls++
	if s.err != nil {
		return s.err
	}
	if s.expected != (runtimedomain.Fence{}) && s.expected != fence {
		return runtimedomain.ErrStaleFence
	}
	return nil
}

type indexArtifactVerifierStub struct {
	verified DurableIndexArtifact
	err      error
	requests []ArtifactVerificationRequest
	actions  *[]string
}

func (s *indexArtifactVerifierStub) VerifyDurable(_ context.Context, request ArtifactVerificationRequest) (DurableIndexArtifact, error) {
	s.requests = append(s.requests, request)
	if s.actions != nil {
		*s.actions = append(*s.actions, "verify-artifact")
	}
	return s.verified, s.err
}

type indexProjectorStub struct {
	projections []IndexIngestProjection
	outcome     ProjectionOutcome
	err         error
	actions     *[]string
}

func (s *indexProjectorStub) ProjectIndexIngest(_ context.Context, projection IndexIngestProjection) (ProjectionOutcome, error) {
	s.projections = append(s.projections, projection)
	if s.actions != nil {
		*s.actions = append(*s.actions, "project-and-settle")
	}
	if s.outcome == (ProjectionOutcome{}) {
		s.outcome = ProjectionOutcome{Inserted: true, Cursor: 1, CommittedSequence: projection.Frame.Sequence}
	}
	return s.outcome, s.err
}

func TestIndexIngestRequiresDurableArtifactBeforeProjection(t *testing.T) {
	frame, expected, verified := validIndexIngestOutput()
	actions := []string{}
	bindings := &indexBindingRepositoryStub{expected: expected}
	fences := &indexFenceVerifierStub{expected: frame.Fence}
	artifacts := &indexArtifactVerifierStub{verified: verified, actions: &actions}
	projector := &indexProjectorStub{actions: &actions}
	service, err := NewIndexIngestService(bindings, fences, artifacts, projector)
	if err != nil {
		t.Fatal(err)
	}

	outcome, err := service.IngestIndex(context.Background(), frame)
	if err != nil {
		t.Fatal(err)
	}
	if !outcome.Inserted || outcome.Cursor != 1 || outcome.CommittedSequence != 1 {
		t.Fatalf("unexpected projection outcome: %+v", outcome)
	}
	if strings.Join(actions, ",") != "verify-artifact,project-and-settle" {
		t.Fatalf("artifact durability was not established before projection: %v", actions)
	}
	if bindings.calls != 1 || fences.calls != 1 || len(artifacts.requests) != 1 || len(projector.projections) != 1 {
		t.Fatalf("unexpected service calls: bindings=%d fences=%d artifacts=%d projections=%d", bindings.calls, fences.calls, len(artifacts.requests), len(projector.projections))
	}
	request := artifacts.requests[0]
	if err := request.Validate(); err != nil || request.Artifact != frame.Result.ResultArtifact || request.ExecutionID != expected.ExecutionID || request.TenantID != expected.TenantID {
		t.Fatalf("artifact verification was not bound to admitted identity: %+v err=%v", request, err)
	}
	if projector.projections[0].VerifiedArtifact != verified {
		t.Fatalf("projector did not receive exact durable evidence: %+v", projector.projections[0].VerifiedArtifact)
	}

	frame.EncodedResult[0] ^= 0xff
	if projector.projections[0].Frame.EncodedResult[0] == frame.EncodedResult[0] {
		t.Fatal("projected frame retained caller-owned result bytes")
	}
}

func TestIndexIngestProjectsCurrentInlineSummaryWithoutArtifactLookup(t *testing.T) {
	frame, expected, _ := validIndexIngestOutput()
	frame.Result.ResultArtifact = IndexArtifactReference{}
	frame.Result.ResultSummary = IndexIngestSummary{
		Status:  IndexIngestStatusOK,
		Message: "Successfully indexed 3 documents (7 chunks).",
	}
	rebindIndexApplicationFrame(&frame, "inline-summary")
	bindings := &indexBindingRepositoryStub{expected: expected}
	fences := &indexFenceVerifierStub{expected: frame.Fence}
	artifacts := &indexArtifactVerifierStub{}
	projector := &indexProjectorStub{}
	service, err := NewIndexIngestService(bindings, fences, artifacts, projector)
	if err != nil {
		t.Fatal(err)
	}

	outcome, err := service.IngestIndex(context.Background(), frame)
	if err != nil {
		t.Fatal(err)
	}
	if !outcome.Inserted || len(artifacts.requests) != 0 || len(projector.projections) != 1 {
		t.Fatalf("inline summary crossed the artifact boundary: outcome=%+v artifact_calls=%d projections=%d", outcome, len(artifacts.requests), len(projector.projections))
	}
	projected := projector.projections[0]
	if projected.VerifiedArtifact != (DurableIndexArtifact{}) || projected.Frame.Result.ResultSummary != frame.Result.ResultSummary {
		t.Fatalf("inline summary projection changed: %+v", projected)
	}
}

func TestIndexIngestSummaryAcceptsOnlyCurrentBoundedTerminalShape(t *testing.T) {
	valid := IndexIngestSummary{Status: IndexIngestStatusOK, Message: "No new documents to index."}
	for _, status := range []IndexIngestStatus{IndexIngestStatusOK, IndexIngestStatusPartlyIndexed, IndexIngestStatusError} {
		summary := valid
		summary.Status = status
		if err := summary.Validate(); err != nil {
			t.Fatalf("current status %q was rejected: %v", status, err)
		}
	}

	invalid := []IndexIngestSummary{
		{Status: "success", Message: valid.Message},
		{Status: IndexIngestStatusOK},
		{Status: IndexIngestStatusOK, Message: strings.Repeat("x", MaxIndexIngestSummaryMessageBytes+1)},
		{Status: IndexIngestStatusOK, Message: string([]byte{0xff})},
		{Status: IndexIngestStatusOK, Message: "safe prefix\x00hidden suffix"},
		{
			Status:        IndexIngestStatusOK,
			Message:       valid.Message,
			TerminalState: IndexIngestTerminalFailed,
		},
		{
			Status:        IndexIngestStatusError,
			Message:       "failed",
			TerminalState: IndexIngestTerminalCreated,
		},
		// A stopped run is an error summary that describes the store: without
		// the totals, or with another status, it is malformed.
		{
			Status:        IndexIngestStatusError,
			Message:       "stopped",
			TerminalState: IndexIngestTerminalCancelled,
		},
		{
			Status:        IndexIngestStatusOK,
			Message:       "stopped",
			TerminalState: IndexIngestTerminalCancelled,
			TotalsPresent: true,
		},
		{
			Status:  IndexIngestStatusOK,
			Message: valid.Message,
			Indexed: 1,
		},
		{
			Status:  IndexIngestStatusOK,
			Message: valid.Message,
			Reindex: true,
		},
	}
	for _, summary := range invalid {
		if err := summary.Validate(); !errors.Is(err, ErrInvalidIndexIngestOutput) {
			t.Fatalf("invalid summary was accepted: status=%q message_bytes=%d err=%v", summary.Status, len(summary.Message), err)
		}
	}

	frame, _, _ := validIndexIngestOutput()
	frame.Result.ResultSummary = valid
	if err := frame.Result.Validate(); !errors.Is(err, ErrInvalidIndexIngestOutput) {
		t.Fatalf("artifact and summary were accepted together: %v", err)
	}
	frame.Result.ResultArtifact = IndexArtifactReference{}
	frame.Result.ResultSummary = IndexIngestSummary{}
	if err := frame.Result.Validate(); !errors.Is(err, ErrInvalidIndexIngestOutput) {
		t.Fatalf("missing terminal result was accepted: %v", err)
	}
}

func TestIndexIngestFrameCouplesSummaryStatusToSettlementOutcome(t *testing.T) {
	for _, test := range []struct {
		name    string
		summary IndexIngestSummary
		outcome executionapp.SettlementOutcome
	}{
		{name: "ok", summary: IndexIngestSummary{Status: IndexIngestStatusOK}, outcome: executionapp.SettlementSucceeded},
		{name: "partly indexed", summary: IndexIngestSummary{Status: IndexIngestStatusPartlyIndexed}, outcome: executionapp.SettlementSucceeded},
		{name: "error", summary: IndexIngestSummary{Status: IndexIngestStatusError}, outcome: executionapp.SettlementFailed},
		// A run stopped after it touched the store (ADR-0030 I1): an error
		// summary with the store's totals that settles CANCELLED, not FAILED.
		{name: "cancelled", summary: IndexIngestSummary{
			Status: IndexIngestStatusError, TerminalState: IndexIngestTerminalCancelled,
			TotalsPresent: true, TotalDocuments: 2, TotalChunks: 5, IndexedDocuments: 2, IndexedChunks: 5,
		}, outcome: executionapp.SettlementCancelled},
	} {
		t.Run(test.name, func(t *testing.T) {
			frame, _, _ := validIndexIngestOutput()
			frame.Result.ResultArtifact = IndexArtifactReference{}
			frame.Result.ResultSummary = test.summary
			frame.Result.ResultSummary.Message = "bounded terminal message"
			if got := frame.Result.ResultSummary.SettlementOutcome(); got != test.outcome {
				t.Fatalf("SettlementOutcome() = %q, want %q", got, test.outcome)
			}
			frame.Settlement.Outcome = test.outcome
			rebindIndexApplicationFrame(&frame, test.name)
			if err := frame.Validate(); err != nil {
				t.Fatalf("matching terminal outcome was rejected: %v", err)
			}
			for _, other := range []executionapp.SettlementOutcome{executionapp.SettlementSucceeded, executionapp.SettlementFailed, executionapp.SettlementCancelled} {
				if other == test.outcome {
					continue
				}
				frame.Settlement.Outcome = other
				if err := frame.Validate(); !errors.Is(err, ErrInvalidIndexIngestOutput) {
					t.Fatalf("mismatched terminal outcome %q was accepted: %v", other, err)
				}
			}
		})
	}
}

func TestIndexIngestRejectsWrongExtraAndMissingInputBindings(t *testing.T) {
	tests := []struct {
		name       string
		mutate     func(*IndexIngestFrame, *ExpectedIndexIngest)
		want       error
		wantLookup bool
	}{
		{
			name: "wrong required binding digest",
			mutate: func(frame *IndexIngestFrame, _ *ExpectedIndexIngest) {
				frame.Result.Bindings.ToolParameters.ContentDigest = runtimedomain.SHA256([]byte("forged-tool-parameters"))
			},
			want:       ErrIndexIngestBindingMismatch,
			wantLookup: true,
		},
		{
			name: "extra optional binding",
			mutate: func(frame *IndexIngestFrame, _ *ExpectedIndexIngest) {
				frame.Result.Bindings.LLMConfiguration = OptionalIndexInputBinding{Present: true, Binding: testIndexBinding("llm-configuration", "llm-configuration")}
			},
			want:       ErrIndexIngestBindingMismatch,
			wantLookup: true,
		},
		{
			name: "missing optional binding",
			mutate: func(frame *IndexIngestFrame, _ *ExpectedIndexIngest) {
				frame.Result.Bindings.LLMModel = OptionalIndexInputBinding{}
			},
			want:       ErrIndexIngestBindingMismatch,
			wantLookup: true,
		},
		{
			name: "missing required binding",
			mutate: func(frame *IndexIngestFrame, _ *ExpectedIndexIngest) {
				frame.Result.Bindings.ToolParameters = IndexInputBinding{}
			},
			want: ErrInvalidIndexIngestOutput,
		},
		{
			name: "wrong input bundle digest",
			mutate: func(frame *IndexIngestFrame, _ *ExpectedIndexIngest) {
				frame.Result.InputBundleDigest = runtimedomain.SHA256([]byte("forged-manifest"))
			},
			want:       ErrIndexIngestBindingMismatch,
			wantLookup: true,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			frame, expected, verified := validIndexIngestOutput()
			test.mutate(&frame, &expected)
			rebindIndexApplicationFrame(&frame, test.name)
			bindings := &indexBindingRepositoryStub{expected: expected}
			artifacts := &indexArtifactVerifierStub{verified: verified}
			projector := &indexProjectorStub{}
			service, err := NewIndexIngestService(bindings, &indexFenceVerifierStub{expected: frame.Fence}, artifacts, projector)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := service.IngestIndex(context.Background(), frame); !errors.Is(err, test.want) {
				t.Fatalf("expected %v, got %v", test.want, err)
			}
			if (test.wantLookup && bindings.calls != 1) || (!test.wantLookup && bindings.calls != 0) {
				t.Fatalf("unexpected admitted-binding lookup count: %d", bindings.calls)
			}
			if len(artifacts.requests) != 0 || len(projector.projections) != 0 {
				t.Fatal("invalid input binding reached artifact verification or projection")
			}
		})
	}
}

func TestIndexIngestRejectsForgedOrAbsentArtifactBeforeProjection(t *testing.T) {
	t.Run("forged authoritative metadata", func(t *testing.T) {
		frame, expected, verified := validIndexIngestOutput()
		verified.Reference.Digest = runtimedomain.SHA256([]byte("authoritative-other-content"))
		artifacts := &indexArtifactVerifierStub{verified: verified}
		projector := &indexProjectorStub{}
		service, err := NewIndexIngestService(&indexBindingRepositoryStub{expected: expected}, &indexFenceVerifierStub{expected: frame.Fence}, artifacts, projector)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := service.IngestIndex(context.Background(), frame); !errors.Is(err, ErrIndexIngestArtifactMismatch) {
			t.Fatalf("expected forged artifact rejection, got %v", err)
		}
		if len(projector.projections) != 0 {
			t.Fatal("forged artifact reached projection")
		}
	})

	t.Run("absent artifact", func(t *testing.T) {
		frame, expected, _ := validIndexIngestOutput()
		artifacts := &indexArtifactVerifierStub{err: ErrIndexIngestArtifactUnavailable}
		projector := &indexProjectorStub{}
		service, err := NewIndexIngestService(&indexBindingRepositoryStub{expected: expected}, &indexFenceVerifierStub{expected: frame.Fence}, artifacts, projector)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := service.IngestIndex(context.Background(), frame); !errors.Is(err, ErrIndexIngestArtifactUnavailable) {
			t.Fatalf("expected absent artifact rejection, got %v", err)
		}
		if len(projector.projections) != 0 {
			t.Fatal("absent artifact reached projection")
		}
	})

	t.Run("artifact violates admitted contract", func(t *testing.T) {
		frame, expected, verified := validIndexIngestOutput()
		frame.Result.ResultArtifact.Classification = "public"
		rebindIndexApplicationFrame(&frame, "classification")
		artifacts := &indexArtifactVerifierStub{verified: verified}
		projector := &indexProjectorStub{}
		service, err := NewIndexIngestService(&indexBindingRepositoryStub{expected: expected}, &indexFenceVerifierStub{expected: frame.Fence}, artifacts, projector)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := service.IngestIndex(context.Background(), frame); !errors.Is(err, ErrIndexIngestArtifactMismatch) {
			t.Fatalf("expected classification rejection, got %v", err)
		}
		if len(artifacts.requests) != 0 || len(projector.projections) != 0 {
			t.Fatal("policy-invalid artifact reached storage or projection")
		}
	})
}

func TestIndexIngestStrictlyValidatesFenceIdentitySequenceDigestCapabilityAndMetadata(t *testing.T) {
	tests := []struct {
		name         string
		mutateFrame  func(*IndexIngestFrame)
		mutateExpect func(*ExpectedIndexIngest)
		want         error
	}{
		{name: "stale fence", mutateFrame: func(frame *IndexIngestFrame) { frame.Fence.Token[0] ^= 0xff; frame.Settlement.Fence = frame.Fence }, want: runtimedomain.ErrStaleFence},
		{name: "wrong tenant", mutateFrame: func(frame *IndexIngestFrame) { frame.TenantID = "tenant-2" }, want: ErrIndexIngestBindingMismatch},
		{name: "wrong logical output", mutateFrame: func(frame *IndexIngestFrame) {
			frame.LogicalOutputID = "index-ingest:other"
			frame.Settlement.TerminalLogicalOutputID = frame.LogicalOutputID
		}, want: ErrIndexIngestBindingMismatch},
		{name: "wrong sequence", mutateFrame: func(frame *IndexIngestFrame) { frame.Sequence = 2; frame.Settlement.TerminalSequence = 2 }, want: ErrInvalidIndexIngestOutput},
		{name: "wrong payload digest", mutateFrame: func(frame *IndexIngestFrame) {
			frame.PayloadDigest[0] ^= 0xff
			frame.Settlement.TerminalPayloadDigest = frame.PayloadDigest
		}, want: ErrInvalidIndexIngestOutput},
		{name: "wrong capability", mutateExpect: func(expected *ExpectedIndexIngest) {
			expected.CapabilityID = executiondomain.ConfigurationValidationCapability
		}, want: ErrInvalidIndexIngestOutput},
		{name: "oversized metadata", mutateFrame: func(frame *IndexIngestFrame) {
			frame.Result.ResultArtifact.ArtifactID = strings.Repeat("a", maxIndexOutputMetadataBytes+1)
		}, want: ErrInvalidIndexIngestOutput},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			frame, expected, verified := validIndexIngestOutput()
			originalFence := frame.Fence
			if test.mutateFrame != nil {
				test.mutateFrame(&frame)
			}
			if test.mutateExpect != nil {
				test.mutateExpect(&expected)
			}
			if test.name != "wrong payload digest" {
				rebindIndexApplicationFrame(&frame, test.name)
			}
			artifacts := &indexArtifactVerifierStub{verified: verified}
			projector := &indexProjectorStub{}
			service, err := NewIndexIngestService(&indexBindingRepositoryStub{expected: expected}, &indexFenceVerifierStub{expected: originalFence}, artifacts, projector)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := service.IngestIndex(context.Background(), frame); !errors.Is(err, test.want) {
				t.Fatalf("expected %v, got %v", test.want, err)
			}
			if len(artifacts.requests) != 0 || len(projector.projections) != 0 {
				t.Fatal("invalid output reached artifact verification or projection")
			}
		})
	}
}

func validIndexIngestOutput() (IndexIngestFrame, ExpectedIndexIngest, DurableIndexArtifact) {
	bindings := IndexIngestBindings{
		ToolkitConfiguration: testIndexBinding("toolkit-configuration", "toolkit-settings"),
		ToolParameters:       testIndexBinding("tool-parameters", "tool-parameters"),
		LLMModel: OptionalIndexInputBinding{
			Present: true,
			Binding: testIndexBinding("llm-model", "llm-model"),
		},
		MCPTokens: OptionalIndexInputBinding{
			Present: true,
			Binding: testIndexBinding("mcp-credential-references", "mcp-references"),
		},
		EmbeddingBinding: OptionalIndexInputBinding{
			Present: true,
			Binding: testIndexBinding("embedding-binding", "embedding"),
		},
	}
	fence := runtimedomain.Fence{
		CommandID:         "command-index-1",
		ExecutionID:       "execution-index-1",
		Generation:        1,
		WorkloadIdentity:  "spiffe://elitea.test/workload/indexer-1",
		WorkloadSessionID: "workload-index-1",
		ProducerID:        "indexer-1",
		ClaimAttempt:      1,
		LeaseEpoch:        1,
		Token:             runtimedomain.FenceToken(runtimedomain.SHA256([]byte("unpredictable-index-fence-token"))),
	}
	artifactDigest := runtimedomain.SHA256([]byte("durable artifact bytes stay on the data plane"))
	artifact := IndexArtifactReference{
		ArtifactID:       "artifact-index-1",
		ImmutableVersion: artifactDigest.String(),
		MediaType:        "application/json",
		ByteLength:       uint64(len("durable artifact bytes stay on the data plane")),
		Digest:           artifactDigest,
		Classification:   "project-confidential",
	}
	result := IndexIngestResult{
		InputBundleID:     "input-bundle-index-1",
		InputBundleDigest: runtimedomain.SHA256([]byte("index-input-manifest")),
		Bindings:          bindings,
		ResultArtifact:    artifact,
	}
	encodedResult := []byte("deterministic-index-result-protobuf-metadata-only")
	encodedSettlement := []byte("deterministic-index-settlement-protobuf")
	payloadDigest := runtimedomain.SHA256(encodedResult)
	logicalOutputID := "index-ingest:execution-index-1"
	frame := IndexIngestFrame{
		StreamID:            "execution-index-1:1",
		TenantID:            "tenant-1",
		ResourceProjectID:   "project-1",
		ProjectionProjectID: "project-1",
		WorkloadSessionID:   fence.WorkloadSessionID,
		ProducerID:          fence.ProducerID,
		EventID:             "command-index-1:1",
		LogicalOutputID:     logicalOutputID,
		Sequence:            1,
		OccurredAt:          time.Date(2026, time.July, 22, 12, 0, 0, 0, time.UTC),
		Fence:               fence,
		PayloadDigest:       payloadDigest,
		EncodedResult:       encodedResult,
		Settlement: executionapp.SettlementProposal{
			Fence:                   fence,
			ProposalID:              "command-index-1:settlement",
			Outcome:                 executionapp.SettlementSucceeded,
			TerminalLogicalOutputID: logicalOutputID,
			TerminalEventID:         "command-index-1:1",
			TerminalSequence:        1,
			TerminalPayloadDigest:   payloadDigest,
			ProposalDigest:          runtimedomain.SHA256(encodedSettlement),
			IdempotencyKey:          "command-index-1:prepare-settlement",
		},
		EncodedSettlement: encodedSettlement,
		Result:            result,
	}
	expected := ExpectedIndexIngest{
		TenantID:            frame.TenantID,
		ResourceProjectID:   frame.ResourceProjectID,
		ProjectionProjectID: frame.ProjectionProjectID,
		CapabilityID:        executiondomain.IndexIngestCapability,
		CommandID:           fence.CommandID,
		ExecutionID:         fence.ExecutionID,
		Generation:          fence.Generation,
		LogicalOutputID:     logicalOutputID,
		InputBundleID:       result.InputBundleID,
		InputBundleDigest:   result.InputBundleDigest,
		Bindings:            bindings,
		ArtifactContract: IndexArtifactContract{
			MediaType:      artifact.MediaType,
			Classification: artifact.Classification,
			MaxByteLength:  1024 * 1024,
		},
	}
	verified := DurableIndexArtifact{
		Reference:       artifact,
		StorageRecordID: "artifact-storage-record-1",
		VerifiedAt:      time.Date(2026, time.July, 22, 12, 0, 1, 0, time.UTC),
	}
	return frame, expected, verified
}

func testIndexBinding(entryID, content string) IndexInputBinding {
	digest := runtimedomain.SHA256([]byte(content))
	return IndexInputBinding{EntryID: entryID, ImmutableVersion: digest.String(), ContentDigest: digest}
}

func rebindIndexApplicationFrame(frame *IndexIngestFrame, marker string) {
	frame.EncodedResult = []byte("deterministic-index-result-" + marker)
	frame.PayloadDigest = runtimedomain.SHA256(frame.EncodedResult)
	frame.Settlement.TerminalPayloadDigest = frame.PayloadDigest
	frame.EncodedSettlement = []byte("deterministic-index-settlement-" + marker)
	frame.Settlement.ProposalDigest = runtimedomain.SHA256(frame.EncodedSettlement)
}
