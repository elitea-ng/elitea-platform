package output

import (
	"errors"
	"math"
	"strings"
	"testing"
)

// typedIndexResult is a valid inline (no artifact) result of a Rust run with a
// typed summary, before any per-document list is attached.
func typedIndexResult() IndexIngestResult {
	frame, _, _ := validIndexIngestOutput()
	result := frame.Result
	result.ResultArtifact = IndexArtifactReference{}
	result.ResultSummary = IndexIngestSummary{
		Status:             IndexIngestStatusOK,
		Message:            "Indexed 3 documents.",
		TerminalState:      IndexIngestTerminalCompleted,
		IndexedDocuments:   3,
		IndexedChunks:      9,
		EmbeddingModel:     "text-embedding-3-small",
		EmbeddingDimension: 1536,
		TotalsPresent:      true,
		TotalDocuments:     3,
		TotalChunks:        9,
	}
	return result
}

// ADR-0030 I1: the result carries the run's per-document rows beside the
// comparable summary, bounded, with distinct keys that are in one list only.
func TestIndexIngestResultAcceptsBoundedDistinctDocumentLists(t *testing.T) {
	result := typedIndexResult()
	result.Documents = []IndexIngestDocument{
		{Key: "docs/a.md", Version: "etag-1", ChunkCount: 4},
		{Key: "docs/b.md", Version: "", ChunkCount: 0},
	}
	result.RemovedDocumentKeys = []string{"docs/gone.md"}
	if err := result.Validate(); err != nil {
		t.Fatalf("a typed result with document lists = %v", err)
	}
	result.DocumentsTruncated = true
	if err := result.Validate(); err != nil {
		t.Fatalf("a truncated list is a valid (bounded) list: %v", err)
	}
	// The Python path: no lists, no typed summary.
	frame, _, _ := validIndexIngestOutput()
	if err := frame.Result.Validate(); err != nil {
		t.Fatalf("an artifact result without lists = %v", err)
	}
}

func TestIndexIngestResultRefusesMalformedDocumentLists(t *testing.T) {
	many := func(n int) []IndexIngestDocument {
		out := make([]IndexIngestDocument, 0, n)
		for i := 0; i < n; i++ {
			out = append(out, IndexIngestDocument{Key: "k" + string(rune('a'+i%26)) + strings.Repeat("x", i/26), Version: "v"})
		}
		return out
	}
	cases := map[string]func(*IndexIngestResult){
		"documents without a typed summary": func(r *IndexIngestResult) {
			r.ResultSummary = IndexIngestSummary{Status: IndexIngestStatusOK, Message: "ok", TerminalState: IndexIngestTerminalCompleted, Indexed: 1}
			r.Documents = []IndexIngestDocument{{Key: "a", Version: "v"}}
		},
		"removed keys without a typed summary": func(r *IndexIngestResult) {
			r.ResultSummary = IndexIngestSummary{Status: IndexIngestStatusOK, Message: "ok", TerminalState: IndexIngestTerminalCompleted, Indexed: 1}
			r.RemovedDocumentKeys = []string{"a"}
		},
		"truncated flag without a typed summary": func(r *IndexIngestResult) {
			r.ResultSummary = IndexIngestSummary{Status: IndexIngestStatusOK, Message: "ok", TerminalState: IndexIngestTerminalCompleted, Indexed: 1}
			r.DocumentsTruncated = true
		},
		"documents beside an artifact": func(r *IndexIngestResult) {
			frame, _, _ := validIndexIngestOutput()
			*r = frame.Result
			r.Documents = []IndexIngestDocument{{Key: "a", Version: "v"}}
		},
		"empty key":  func(r *IndexIngestResult) { r.Documents = []IndexIngestDocument{{Key: "", Version: "v"}} },
		"NUL in key": func(r *IndexIngestResult) { r.Documents = []IndexIngestDocument{{Key: "a\x00b", Version: "v"}} },
		"invalid UTF-8 key": func(r *IndexIngestResult) {
			r.Documents = []IndexIngestDocument{{Key: "a\xffb", Version: "v"}}
		},
		"oversized key": func(r *IndexIngestResult) {
			r.Documents = []IndexIngestDocument{{Key: strings.Repeat("k", MaxIndexDocumentKeyBytes+1), Version: "v"}}
		},
		"oversized version": func(r *IndexIngestResult) {
			r.Documents = []IndexIngestDocument{{Key: "a", Version: strings.Repeat("v", MaxIndexDocumentVersionBytes+1)}}
		},
		"NUL in version":    func(r *IndexIngestResult) { r.Documents = []IndexIngestDocument{{Key: "a", Version: "v\x00"}} },
		"empty removed key": func(r *IndexIngestResult) { r.RemovedDocumentKeys = []string{""} },
		"duplicate document key": func(r *IndexIngestResult) {
			r.Documents = []IndexIngestDocument{{Key: "a", Version: "1"}, {Key: "a", Version: "2"}}
		},
		"duplicate removed key": func(r *IndexIngestResult) { r.RemovedDocumentKeys = []string{"a", "a"} },
		"key written and removed": func(r *IndexIngestResult) {
			r.Documents = []IndexIngestDocument{{Key: "a", Version: "1"}}
			r.RemovedDocumentKeys = []string{"a"}
		},
		"too many documents": func(r *IndexIngestResult) { r.Documents = many(MaxIndexResultDocuments + 1) },
		"too many removed keys": func(r *IndexIngestResult) {
			for _, document := range many(MaxIndexResultDocuments + 1) {
				r.RemovedDocumentKeys = append(r.RemovedDocumentKeys, document.Key)
			}
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			result := typedIndexResult()
			mutate(&result)
			if err := result.Validate(); !errors.Is(err, ErrInvalidIndexIngestOutput) {
				t.Fatalf("validate = %v, want ErrInvalidIndexIngestOutput", err)
			}
		})
	}
	// The bound itself is accepted.
	result := typedIndexResult()
	result.Documents = many(MaxIndexResultDocuments)
	if err := result.Validate(); err != nil {
		t.Fatalf("exactly MaxIndexResultDocuments documents = %v", err)
	}
}

// The totals are the store after the run with an explicit presence; a summary
// carrying only them (zero included) is a typed (Rust) result, a value without
// presence is malformed, and they are bounded like every other count.
func TestIndexIngestSummaryTotalsAreTypedAndBounded(t *testing.T) {
	summary := IndexIngestSummary{
		Status: IndexIngestStatusOK, Message: "Nothing changed.", TerminalState: IndexIngestTerminalCompleted,
		TotalsPresent: true, TotalDocuments: 12, TotalChunks: 40,
	}
	if !summary.HasTypedResult() {
		t.Fatal("a summary with totals is a typed result")
	}
	if err := summary.Validate(); err != nil {
		t.Fatalf("totals only = %v", err)
	}
	// A run that emptied the store reports zero totals, present: still typed,
	// so the registry resets its totals instead of ignoring the run.
	emptied := IndexIngestSummary{
		Status: IndexIngestStatusOK, Message: "Everything was removed.", TerminalState: IndexIngestTerminalCompleted,
		TotalsPresent: true,
	}
	if err := emptied.Validate(); err != nil || !emptied.HasTypedResult() {
		t.Fatalf("zero totals, present = %v typed=%v", err, emptied.HasTypedResult())
	}
	// The whole-index statement alone is typed too, and refused on its own
	// (its totals are what it is checked against).
	if (IndexIngestSummary{Status: IndexIngestStatusOK, Message: "ok", TerminalState: IndexIngestTerminalCompleted, WholeIndexRecord: true}).Validate() == nil {
		t.Fatal("a whole-index statement without totals was accepted")
	}
	for name, mutate := range map[string]func(*IndexIngestSummary){
		"documents beyond int64":     func(s *IndexIngestSummary) { s.TotalDocuments = math.MaxInt64 + 1 },
		"chunks beyond int64":        func(s *IndexIngestSummary) { s.TotalChunks = math.MaxInt64 + 1 },
		"documents without presence": func(s *IndexIngestSummary) { s.TotalsPresent = false },
		"chunks without presence": func(s *IndexIngestSummary) {
			s.TotalsPresent, s.TotalDocuments = false, 0
		},
	} {
		t.Run(name, func(t *testing.T) {
			s := summary
			mutate(&s)
			if err := s.Validate(); !errors.Is(err, ErrInvalidIndexIngestOutput) {
				t.Fatalf("validate = %v", err)
			}
		})
	}
}

// ReportsWholeIndex is the clean-index decision: the registry may delete every
// document row the list does not name only when the worker STATED the list is
// the index (whole_index_record). Counts never imply it; the statement is
// checked against the list instead.
func TestIndexIngestResultReportsWholeIndexOnlyWhenTheWorkerStatesIt(t *testing.T) {
	result := typedIndexResult()
	result.Documents = []IndexIngestDocument{{Key: "a", Version: "1"}, {Key: "b", Version: "1"}, {Key: "c", Version: "1"}}
	result.ResultSummary.TotalDocuments = 3
	if result.ReportsWholeIndex() {
		t.Fatal("three documents of an index of three is not a whole-index statement")
	}
	if err := result.Validate(); err != nil {
		t.Fatalf("an incremental result = %v", err)
	}
	result.ResultSummary.WholeIndexRecord = true
	if !result.ReportsWholeIndex() {
		t.Fatal("the worker's statement was not read")
	}
	if err := result.Validate(); err != nil {
		t.Fatalf("a whole-index record of three = %v", err)
	}
	for name, mutate := range map[string]func(*IndexIngestResult){
		"truncated":       func(r *IndexIngestResult) { r.DocumentsTruncated = true },
		"totals disagree": func(r *IndexIngestResult) { r.ResultSummary.TotalDocuments = 10 },
		"totals absent": func(r *IndexIngestResult) {
			r.ResultSummary.TotalsPresent, r.ResultSummary.TotalDocuments, r.ResultSummary.TotalChunks = false, 0, 0
		},
		"list shorter than it": func(r *IndexIngestResult) { r.Documents = r.Documents[:2] },
	} {
		t.Run(name, func(t *testing.T) {
			stated := result
			stated.Documents = append([]IndexIngestDocument(nil), result.Documents...)
			mutate(&stated)
			if err := stated.Validate(); !errors.Is(err, ErrInvalidIndexIngestOutput) {
				t.Fatalf("a whole-index statement that does not describe its list: %v", err)
			}
		})
	}
	// A run that emptied the index: no documents, totals zero (present), the
	// worker states the (empty) list is the index.
	emptied := typedIndexResult()
	emptied.Documents = nil
	emptied.RemovedDocumentKeys = []string{"a", "b"}
	emptied.ResultSummary.TotalDocuments, emptied.ResultSummary.TotalChunks = 0, 0
	emptied.ResultSummary.WholeIndexRecord = true
	if err := emptied.Validate(); err != nil || !emptied.ReportsWholeIndex() {
		t.Fatalf("an index stated empty: err=%v whole=%v", err, emptied.ReportsWholeIndex())
	}
	// A run that only removed documents, with no other typed field than the
	// (zero) totals, is a valid typed result.
	removalOnly := typedIndexResult()
	removalOnly.ResultSummary = IndexIngestSummary{
		Status: IndexIngestStatusOK, Message: "Two documents were removed.", TerminalState: IndexIngestTerminalCompleted,
		TotalsPresent: true,
	}
	removalOnly.RemovedDocumentKeys = []string{"a", "b"}
	if err := removalOnly.Validate(); err != nil || removalOnly.ReportsWholeIndex() {
		t.Fatalf("a removal-only result: err=%v whole=%v", err, removalOnly.ReportsWholeIndex())
	}
	// A failed run that wrote before failing reports what it wrote and the
	// store's totals; the record is applied like any other.
	failed := typedIndexResult()
	failed.ResultSummary.Status, failed.ResultSummary.TerminalState = IndexIngestStatusError, IndexIngestTerminalFailed
	failed.ResultSummary.EmbeddingModel, failed.ResultSummary.EmbeddingDimension = "", 0
	failed.ResultSummary.TotalDocuments, failed.ResultSummary.TotalChunks = 1, 2
	failed.Documents = []IndexIngestDocument{{Key: "a", Version: "1", ChunkCount: 2}}
	failed.RemovedDocumentKeys = []string{"dropped"}
	if err := failed.Validate(); err != nil {
		t.Fatalf("a failed result with its record = %v", err)
	}
	// A Python (untyped) result reports nothing about the whole index.
	frame, _, _ := validIndexIngestOutput()
	if frame.Result.ReportsWholeIndex() {
		t.Fatal("an untyped result must not clean the index")
	}
}

// The projector receives its own copy of the lists: a later mutation of the
// caller's frame cannot change what was projected.
func TestIndexIngestServiceClonesTheDocumentLists(t *testing.T) {
	frame, expected, _ := validIndexIngestOutput()
	frame.Result = typedIndexResult()
	frame.Result.Documents = []IndexIngestDocument{{Key: "a", Version: "1", ChunkCount: 2}}
	frame.Result.RemovedDocumentKeys = []string{"gone"}
	projector := &indexProjectorStub{}
	service, err := NewIndexIngestService(
		&indexBindingRepositoryStub{expected: expected},
		&indexFenceVerifierStub{},
		&indexArtifactVerifierStub{},
		projector,
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.IngestIndex(t.Context(), frame); err != nil {
		t.Fatal(err)
	}
	frame.Result.Documents[0].Key = "mutated"
	frame.Result.RemovedDocumentKeys[0] = "mutated"
	projected := projector.projections[0].Frame.Result
	if projected.Documents[0].Key != "a" || projected.RemovedDocumentKeys[0] != "gone" {
		t.Fatalf("the projection shares the caller's slices: %+v", projected)
	}
}
