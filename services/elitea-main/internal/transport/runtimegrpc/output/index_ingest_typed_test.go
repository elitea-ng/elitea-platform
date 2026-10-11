package output

import (
	"context"
	"testing"

	runtimev1 "github.com/EliteaAI/elitea-platform/libs/proto/gen/go/elitea/runtime/v1"
	outputapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/output"
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
