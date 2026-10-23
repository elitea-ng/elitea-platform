package output

import (
	"context"
	"fmt"
	"strings"
	"testing"

	runtimev1 "github.com/elitea-ng/elitea-platform/libs/proto/gen/go/elitea/runtime/v1"
	outputapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/output"
	"google.golang.org/protobuf/proto"
)

func publishIndexSummary(t *testing.T, summary *runtimev1.IndexIngestSummaryV1) (*outputStreamStub, *indexIngestorStub) {
	t.Helper()
	frame := validIndexWireFrame(t)
	payload := frame.GetIndexIngest()
	payload.ResultArtifact = nil
	payload.ResultSummary = summary
	rebindFramePayload(t, frame, payload)
	indexes := &indexIngestorStub{}
	server := newIndexOutputTestServer(t, 64*1024, &validationIngestorStub{}, &failureIngestorStub{}, indexes)
	stream := &outputStreamStub{context: context.Background(), frames: []*runtimev1.ExecutionOutputFrameV1{frame}}
	if err := server.Publish(stream); err != nil {
		t.Fatal(err)
	}
	return stream, indexes
}

// ADR-0030 decision 2: the Rust runtime reports counts, skips and the embedding
// stamp as typed fields, and Main reads them from the wire message rather than
// from a result sentence.
func TestOutputServerMapsTypedIndexCountsSkipsAndEmbeddingStamp(t *testing.T) {
	stream, indexes := publishIndexSummary(t, &runtimev1.IndexIngestSummaryV1{
		Status:             runtimev1.IndexIngestStatusV1_INDEX_INGEST_STATUS_V1_PARTLY_INDEXED,
		Message:            "Indexed 3 documents (7 chunks); 1 chunk failed.",
		TerminalState:      runtimev1.IndexIngestTerminalStateV1_INDEX_INGEST_TERMINAL_STATE_V1_PARTLY_INDEXED,
		IndexedDocuments:   3,
		IndexedChunks:      7,
		FailedChunks:       1,
		Skipped:            map[string]uint64{"unsupported_type": 2, "too_large": 1},
		EmbeddingModel:     "text-embedding-3-small",
		EmbeddingDimension: 1536,
	})
	if len(stream.acks) != 2 || stream.acks[1].GetRejection() != nil || len(indexes.frames) != 1 {
		t.Fatalf("typed summary was not accepted: acks=%v frames=%d", stream.acks, len(indexes.frames))
	}
	summary := indexes.frames[0].Result.ResultSummary
	if summary.IndexedDocuments != 3 || summary.IndexedChunks != 7 || summary.FailedChunks != 1 ||
		summary.EmbeddingModel != "text-embedding-3-small" || summary.EmbeddingDimension != 1536 ||
		summary.SkippedJSON != `{"too_large":1,"unsupported_type":2}` {
		t.Fatalf("typed result changed during mapping: %+v", summary)
	}
	if !summary.HasTypedResult() {
		t.Fatal("a summary with typed fields did not report a typed result")
	}
	if skipped := summary.Skipped(); skipped["unsupported_type"] != 2 || skipped["too_large"] != 1 || len(skipped) != 2 {
		t.Fatalf("skip breakdown did not round-trip: %v", skipped)
	}
}

// The Python worker keeps sending the sentence-derived summary with none of the
// new fields. It must map exactly as before and report no typed result.
func TestOutputServerLegacySummaryHasNoTypedResult(t *testing.T) {
	stream, indexes := publishIndexSummary(t, &runtimev1.IndexIngestSummaryV1{
		Status:        runtimev1.IndexIngestStatusV1_INDEX_INGEST_STATUS_V1_OK,
		Message:       "Successfully indexed 3 documents (7 chunks).",
		TerminalState: runtimev1.IndexIngestTerminalStateV1_INDEX_INGEST_TERMINAL_STATE_V1_COMPLETED,
		Indexed:       3,
	})
	if len(stream.acks) != 2 || stream.acks[1].GetRejection() != nil || len(indexes.frames) != 1 {
		t.Fatalf("legacy summary was not accepted: acks=%v frames=%d", stream.acks, len(indexes.frames))
	}
	if summary := indexes.frames[0].Result.ResultSummary; summary.HasTypedResult() || summary.SkippedJSON != "" || len(summary.Skipped()) != 0 {
		t.Fatalf("legacy summary gained typed fields: %+v", summary)
	}
	if result := indexes.frames[0].Result; result.Documents != nil || result.RemovedDocumentKeys != nil || result.DocumentsTruncated {
		t.Fatalf("legacy summary gained document lists: %+v", result)
	}
}

func TestOutputServerRefusesMalformedTypedIndexSummaries(t *testing.T) {
	base := func() *runtimev1.IndexIngestSummaryV1 {
		return &runtimev1.IndexIngestSummaryV1{
			Status:        runtimev1.IndexIngestStatusV1_INDEX_INGEST_STATUS_V1_OK,
			Message:       "Indexed.",
			TerminalState: runtimev1.IndexIngestTerminalStateV1_INDEX_INGEST_TERMINAL_STATE_V1_COMPLETED,
		}
	}
	tests := map[string]func(*runtimev1.IndexIngestSummaryV1){
		"model without a dimension": func(s *runtimev1.IndexIngestSummaryV1) { s.EmbeddingModel = "m" },
		"dimension without a model": func(s *runtimev1.IndexIngestSummaryV1) { s.EmbeddingDimension = 8 },
		"dimension beyond any model": func(s *runtimev1.IndexIngestSummaryV1) {
			s.EmbeddingModel, s.EmbeddingDimension = "m", outputapp.MaxIndexEmbeddingDimension+1
		},
		"free-text skip reason":  func(s *runtimev1.IndexIngestSummaryV1) { s.Skipped = map[string]uint64{"File too big!": 1} },
		"empty skip reason":      func(s *runtimev1.IndexIngestSummaryV1) { s.Skipped = map[string]uint64{"": 1} },
		"upper-case skip reason": func(s *runtimev1.IndexIngestSummaryV1) { s.Skipped = map[string]uint64{"TooBig": 1} },
		"too many skip reasons": func(s *runtimev1.IndexIngestSummaryV1) {
			s.Skipped = map[string]uint64{}
			for i := 0; i <= outputapp.MaxIndexSkipReasons; i++ {
				s.Skipped["reason_"+string(rune('a'+i%26))+string(rune('a'+i/26))] = 1
			}
		},
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			summary := base()
			mutate(summary)
			stream, indexes := publishIndexSummary(t, summary)
			if len(indexes.frames) != 0 || len(stream.acks) != 2 ||
				stream.acks[1].GetRejection().GetCode() != runtimev1.RuntimeErrorCodeV1_RUNTIME_ERROR_CODE_V1_PROTOCOL_VIOLATION {
				t.Fatalf("malformed typed summary was not refused: acks=%v frames=%d", stream.acks, len(indexes.frames))
			}
		})
	}
}

// ADR-0030 I1: the per-document rows (documents written, keys removed, the
// truncation flag) and the whole index's totals ride the typed summary and are
// mapped onto the result, bounded by the application layer's rules.
func TestOutputServerMapsTypedIndexDocumentsAndTotals(t *testing.T) {
	stream, indexes := publishIndexSummary(t, &runtimev1.IndexIngestSummaryV1{
		Status:             runtimev1.IndexIngestStatusV1_INDEX_INGEST_STATUS_V1_OK,
		Message:            "Indexed 2 documents (5 chunks).",
		TerminalState:      runtimev1.IndexIngestTerminalStateV1_INDEX_INGEST_TERMINAL_STATE_V1_COMPLETED,
		IndexedDocuments:   2,
		IndexedChunks:      5,
		EmbeddingModel:     "text-embedding-3-small",
		EmbeddingDimension: 1536,
		Documents: []*runtimev1.IndexIngestDocumentV1{
			{DocumentKey: "docs/a.md", Version: "etag-1", ChunkCount: 2},
			{DocumentKey: "docs/b.md", Version: "etag-2", ChunkCount: 3},
		},
		RemovedDocumentKeys: []string{"docs/gone.md"},
		DocumentsTruncated:  true,
		TotalDocuments:      proto.Uint64(40),
		TotalChunks:         proto.Uint64(120),
		UnchangedDocuments:  proto.Uint64(37),
	})
	if len(stream.acks) != 2 || stream.acks[1].GetRejection() != nil || len(indexes.frames) != 1 {
		t.Fatalf("typed documents were not accepted: acks=%v frames=%d", stream.acks, len(indexes.frames))
	}
	result := indexes.frames[0].Result
	if len(result.Documents) != 2 || result.Documents[0] != (outputapp.IndexIngestDocument{Key: "docs/a.md", Version: "etag-1", ChunkCount: 2}) ||
		result.Documents[1] != (outputapp.IndexIngestDocument{Key: "docs/b.md", Version: "etag-2", ChunkCount: 3}) ||
		len(result.RemovedDocumentKeys) != 1 || result.RemovedDocumentKeys[0] != "docs/gone.md" || !result.DocumentsTruncated {
		t.Fatalf("document lists changed during mapping: %+v", result)
	}
	if !result.ResultSummary.TotalsPresent || result.ResultSummary.TotalDocuments != 40 || result.ResultSummary.TotalChunks != 120 {
		t.Fatalf("totals changed during mapping: %+v", result.ResultSummary)
	}
	if !result.ResultSummary.UnchangedPresent || result.ResultSummary.UnchangedDocuments != 37 {
		t.Fatalf("unchanged count changed during mapping: %+v", result.ResultSummary)
	}
	if result.ReportsWholeIndex() {
		t.Fatal("a truncated list of 2 in an index of 40 is not the whole index")
	}
}

// The totals' presence and the whole-index statement are carried as the
// worker set them: zero totals present stay present, and absent totals stay
// absent (zero without presence).
func TestOutputServerMapsTotalsPresenceAndTheWholeIndexStatement(t *testing.T) {
	stream, indexes := publishIndexSummary(t, &runtimev1.IndexIngestSummaryV1{
		Status:              runtimev1.IndexIngestStatusV1_INDEX_INGEST_STATUS_V1_OK,
		Message:             "Every document was removed.",
		TerminalState:       runtimev1.IndexIngestTerminalStateV1_INDEX_INGEST_TERMINAL_STATE_V1_COMPLETED,
		RemovedDocumentKeys: []string{"docs/a.md", "docs/b.md"},
		TotalDocuments:      proto.Uint64(0),
		TotalChunks:         proto.Uint64(0),
		WholeIndexRecord:    true,
	})
	if len(stream.acks) != 2 || stream.acks[1].GetRejection() != nil || len(indexes.frames) != 1 {
		t.Fatalf("a removal-only whole-index result was not accepted: acks=%v frames=%d", stream.acks, len(indexes.frames))
	}
	result := indexes.frames[0].Result
	if !result.ResultSummary.TotalsPresent || result.ResultSummary.TotalDocuments != 0 || !result.ResultSummary.HasTypedResult() ||
		!result.ReportsWholeIndex() || len(result.RemovedDocumentKeys) != 2 {
		t.Fatalf("mapped = %+v", result)
	}
	stream, indexes = publishIndexSummary(t, &runtimev1.IndexIngestSummaryV1{
		Status:           runtimev1.IndexIngestStatusV1_INDEX_INGEST_STATUS_V1_OK,
		Message:          "Indexed.",
		TerminalState:    runtimev1.IndexIngestTerminalStateV1_INDEX_INGEST_TERMINAL_STATE_V1_COMPLETED,
		IndexedDocuments: 1,
	})
	if len(indexes.frames) != 1 || indexes.frames[0].Result.ResultSummary.TotalsPresent || indexes.frames[0].Result.ReportsWholeIndex() {
		t.Fatalf("absent totals were mapped as present: acks=%v %+v", stream.acks, indexes.frames)
	}
}

func TestOutputServerRefusesMalformedTypedIndexDocuments(t *testing.T) {
	typed := func() *runtimev1.IndexIngestSummaryV1 {
		return &runtimev1.IndexIngestSummaryV1{
			Status:           runtimev1.IndexIngestStatusV1_INDEX_INGEST_STATUS_V1_OK,
			Message:          "Indexed.",
			TerminalState:    runtimev1.IndexIngestTerminalStateV1_INDEX_INGEST_TERMINAL_STATE_V1_COMPLETED,
			IndexedDocuments: 1,
			TotalDocuments:   proto.Uint64(1),
			TotalChunks:      proto.Uint64(1),
		}
	}
	tests := map[string]func(*runtimev1.IndexIngestSummaryV1){
		"documents on an untyped (python) summary": func(s *runtimev1.IndexIngestSummaryV1) {
			s.IndexedDocuments, s.TotalDocuments, s.TotalChunks, s.Indexed = 0, nil, nil, 1
			s.Documents = []*runtimev1.IndexIngestDocumentV1{{DocumentKey: "a", Version: "v"}}
		},
		"half a totals pair": func(s *runtimev1.IndexIngestSummaryV1) { s.TotalChunks = nil },
		"whole-index statement on a truncated list": func(s *runtimev1.IndexIngestSummaryV1) {
			s.Documents = []*runtimev1.IndexIngestDocumentV1{{DocumentKey: "a", Version: "v"}}
			s.DocumentsTruncated, s.WholeIndexRecord = true, true
		},
		"whole-index statement disagreeing with its totals": func(s *runtimev1.IndexIngestSummaryV1) {
			s.Documents = []*runtimev1.IndexIngestDocumentV1{{DocumentKey: "a", Version: "v"}}
			s.TotalDocuments, s.WholeIndexRecord = proto.Uint64(2), true
		},
		"whole-index statement without totals": func(s *runtimev1.IndexIngestSummaryV1) {
			s.TotalDocuments, s.TotalChunks, s.WholeIndexRecord = nil, nil, true
		},
		"empty document key": func(s *runtimev1.IndexIngestSummaryV1) {
			s.Documents = []*runtimev1.IndexIngestDocumentV1{{DocumentKey: "", Version: "v"}}
		},
		"nil document entry": func(s *runtimev1.IndexIngestSummaryV1) {
			s.Documents = []*runtimev1.IndexIngestDocumentV1{nil}
		},
		"duplicate document key": func(s *runtimev1.IndexIngestSummaryV1) {
			s.Documents = []*runtimev1.IndexIngestDocumentV1{{DocumentKey: "a", Version: "1"}, {DocumentKey: "a", Version: "2"}}
		},
		"key both written and removed": func(s *runtimev1.IndexIngestSummaryV1) {
			s.Documents = []*runtimev1.IndexIngestDocumentV1{{DocumentKey: "a", Version: "1"}}
			s.RemovedDocumentKeys = []string{"a"}
		},
		"empty removed key": func(s *runtimev1.IndexIngestSummaryV1) { s.RemovedDocumentKeys = []string{""} },
		"too many documents": func(s *runtimev1.IndexIngestSummaryV1) {
			for i := 0; i <= outputapp.MaxIndexResultDocuments; i++ {
				s.Documents = append(s.Documents, &runtimev1.IndexIngestDocumentV1{DocumentKey: fmt.Sprintf("k%d", i), Version: "v"})
			}
		},
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			summary := typed()
			mutate(summary)
			stream, indexes := publishIndexSummary(t, summary)
			if len(indexes.frames) != 0 || len(stream.acks) != 2 ||
				stream.acks[1].GetRejection().GetCode() != runtimev1.RuntimeErrorCodeV1_RUNTIME_ERROR_CODE_V1_PROTOCOL_VIOLATION {
				t.Fatalf("malformed document list was not refused: acks=%v frames=%d", stream.acks, len(indexes.frames))
			}
		})
	}
}

// ADR-0030 I1: a result the registry refuses (another embedding space than the
// stamp, or no typed summary) is rejected with RESULT_REFUSED, not retryable,
// and the worker then settles the run failed with a RuntimeError of that code,
// which the runtime-failure path accepts under its fixed public policy.
func TestOutputServerRejectsARefusedIndexResultWithResultRefused(t *testing.T) {
	frame := validIndexWireFrame(t)
	payload := frame.GetIndexIngest()
	payload.ResultArtifact = nil
	payload.ResultSummary = &runtimev1.IndexIngestSummaryV1{
		Status:             runtimev1.IndexIngestStatusV1_INDEX_INGEST_STATUS_V1_OK,
		Message:            "Indexed.",
		TerminalState:      runtimev1.IndexIngestTerminalStateV1_INDEX_INGEST_TERMINAL_STATE_V1_COMPLETED,
		IndexedDocuments:   1,
		IndexedChunks:      1,
		EmbeddingModel:     "other-model",
		EmbeddingDimension: 768,
	}
	rebindFramePayload(t, frame, payload)
	indexes := &indexIngestorStub{err: fmt.Errorf("project index ingest: %w: embedding dimension changed", outputapp.ErrIndexResultRefused)}
	server := newIndexOutputTestServer(t, 64*1024, &validationIngestorStub{}, &failureIngestorStub{}, indexes)
	stream := &outputStreamStub{context: context.Background(), frames: []*runtimev1.ExecutionOutputFrameV1{frame}}
	if err := server.Publish(stream); err != nil {
		t.Fatal(err)
	}
	if len(stream.acks) != 2 {
		t.Fatalf("acks = %v", stream.acks)
	}
	rejection := stream.acks[1].GetRejection()
	if rejection.GetCode() != runtimev1.RuntimeErrorCodeV1_RUNTIME_ERROR_CODE_V1_RESULT_REFUSED || rejection.GetRetryable() ||
		!strings.Contains(rejection.GetSafeMessage(), "refused") || strings.Contains(rejection.GetSafeMessage(), "dimension changed") {
		t.Fatalf("rejection = %+v, want RESULT_REFUSED with the fixed safe message", rejection)
	}

	policy, ok := runtimeFailurePolicyFor(runtimev1.RuntimeErrorCodeV1_RUNTIME_ERROR_CODE_V1_RESULT_REFUSED)
	if !ok || policy.Code != "RESULT_REFUSED" || policy.Retryable || policy.SafeMessage != "The index result was refused by the platform." {
		t.Fatalf("RESULT_REFUSED failure policy = %+v ok=%v", policy, ok)
	}
	// The worker's FAILED terminal is accepted only with that exact text.
	accepted, ok := runtimeFailurePolicyForError(&runtimev1.RuntimeErrorV1{
		Code: runtimev1.RuntimeErrorCodeV1_RUNTIME_ERROR_CODE_V1_RESULT_REFUSED, SafeMessage: policy.SafeMessage,
	})
	if !ok || accepted != policy {
		t.Fatalf("policy for the worker's terminal = %+v ok=%v", accepted, ok)
	}
}
