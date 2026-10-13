package indexregistry

import (
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	indexingapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/indexing"
	outputapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/output"
)

// rustRunRow is a finished Rust run: 3 documents written, 13 skipped, 21 in the
// index, so 5 are unchanged, with every kind of skip.
func rustRunRow(t *testing.T) Row {
	t.Helper()
	row := started(t, nil, 1)
	next, _, mismatch, err := applyResult(row, resultFor(1, okSummary(func(s *outputapp.IndexIngestSummary) {
		s.Status = outputapp.IndexIngestStatusPartlyIndexed
		s.TerminalState = outputapp.IndexIngestTerminalPartlyIndexed
		s.IndexedDocuments = 3
		s.IndexedChunks = 9
		s.FailedChunks = 1
		s.SkippedJSON = `{"empty":1,"fetch_failed":1,"too_large":2,"unreadable":2,"unsupported_type":4,"weird_new_reason":3}`
		s.TotalsPresent = true
		s.TotalDocuments = 21
		s.TotalChunks = 40
	})))
	if err != nil || mismatch != nil {
		t.Fatalf("apply: %v %v", err, mismatch)
	}
	return next
}

func reportOf(t *testing.T, row Row) Report {
	t.Helper()
	record, ok := lastRecord(row.History)
	if !ok {
		t.Fatalf("no run record on the newest history entry: %v", row.History[len(row.History)-1])
	}
	return record.Report()
}

func totalsOf(t *testing.T, metadata map[string]any) ReportTotals {
	t.Helper()
	report, ok := metadata["report"].(Report)
	if !ok {
		t.Fatalf("no report in metadata: %v", metadata["report"])
	}
	return report.Totals
}

func TestEveryKnownRustSkipReasonHasAGroup(t *testing.T) {
	for reason, spec := range skipReasonGroups {
		if spec.reason == "" || spec.label == "" {
			t.Errorf("%s maps to an incomplete group: %+v", reason, spec)
		}
		switch spec.kind {
		case kindSkipped, kindNotIndexed, kindFailed:
		default:
			t.Errorf("%s maps to kind %q", reason, spec.kind)
		}
	}
}

// TestSkipReasonTableCoversTheRustWorker is the drift guard: a SKIP_* constant
// added to the Rust worker without a row in skipReasonGroups would reach the
// UI as an "other" group.
func TestSkipReasonTableCoversTheRustWorker(t *testing.T) {
	root := filepath.Join("..", "..", "..", "..", "elitea-worker-rust", "src")
	pattern := regexp.MustCompile(`const SKIP_[A-Z_]+:\s*&str\s*=\s*"([a-z0-9_]+)"`)
	found := map[string]string{}
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(path, ".rs") {
			return nil
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		for _, match := range pattern.FindAllStringSubmatch(string(raw), -1) {
			found[match[1]] = path
		}
		return nil
	})
	if err != nil {
		t.Fatalf("read the Rust worker sources: %v", err)
	}
	if len(found) < 10 {
		t.Fatalf("expected the worker's SKIP_* constants, found %v", found)
	}
	for reason, path := range found {
		if _, ok := skipReasonGroups[reason]; !ok {
			t.Errorf("skip reason %q (%s) has no entry in skipReasonGroups", reason, path)
		}
	}
}

func TestUnknownSkipReasonGetsAGenericLabelAndKeepsItsToken(t *testing.T) {
	spec, known := groupForSkipReason("brand_new")
	if known || spec.kind != kindNotIndexed || spec.reason != "brand_new" || spec.label != unknownReasonLabel {
		t.Fatalf("unknown reason: %+v known=%v", spec, known)
	}
	report := runRecord{Status: statusOK, Skipped: map[string]uint64{"brand_new": 2}}.Report()
	group := report.Categories[2].Groups[0]
	if group.Label != "Not indexed (other)" || group.Code != "brand_new" || group.Count != 2 {
		t.Fatalf("group: %+v", group)
	}
	known1 := runRecord{Status: statusOK, Skipped: map[string]uint64{"too_large": 1}}.Report().Categories[2].Groups[0]
	if known1.Code != "" {
		t.Fatalf("a known reason carries no code: %+v", known1)
	}
}

func TestRunReportCountsAndMergesGroups(t *testing.T) {
	report := reportOf(t, rustRunRow(t))
	want := ReportTotals{Indexed: 3, Skipped: 1, NotIndexed: 9, Failed: 3, Unchanged: 5, Total: 21}
	// 21 documents in the index, 3 written, 13 skipped: the skipped documents
	// are not counted twice.
	if report.Totals != want {
		t.Errorf("totals = %+v, want %+v", report.Totals, want)
	}
	if report.Status != statusPartlyIndexed || report.Version != ReportVersion {
		t.Errorf("status/version: %v %v", report.Status, report.Version)
	}
	// fetch_failed and unreadable share the SDK's read_error group.
	failed := report.Categories[3].Groups
	if len(failed) != 1 || failed[0].Count != 3 {
		t.Errorf("failed groups: %v", failed)
	}
	if report.ErrorsTotal != 1 {
		t.Errorf("failed chunks should surface as an error: %v", report.Errors)
	}
}

func TestUnchangedIsDerivedWithoutDoubleCounting(t *testing.T) {
	cases := []struct {
		name      string
		indexed   uint64
		skipped   string
		total     uint64
		present   bool
		unchanged uint64
	}{
		{"plain", 3, ``, 8, true, 5},
		{"skipped documents still stored are not unchanged", 3, `{"too_large":2}`, 8, true, 3},
		{"never negative", 3, `{"too_large":9}`, 8, true, 0},
		{"stale totals are not read", 3, ``, 8, false, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			row := started(t, nil, 1)
			next, _, _, err := applyResult(row, resultFor(1, okSummary(func(s *outputapp.IndexIngestSummary) {
				s.IndexedDocuments = tc.indexed
				s.FailedChunks = 0
				s.SkippedJSON = tc.skipped
				s.TotalsPresent = tc.present
				if tc.present {
					s.TotalDocuments = tc.total
					s.TotalChunks = 40
				}
			})))
			if err != nil {
				t.Fatal(err)
			}
			if got := reportOf(t, next).Totals.Unchanged; got != tc.unchanged {
				t.Fatalf("unchanged = %d, want %d", got, tc.unchanged)
			}
		})
	}
}

func TestFailedAndCancelledRunsHaveNoUnchangedAndNoStaleTotals(t *testing.T) {
	// A previous run left totals on the row; the next run fails before any
	// result.
	done := rustRunRow(t)
	second := started(t, &done, 2)
	failed, _, err := ApplyTerminal(second, terminalFor(2, indexingapp.CurrentIndexMetaFailed, "boom"))
	if err != nil {
		t.Fatal(err)
	}
	metadata := Metadata(failed)
	totals := totalsOf(t, metadata)
	if totals.Unchanged != 0 || totals.Indexed != 0 || totals.Total != 0 {
		t.Fatalf("a failed run reports nothing it did not do: %+v", totals)
	}
	// The run-facing count is the run's own; the index totals are separate.
	if metadata["indexed"] != uint64(0) || metadata["index_total_documents"] != done.TotalDocuments {
		t.Fatalf("indexed=%v index_total_documents=%v", metadata["indexed"], metadata["index_total_documents"])
	}
	if metadata["total"] != uint64(0) {
		t.Fatalf("total = %v", metadata["total"])
	}
	report := metadata["report"].(Report)
	if report.Status != statusError || len(report.Errors) != 1 || report.Errors[0] != "boom" {
		t.Fatalf("report: %+v", report)
	}

	// A cancelled result that touched the store: counts kept, unchanged zero.
	third := started(t, &done, 2)
	cancelled, _, _, err := applyResult(third, resultFor(2, okSummary(func(s *outputapp.IndexIngestSummary) {
		s.Status = outputapp.IndexIngestStatusError
		s.TerminalState = outputapp.IndexIngestTerminalCancelled
		s.TotalsPresent = true
		s.TotalDocuments = 21
		s.TotalChunks = 80
		s.SkippedJSON = ""
	})))
	if err != nil {
		t.Fatal(err)
	}
	cancelledReport := reportOf(t, cancelled)
	if cancelledReport.Status != statusCancelled || cancelledReport.Totals.Unchanged != 0 || cancelledReport.Totals.Indexed != 3 ||
		len(cancelledReport.Errors) == 0 || cancelledReport.Errors[0] != cancelledMessage {
		t.Fatalf("cancelled report: %+v", cancelledReport)
	}
}

func TestCancelledRunWithoutResultIsCancelledNotError(t *testing.T) {
	second := started(t, nil, 1)
	cancelled, _, err := ApplyTerminal(second, terminalFor(1, indexingapp.CurrentIndexMetaCancelled, ""))
	if err != nil {
		t.Fatal(err)
	}
	report := reportOf(t, cancelled)
	if report.Status != "cancelled" || len(report.Errors) != 1 || report.ErrorsTotal != 1 {
		t.Fatalf("report: %+v", report)
	}
}

func TestFirstRunThatIndexedNothingButSkippedFilesHasAReport(t *testing.T) {
	row := started(t, nil, 1)
	next, _, _, err := applyResult(row, resultFor(1, outputapp.IndexIngestSummary{
		Status: outputapp.IndexIngestStatusOK, TerminalState: outputapp.IndexIngestTerminalCompleted,
		Message: "nothing to index", SkippedJSON: `{"unsupported_type":4}`,
		TotalsPresent: true,
	}))
	if err != nil {
		t.Fatal(err)
	}
	if next.Stamped() {
		t.Fatal("nothing was embedded, so the index is not stamped")
	}
	metadata := Metadata(next)
	totals := totalsOf(t, metadata)
	if totals.NotIndexed != 4 || totals.Total != 4 {
		t.Fatalf("totals: %+v", totals)
	}
	if _, ok := metadata["skipped"]; !ok {
		// skipped is stamped-only today; the report carries the breakdown.
		t.Log("flat skipped map is absent on an unstamped index")
	}
}

func TestScheduledFailureDoesNotReuseThePreviousRunsCounts(t *testing.T) {
	done := rustRunRow(t)
	failed, changed := RecordScheduledFailure(done, "effect-1", "schedule refused", t0.Add(2*time.Hour))
	if !changed {
		t.Fatal("not recorded")
	}
	entry := failed.History[len(failed.History)-1]
	record, ok := recordOf(entry)
	if !ok || record.Status != statusError || record.Indexed != 0 || len(record.Skipped) != 0 || record.Unchanged != 0 ||
		len(record.Errors) != 1 || record.Errors[0] != "schedule refused" {
		t.Fatalf("record: %+v", record)
	}
	if entry["indexed"] != int64(0) {
		t.Fatalf("the entry's own count: %v", entry["indexed"])
	}
	report := Metadata(failed)["report"].(Report)
	if report.Totals.Total != 0 || len(report.Categories[2].Groups) != 0 || report.Status != statusError {
		t.Fatalf("report: %+v", report)
	}
}

func TestMetadataCarriesReportAndTotalInRowAndHistory(t *testing.T) {
	row := rustRunRow(t)
	metadata := Metadata(row)
	if metadata["report"] == nil || metadata["total"] != uint64(21) {
		t.Fatalf("report/total missing: %v %v", metadata["report"], metadata["total"])
	}
	if metadata["indexed"] != int64(21) || metadata["index_total_documents"] != int64(21) {
		t.Fatalf("a completed run keeps the index total as indexed: %v", metadata["indexed"])
	}
	if _, ok := metadata["skipped"].(map[string]any)["too_large"]; !ok {
		t.Fatal("the flat skipped map is kept")
	}
	history := metadata["history"].([]any)
	entry := history[len(history)-1].(map[string]any)
	if entry["report"] == nil || entry["total"] != uint64(21) {
		t.Fatalf("the history entry lacks the report: %v", entry)
	}
	if _, stored := entry[runRecordKey]; stored {
		t.Fatal("the stored record is not served")
	}
	if _, stored := row.History[len(row.History)-1][runRecordKey]; !stored {
		t.Fatal("serving must not change the stored entry")
	}
}

func TestStoredHistoryEntryStaysSmall(t *testing.T) {
	row := rustRunRow(t)
	stored, err := json.Marshal(row.History[len(row.History)-1])
	if err != nil {
		t.Fatal(err)
	}
	served, err := json.Marshal(expandEntry(row.History[len(row.History)-1]))
	if err != nil {
		t.Fatal(err)
	}
	if len(stored) >= len(served)-800 {
		t.Fatalf("stored %d bytes vs served %d: the constants must not be stored", len(stored), len(served))
	}
	if strings.Contains(string(stored), "Unsupported format") || strings.Contains(string(stored), "dependent_labels") {
		t.Fatalf("labels were stored: %s", stored)
	}
}

// TestMetadataReportGolden pins the JSON against the shape of the SDK's
// build_report; the web test feeds the same file through
// normalizeIndexingReport. Regenerate with UPDATE_GOLDEN=1.
func TestMetadataReportGolden(t *testing.T) {
	report := Metadata(rustRunRow(t))["report"].(Report)
	got, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	path := "testdata/rust_run_report.golden.json"
	if os.Getenv("UPDATE_GOLDEN") != "" {
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, append(got, '\n'), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var a, b any
	_ = json.Unmarshal(got, &a)
	_ = json.Unmarshal(want, &b)
	ga, _ := json.Marshal(a)
	gb, _ := json.Marshal(b)
	if string(ga) != string(gb) {
		t.Fatalf("report drifted from the golden file:\n%s", got)
	}
}
