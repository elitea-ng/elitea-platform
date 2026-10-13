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
		s.UnchangedPresent = true
		s.UnchangedDocuments = 5
	})))
	if err != nil || mismatch != nil {
		t.Fatalf("apply: %v %v", err, mismatch)
	}
	return next
}

func reportOf(t *testing.T, row Row) Report {
	t.Helper()
	record, ok := recordOf(row.History[len(row.History)-1])
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

func TestUnchangedIsTheWorkersExactCount(t *testing.T) {
	cases := []struct {
		name      string
		present   bool
		unchanged uint64
		want      uint64
	}{
		{"exact", true, 5, 5},
		{"skipped documents are not subtracted", true, 2, 2},
		{"absent from an older worker: no line, nothing derived", false, 0, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			row := started(t, nil, 1)
			next, _, _, err := applyResult(row, resultFor(1, okSummary(func(s *outputapp.IndexIngestSummary) {
				s.IndexedDocuments = 3
				s.FailedChunks = 0
				s.SkippedJSON = `{"too_large":2}`
				s.TotalsPresent = true
				s.TotalDocuments = 30
				s.TotalChunks = 40
				s.UnchangedPresent = tc.present
				s.UnchangedDocuments = tc.unchanged
			})))
			if err != nil {
				t.Fatal(err)
			}
			report := reportOf(t, next)
			if report.Totals.Unchanged != tc.want {
				t.Fatalf("unchanged = %d, want %d", report.Totals.Unchanged, tc.want)
			}
			hasLine := false
			for _, group := range report.Categories[1].Groups {
				hasLine = hasLine || group.Reason == "unchanged"
			}
			if hasLine != tc.present {
				t.Fatalf("unchanged line present = %v, want %v", hasLine, tc.present)
			}
		})
	}
}

func TestUnchangedGroupComesFirstAndUnknownTokensStayApart(t *testing.T) {
	report := runRecord{
		Status: statusOK, Unchanged: 4,
		// "unsupported_format" is a group KEY of a known reason, not a known
		// token: as a token it is unknown and must not merge into that group.
		Skipped: map[string]uint64{"empty": 1, "unsupported_type": 2, "unsupported_format": 3, "zzz": 1},
	}.Report()
	if got := report.Categories[1].Groups; got[0].Reason != "unchanged" || got[1].Reason != "empty" {
		t.Fatalf("skipped groups: %+v", got)
	}
	var reasons []string
	for _, group := range report.Categories[2].Groups {
		reasons = append(reasons, group.Reason+":"+group.Code)
	}
	if strings.Join(reasons, ",") != "unsupported_format:unsupported_format,unsupported_format:,zzz:zzz" {
		t.Fatalf("not-indexed groups: %v", reasons)
	}
	if report.Categories[2].Groups[0].Count != 3 || report.Categories[2].Groups[1].Count != 2 {
		t.Fatalf("counts: %+v", report.Categories[2].Groups)
	}
}

func TestErrorRunWithoutAMessageHasNoBlankErrorLine(t *testing.T) {
	record := resultRecord(outputapp.IndexIngestSummary{Status: outputapp.IndexIngestStatusError}, statusError)
	report := record.Report()
	if len(record.Errors) != 0 || report.ErrorsTotal != 0 || len(report.Errors) != 0 {
		t.Fatalf("record %+v report %+v", record, report)
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
	if metadata["indexed"] != done.TotalDocuments {
		t.Fatalf("indexed = %v: the list badge shows what the store holds", metadata["indexed"])
	}
	if _, has := metadata["index_total_documents"]; has {
		t.Fatal("index_total_documents is gone")
	}
	if metadata["updated"] != int64(0) || metadata["failed_chunks"] != int64(0) || len(metadata["skipped"].(map[string]any)) != 0 {
		t.Fatalf("the flat per-run keys describe the failed run: %v %v %v", metadata["updated"], metadata["failed_chunks"], metadata["skipped"])
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
	for _, key := range []string{"indexed", "indexed_chunks", "failed_chunks", "skipped"} {
		if _, dup := entry[key]; dup {
			t.Fatalf("%s duplicates the run record", key)
		}
	}
	metadata := Metadata(failed)
	if metadata["indexed"] != done.TotalDocuments || metadata["updated"] != int64(0) ||
		metadata["failed_chunks"] != int64(0) || len(metadata["skipped"].(map[string]any)) != 0 {
		t.Fatalf("top-level keys must describe the failed schedule: %v", metadata)
	}
	report := metadata["report"].(Report)
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
	if metadata["indexed"] != int64(21) {
		t.Fatalf("a completed run keeps the index total as indexed: %v", metadata["indexed"])
	}
	if _, ok := metadata["skipped"].(map[string]any)["too_large"]; !ok {
		t.Fatal("the flat skipped map is kept")
	}
	history := metadata["history"].([]any)
	entry := history[len(history)-1].(map[string]any)
	slim, ok := entry["report"].(map[string]any)
	if !ok || entry["total"] != uint64(21) {
		t.Fatalf("the history entry lacks the slim report: %v", entry)
	}
	if _, labels := slim["item_labels"]; labels {
		t.Fatal("a list response's history entries carry the slim report")
	}
	exact := MetadataExact(row)["history"].([]any)
	if _, full := exact[len(exact)-1].(map[string]any)["report"].(Report); !full {
		t.Fatal("an exact read expands every entry in full")
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
	served, err := json.Marshal(expandEntry(row.History[len(row.History)-1], runRecordOf(t, row.History[len(row.History)-1]), true, true))
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

func runRecordOf(t *testing.T, entry map[string]any) runRecord {
	t.Helper()
	record, ok := recordOf(entry)
	if !ok {
		t.Fatal("no record")
	}
	return record
}

// A list of indexes with deep histories must not carry a full report per run.
func TestListPayloadWithDeepHistoriesStaysSmall(t *testing.T) {
	row := rustRunRow(t)
	for run := 2; run < 2+MaxHistoryEntries; run++ {
		next := started(t, &row, run)
		done, _, _, err := applyResult(next, resultFor(run, okSummary(func(s *outputapp.IndexIngestSummary) {
			s.IndexedDocuments = 1
			s.SkippedJSON = `{"too_large":2,"unsupported_type":1}`
			s.TotalsPresent = true
			s.TotalDocuments = 21
			s.TotalChunks = 40
			s.UnchangedPresent = true
			s.UnchangedDocuments = 17
		})))
		if err != nil {
			t.Fatal(err)
		}
		row = done
	}
	list, err := json.Marshal(Metadata(row))
	if err != nil {
		t.Fatal(err)
	}
	exact, err := json.Marshal(MetadataExact(row))
	if err != nil {
		t.Fatal(err)
	}
	if len(list) >= len(exact)*9/10 {
		t.Fatalf("list %d bytes vs exact %d: history entries are not slim", len(list), len(exact))
	}
	if n := strings.Count(string(list), "dependent_labels"); n != 1 {
		t.Fatalf("only the newest report is full; %d full reports in the list", n)
	}
	// Each stored entry holds the run's counts once.
	for _, entry := range row.History[1:] {
		if _, dup := entry["skipped"]; dup {
			t.Fatalf("duplicated counts in a stored entry: %v", entry)
		}
	}
}
