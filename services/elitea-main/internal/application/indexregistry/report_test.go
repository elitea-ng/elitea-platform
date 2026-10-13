package indexregistry

import (
	"encoding/json"
	"os"
	"sort"
	"testing"

	outputapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/output"
)

// rustRunRow is a finished Rust run: 3 documents written of 8 in the index,
// so 5 are unchanged, with every kind of skip.
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
		s.TotalDocuments = 8
		s.TotalChunks = 40
	})))
	if err != nil || mismatch != nil {
		t.Fatalf("apply: %v %v", err, mismatch)
	}
	return next
}

func TestEveryKnownRustSkipReasonHasAGroupAndUnknownOnesAreKept(t *testing.T) {
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
	unknown := groupForSkipReason("brand_new")
	if unknown.kind != kindNotIndexed || unknown.reason != "brand_new" || unknown.label == "" {
		t.Fatalf("unknown reason: %+v", unknown)
	}
}

func TestRunReportCountsAndMergesGroups(t *testing.T) {
	row := rustRunRow(t)
	report := runReport(row)
	if report == nil {
		t.Fatal("no report for a finished run")
	}
	totals := report["totals"].(map[string]any)
	want := map[string]int64{"indexed": 3, "skipped": 1, "not_indexed": 9, "failed": 3, "unchanged": 5, "total": 21}
	for key, value := range want {
		if totals[key] != value {
			t.Errorf("totals[%s] = %v, want %d", key, totals[key], value)
		}
	}
	if report["status"] != "partly_indexed" || report["version"] != ReportVersion {
		t.Errorf("status/version: %v %v", report["status"], report["version"])
	}
	// fetch_failed and unreadable share the SDK's read_error group.
	failed := report["categories"].([]any)[3].(map[string]any)["groups"].([]any)
	if len(failed) != 1 || failed[0].(map[string]any)["count"] != int64(3) {
		t.Errorf("failed groups: %v", failed)
	}
	if report["errors_total"] != 1 {
		t.Errorf("failed chunks should surface as an error: %v", report["errors"])
	}
}

func TestRunReportAbsentWhileRunningOrUnreported(t *testing.T) {
	if runReport(started(t, nil, 1)) != nil {
		t.Fatal("an in-progress run has no report")
	}
	done := rustRunRow(t)
	done.Dimension = 0
	if runReport(done) != nil {
		t.Fatal("a completed row that never reported has no report")
	}
	failed := rustRunRow(t)
	failed.State, failed.Error = StateFailed, ptr("boom")
	report := runReport(failed)
	if report == nil || report["status"] != "error" || report["errors_total"] != 2 {
		t.Fatalf("failed run report: %v", report)
	}
}

func TestMetadataCarriesReportAndTotalInRowAndHistory(t *testing.T) {
	row := rustRunRow(t)
	metadata := Metadata(row)
	if metadata["report"] == nil || metadata["total"] != int64(21) {
		t.Fatalf("report/total missing: %v %v", metadata["report"], metadata["total"])
	}
	if _, ok := metadata["skipped"].(map[string]any)["too_large"]; !ok {
		t.Fatal("the flat skipped map is kept")
	}
	history := metadata["history"].([]any)
	entry := history[len(history)-1].(map[string]any)
	if entry["report"] == nil || entry["total"] != int64(21) {
		t.Fatalf("the history entry lacks the report: %v", entry)
	}
}

// TestMetadataReportGolden pins the JSON against the shape of the SDK's
// build_report; the web test feeds the same file through
// normalizeIndexingReport. Regenerate with UPDATE_GOLDEN=1.
func TestMetadataReportGolden(t *testing.T) {
	report := Metadata(rustRunRow(t))["report"]
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
	var keys []string
	for k := range report.(map[string]any) {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	wantKeys := []string{"categories", "dependent_labels", "errors", "errors_total", "item_labels", "status", "totals", "version"}
	if len(keys) != len(wantKeys) {
		t.Fatalf("keys: %v", keys)
	}
	for i := range keys {
		if keys[i] != wantKeys[i] {
			t.Fatalf("keys: %v", keys)
		}
	}
}
