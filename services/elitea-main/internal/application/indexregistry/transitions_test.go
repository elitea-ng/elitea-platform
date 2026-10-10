package indexregistry

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	indexingapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/indexing"
	outputapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/output"
)

var t0 = time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)

func runFor(n int) indexingapp.RegistryInitialRun {
	return indexingapp.RegistryInitialRun{
		ProjectID:       7,
		ToolkitID:       11,
		IndexName:       "docs",
		MetaID:          fmt.Sprintf("meta-%d", n),
		ExecutionID:     fmt.Sprintf("exec-%d", n),
		CorrelationID:   fmt.Sprintf("corr-%d", n),
		Generation:      uint64(n),
		IndexGeneration: uint64(n),
		Configuration:   []byte(`{"index_name":"docs","progress_step":10,"chunking_config":{"markdown":{"max_tokens":512}}}`),
		AdmittedAt:      t0.Add(time.Duration(n) * time.Minute),
	}
}

func started(t *testing.T, existing *Row, n int) Row {
	t.Helper()
	row, changed, err := StartRun(existing, runFor(n))
	if err != nil || !changed {
		t.Fatalf("start run %d: changed=%v err=%v", n, changed, err)
	}
	if existing != nil {
		row.IndexID = existing.IndexID
	} else {
		row.IndexID = "11111111-1111-1111-1111-111111111111"
	}
	return row
}

func okSummary(mutate func(*outputapp.IndexIngestSummary)) outputapp.IndexIngestSummary {
	summary := outputapp.IndexIngestSummary{
		Status:             outputapp.IndexIngestStatusOK,
		Message:            "Indexed 3 documents.",
		TerminalState:      outputapp.IndexIngestTerminalCompleted,
		IndexedDocuments:   3,
		IndexedChunks:      9,
		FailedChunks:       1,
		SkippedJSON:        `{"too_large":2}`,
		EmbeddingModel:     "Text-Embedding-3-Small",
		EmbeddingDimension: 1536,
	}
	if mutate != nil {
		mutate(&summary)
	}
	return summary
}

func resultFor(n int, summary outputapp.IndexIngestSummary) Result {
	return Result{
		ExecutionID: fmt.Sprintf("exec-%d", n),
		Generation:  uint64(n),
		OccurredAt:  t0.Add(time.Duration(n)*time.Minute + 30*time.Second),
		Summary:     summary,
	}
}

func TestStartRunCreatesTheRowWithCreatedMarkerAndRunEntry(t *testing.T) {
	row := started(t, nil, 1)
	if row.State != StateInProgress || row.TaskID == nil || *row.TaskID != "exec-1" ||
		row.ExecutionID != "exec-1" || row.ExecutionGeneration != 1 || row.IndexGeneration != 1 ||
		row.MetaID != "meta-1" || row.Name != "docs" || row.Stamped() {
		t.Fatalf("new run row = %+v", row)
	}
	if len(row.History) != 2 || row.History[0]["state"] != "created" || row.History[1]["state"] != "in_progress" {
		t.Fatalf("history = %+v", row.History)
	}
	marker := row.History[0]
	for _, key := range []string{"execution_id", "execution_generation", "index_generation", "index_meta_id", "correlation_id"} {
		if _, present := marker[key]; present {
			t.Fatalf("created marker carries run key %q", key)
		}
	}
	if marker["task_id"] != nil || marker["conversation_id"] != nil {
		t.Fatalf("created marker is not run-neutral: %+v", marker)
	}
	// index_meta repeats the configuration once per run; its chunking_config is
	// what made the list response grow (issue #297), so history drops it while
	// the top level keeps it for the edit form and the reindex.
	for _, entry := range row.History {
		if configuration, _ := entry["index_configuration"].(map[string]any); configuration != nil {
			if _, present := configuration["chunking_config"]; present {
				t.Fatalf("history entry kept chunking_config: %+v", entry)
			}
		}
	}
	if _, present := row.IndexConf["chunking_config"]; !present {
		t.Fatal("the top-level configuration lost chunking_config")
	}
}

func TestStartRunRetryIsIdempotentAndFenced(t *testing.T) {
	row := started(t, nil, 1)
	again, changed, err := StartRun(&row, runFor(1))
	if err != nil || changed {
		t.Fatalf("retry of the same admission: changed=%v err=%v", changed, err)
	}
	if len(again.History) != len(row.History) {
		t.Fatal("a retry appended a history entry")
	}
	// The same meta id naming another execution is not a retry.
	other := runFor(1)
	other.ExecutionID = "exec-other"
	if _, _, err := StartRun(&row, other); !errors.Is(err, indexingapp.ErrCurrentIndexMetaConflict) {
		t.Fatalf("a retry naming another execution = %v, want conflict", err)
	}
}

func TestStartRunRefusesAnActiveRunAndSupersededGenerations(t *testing.T) {
	first := started(t, nil, 2)
	if _, _, err := StartRun(&first, runFor(3)); !errors.Is(err, indexingapp.ErrCurrentIndexMetaConflict) {
		t.Fatalf("starting over an in-progress run = %v, want conflict", err)
	}
	// A late initializer for an older generation than the row holds.
	if _, _, err := StartRun(&first, runFor(1)); !errors.Is(err, indexingapp.ErrCurrentIndexMetaSuperseded) {
		t.Fatalf("starting an older generation = %v, want superseded", err)
	}
	// The same index generation under another meta id is two admissions fighting
	// for one slot.
	clash := runFor(2)
	clash.MetaID = "meta-clash"
	if _, _, err := StartRun(&first, clash); !errors.Is(err, indexingapp.ErrCurrentIndexMetaConflict) {
		t.Fatalf("same generation, other meta = %v, want conflict", err)
	}
}

func TestStartRunNextRunKeepsTheStampAndResetsCounts(t *testing.T) {
	row := started(t, nil, 1)
	row, _, _, err := ApplyResult(row, resultFor(1, okSummary(nil)))
	if err != nil {
		t.Fatal(err)
	}
	if row.State != StateCompleted || !row.Stamped() {
		t.Fatalf("after the first result: %+v", row)
	}
	next := started(t, &row, 2)
	if next.State != StateInProgress || *next.TaskID != "exec-2" || next.Error != nil {
		t.Fatalf("second run row = %+v", next)
	}
	if next.Indexed != 0 || next.Chunks != 0 || next.Failed != 0 || len(next.Skipped) != 0 {
		t.Fatalf("counts of the run in flight were not reset: %+v", next)
	}
	if !next.Stamped() || next.Dimension != 1536 || next.Collection != row.Collection {
		t.Fatalf("the embedding stamp was lost between runs: %+v", next)
	}
	if len(next.History) != 3 || next.History[2]["state"] != "in_progress" || next.History[1]["state"] != "completed" {
		t.Fatalf("history = %+v", next.History)
	}
}

func TestApplyResultStampsModelDimensionAndCollectionOnTheFirstResult(t *testing.T) {
	row := started(t, nil, 1)
	next, changed, mismatch, err := ApplyResult(row, resultFor(1, okSummary(nil)))
	if err != nil || !changed || mismatch != nil {
		t.Fatalf("apply: changed=%v mismatch=%v err=%v", changed, mismatch, err)
	}
	if next.State != StateCompleted || next.Error != nil || next.TaskID == nil {
		t.Fatalf("completed row = %+v", next)
	}
	if next.Model != "Text-Embedding-3-Small" || next.Dimension != 1536 ||
		next.Collection != "emb_text_embedding_3_small_1536" {
		t.Fatalf("stamp = model %q dimension %d collection %q", next.Model, next.Dimension, next.Collection)
	}
	if next.Indexed != 3 || next.Chunks != 9 || next.Failed != 1 || next.Skipped["too_large"] != 2 {
		t.Fatalf("typed counts were not applied: %+v", next)
	}
	last := next.History[len(next.History)-1]
	if last["state"] != "completed" || last["created_on"] != row.History[1]["created_on"] {
		t.Fatalf("the run's history entry was not rewritten in place: %+v", last)
	}
}

func TestApplyResultFailsALaterRunWithAnotherDimension(t *testing.T) {
	row := started(t, nil, 1)
	row, _, _, err := ApplyResult(row, resultFor(1, okSummary(nil)))
	if err != nil {
		t.Fatal(err)
	}
	row = started(t, &row, 2)

	next, changed, mismatch, err := ApplyResult(row, resultFor(2, okSummary(func(s *outputapp.IndexIngestSummary) {
		s.EmbeddingDimension = 768
		s.IndexedDocuments = 99
	})))
	if err != nil || !changed || mismatch == nil {
		t.Fatalf("dimension change: changed=%v mismatch=%v err=%v", changed, mismatch, err)
	}
	if next.State != StateFailed || next.Error == nil {
		t.Fatalf("a different dimension did not fail the run: %+v", next)
	}
	for _, want := range []string{"1536", "768", "dimension", "new index"} {
		if !strings.Contains(*next.Error, want) {
			t.Fatalf("failure message %q does not say %q", *next.Error, want)
		}
	}
	// The stamp and the counts are the index's, not the rejected result's.
	if next.Dimension != 1536 || next.Model != "Text-Embedding-3-Small" || next.Indexed == 99 {
		t.Fatalf("a rejected result changed the index: %+v", next)
	}
	if last := next.History[len(next.History)-1]; last["state"] != "failed" || last["error"] != *next.Error {
		t.Fatalf("history does not record the failure: %+v", last)
	}
}

func TestApplyResultFailsAnotherModelOfTheSameDimension(t *testing.T) {
	row := started(t, nil, 1)
	row, _, _, _ = ApplyResult(row, resultFor(1, okSummary(nil)))
	row = started(t, &row, 2)
	next, _, mismatch, err := ApplyResult(row, resultFor(2, okSummary(func(s *outputapp.IndexIngestSummary) {
		s.EmbeddingModel = "another-model"
	})))
	if err != nil || mismatch == nil || next.State != StateFailed || !strings.Contains(*next.Error, "another-model") {
		t.Fatalf("model change: next=%+v mismatch=%v err=%v", next, mismatch, err)
	}
}

func TestApplyResultWithoutAStampKeepsTheExistingOne(t *testing.T) {
	row := started(t, nil, 1)
	row, _, _, _ = ApplyResult(row, resultFor(1, okSummary(nil)))
	row = started(t, &row, 2)
	next, changed, mismatch, err := ApplyResult(row, resultFor(2, okSummary(func(s *outputapp.IndexIngestSummary) {
		s.EmbeddingModel, s.EmbeddingDimension = "", 0
		s.IndexedDocuments = 0
		s.SkippedJSON = ""
	})))
	if err != nil || !changed || mismatch != nil || next.State != StateCompleted || next.Dimension != 1536 {
		t.Fatalf("unchanged run: next=%+v mismatch=%v err=%v", next, mismatch, err)
	}
}

func TestApplyResultMapsStatusesToStates(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*outputapp.IndexIngestSummary)
		state  State
		errMsg string
	}{
		{"created", func(s *outputapp.IndexIngestSummary) { s.TerminalState = outputapp.IndexIngestTerminalCreated }, StateCompleted, ""},
		{"scheduled reindex", func(s *outputapp.IndexIngestSummary) { s.TerminalState = outputapp.IndexIngestTerminalScheduledReindex }, StateCompleted, ""},
		{"partly indexed", func(s *outputapp.IndexIngestSummary) {
			s.Status = outputapp.IndexIngestStatusPartlyIndexed
			s.TerminalState = outputapp.IndexIngestTerminalPartlyIndexed
		}, StatePartlyIndexed, ""},
		{"error", func(s *outputapp.IndexIngestSummary) {
			s.Status = outputapp.IndexIngestStatusError
			s.TerminalState = outputapp.IndexIngestTerminalFailed
			s.Message = "The source refused the credential."
		}, StateFailed, "The source refused the credential."},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			row := started(t, nil, 1)
			next, changed, mismatch, err := ApplyResult(row, resultFor(1, okSummary(tc.mutate)))
			if err != nil || !changed || mismatch != nil || next.State != tc.state {
				t.Fatalf("state = %q, want %q (changed=%v mismatch=%v err=%v)", next.State, tc.state, changed, mismatch, err)
			}
			if tc.errMsg == "" && next.Error != nil {
				t.Fatalf("unexpected error %q", *next.Error)
			}
			if tc.errMsg != "" && (next.Error == nil || *next.Error != tc.errMsg) {
				t.Fatalf("error = %v, want %q", next.Error, tc.errMsg)
			}
		})
	}
}

func TestApplyResultLegacySummaryUsesIndexedAndUpdated(t *testing.T) {
	row := started(t, nil, 1)
	next, _, _, err := ApplyResult(row, resultFor(1, outputapp.IndexIngestSummary{
		Status: outputapp.IndexIngestStatusOK, Message: "ok",
		TerminalState: outputapp.IndexIngestTerminalCompleted, Indexed: 17, Updated: 2,
	}))
	if err != nil || next.Indexed != 17 || next.Updated != 2 || next.Stamped() {
		t.Fatalf("legacy summary: %+v err=%v", next, err)
	}
}

func TestApplyResultIsFencedAndFirstTerminalWins(t *testing.T) {
	row := started(t, nil, 1)
	// A result for another execution names no run of this row.
	if _, _, _, err := ApplyResult(row, resultFor(2, okSummary(nil))); !errors.Is(err, indexingapp.ErrCurrentIndexMetaConflict) {
		t.Fatalf("result for another run = %v, want conflict", err)
	}
	done, _, _, _ := ApplyResult(row, resultFor(1, okSummary(nil)))
	// A cancelled row is not revived by a late result.
	cancelled, _, err := ApplyTerminal(row, terminalFor(1, indexingapp.CurrentIndexMetaCancelled, ""))
	if err != nil || cancelled.State != StateCancelled || cancelled.TaskID != nil {
		t.Fatalf("cancel: %+v err=%v", cancelled, err)
	}
	late, changed, _, err := ApplyResult(cancelled, resultFor(1, okSummary(nil)))
	if err != nil || changed || late.State != StateCancelled {
		t.Fatalf("a late result changed a cancelled run: %+v changed=%v err=%v", late, changed, err)
	}
	// And a terminal effect after the result is a no-op.
	after, changed, err := ApplyTerminal(done, terminalFor(1, indexingapp.CurrentIndexMetaFailed, "worker lost"))
	if err != nil || changed || after.State != StateCompleted {
		t.Fatalf("terminal after result: %+v changed=%v err=%v", after, changed, err)
	}
}

func terminalFor(n int, state indexingapp.CurrentIndexMetaTerminalState, safeError string) indexingapp.RegistryTerminal {
	return indexingapp.RegistryTerminal{
		ProjectID: 7,
		CurrentTerminalIndexMeta: indexingapp.CurrentTerminalIndexMeta{
			MetaID: fmt.Sprintf("meta-%d", n), ExecutionID: fmt.Sprintf("exec-%d", n),
			Generation: uint64(n), IndexGeneration: uint64(n), IndexName: "docs", ToolkitID: 11,
			State: state, OccurredAt: t0.Add(time.Duration(n)*time.Minute + time.Minute), SafeError: safeError,
		},
	}
}

func TestApplyTerminalFailedAndCancelled(t *testing.T) {
	row := started(t, nil, 1)
	failed, changed, err := ApplyTerminal(row, terminalFor(1, indexingapp.CurrentIndexMetaFailed, "worker lost"))
	if err != nil || !changed || failed.State != StateFailed || failed.Error == nil || *failed.Error != "worker lost" || failed.TaskID == nil {
		t.Fatalf("failed: %+v changed=%v err=%v", failed, changed, err)
	}
	cancelled, changed, err := ApplyTerminal(row, terminalFor(1, indexingapp.CurrentIndexMetaCancelled, ""))
	if err != nil || !changed || cancelled.State != StateCancelled || cancelled.TaskID != nil || cancelled.Error != nil {
		t.Fatalf("cancelled: %+v changed=%v err=%v", cancelled, changed, err)
	}
	if last := cancelled.History[len(cancelled.History)-1]; last["state"] != "cancelled" || last["task_id"] != nil {
		t.Fatalf("history entry = %+v", last)
	}
	again, changed, err := ApplyTerminal(cancelled, terminalFor(1, indexingapp.CurrentIndexMetaCancelled, ""))
	if err != nil || changed || again.State != StateCancelled {
		t.Fatalf("repeat: changed=%v err=%v", changed, err)
	}
}

func TestApplyTerminalFences(t *testing.T) {
	row := started(t, nil, 2)
	if _, _, err := ApplyTerminal(row, terminalFor(1, indexingapp.CurrentIndexMetaFailed, "x")); !errors.Is(err, indexingapp.ErrCurrentIndexMetaSuperseded) {
		t.Fatalf("terminal of an older run = %v, want superseded", err)
	}
	wrongMeta := terminalFor(2, indexingapp.CurrentIndexMetaFailed, "x")
	wrongMeta.MetaID = "meta-other"
	if _, _, err := ApplyTerminal(row, wrongMeta); !errors.Is(err, indexingapp.ErrCurrentIndexMetaConflict) {
		t.Fatalf("terminal with another meta id = %v, want conflict", err)
	}
	if _, _, err := ApplyTerminal(row, terminalFor(3, indexingapp.CurrentIndexMetaFailed, "x")); !errors.Is(err, indexingapp.ErrCurrentIndexMetaConflict) {
		t.Fatalf("terminal of a run the row never held = %v, want conflict", err)
	}
}

func TestVerifyManualStop(t *testing.T) {
	row := started(t, nil, 1)
	stop := indexingapp.RegistryManualStop{
		ProjectID: 7,
		CurrentManualStopCleanup: indexingapp.CurrentManualStopCleanup{
			MetaID: "meta-1", ExecutionID: "exec-1", Generation: 1, IndexGeneration: 1, IndexName: "docs", ToolkitID: 11,
		},
	}
	if err := VerifyManualStop(row, stop); !errors.Is(err, indexingapp.ErrCurrentIndexMetaConflict) {
		t.Fatalf("manual stop of a row still in progress = %v, want conflict", err)
	}
	cancelled, _, _ := ApplyTerminal(row, terminalFor(1, indexingapp.CurrentIndexMetaCancelled, ""))
	if err := VerifyManualStop(cancelled, stop); err != nil {
		t.Fatalf("manual stop of the cancelled run = %v", err)
	}
	next := started(t, &cancelled, 2)
	if err := VerifyManualStop(next, stop); !errors.Is(err, indexingapp.ErrCurrentIndexMetaSuperseded) {
		t.Fatalf("manual stop after a newer run = %v, want superseded", err)
	}
}

func TestHistoryIsCappedAtTheSameBoundAsIndexMeta(t *testing.T) {
	var row *Row
	for n := 1; n <= MaxHistoryEntries+25; n++ {
		next := started(t, row, n)
		done, _, _, err := ApplyResult(next, resultFor(n, okSummary(nil)))
		if err != nil {
			t.Fatal(err)
		}
		row = &done
	}
	if MaxHistoryEntries != 200 {
		t.Fatalf("the cap is %d; index_meta_writer.go keeps 200", MaxHistoryEntries)
	}
	if len(row.History) != MaxHistoryEntries {
		t.Fatalf("history has %d entries, want %d", len(row.History), MaxHistoryEntries)
	}
	// The oldest go first, so the newest entry is the last run's.
	last := row.History[len(row.History)-1]
	if last["execution_id"] != fmt.Sprintf("exec-%d", MaxHistoryEntries+25) {
		t.Fatalf("the newest run was dropped: %+v", last)
	}
	if encoded, _ := json.Marshal(row.History); len(encoded) > MaxHistoryBytes {
		t.Fatalf("history is %d bytes, over the %d byte budget", len(encoded), MaxHistoryBytes)
	}
}

func TestHistoryByteBudgetDropsOldestEntries(t *testing.T) {
	big := runFor(1)
	big.Configuration = []byte(`{"index_name":"docs","note":"` + strings.Repeat("x", 40_000) + `"}`)
	row, _, err := StartRun(nil, big)
	if err != nil {
		t.Fatal(err)
	}
	for n := 2; n <= 12; n++ {
		run := runFor(n)
		run.Configuration = big.Configuration
		row, _, _ = completeRun(row, n-1)
		row, _, err = StartRun(&row, run)
		if err != nil {
			t.Fatal(err)
		}
	}
	if encoded, _ := json.Marshal(row.History); len(encoded) > MaxHistoryBytes || len(row.History) < 1 {
		t.Fatalf("history is %d bytes in %d entries, budget %d", len(encoded), len(row.History), MaxHistoryBytes)
	}
}

// completeRun completes the run n so the next one may start.
func completeRun(row Row, n int) (Row, bool, error) {
	return ApplyTerminal(row, terminalFor(n, indexingapp.CurrentIndexMetaFailed, "done"))
}

func TestRecordScheduledFailure(t *testing.T) {
	row := started(t, nil, 1)
	if _, changed := RecordScheduledFailure(row, "effect-1", "cron failed", t0); changed {
		t.Fatal("a schedule failure overwrote a run in progress")
	}
	done, _, _, _ := ApplyResult(row, resultFor(1, okSummary(nil)))
	failed, changed := RecordScheduledFailure(done, "effect-1", "cron failed", t0.Add(time.Hour))
	if !changed || failed.State != StateFailed || failed.Error == nil || *failed.Error != "cron failed" {
		t.Fatalf("scheduled failure: %+v changed=%v", failed, changed)
	}
	if last := failed.History[len(failed.History)-1]; last["schedule_effect_id"] != "effect-1" {
		t.Fatalf("history = %+v", last)
	}
	if _, changed := RecordScheduledFailure(failed, "effect-1", "cron failed", t0.Add(2*time.Hour)); changed {
		t.Fatal("the same effect was recorded twice")
	}
}

func TestCollectionName(t *testing.T) {
	cases := map[string]string{
		"text-embedding-3-small":  "emb_text_embedding_3_small_1536",
		"Qwen/Qwen3-Embedding-4B": "emb_qwen_qwen3_embedding_4b_1536",
		"  ":                      "emb_model_1536",
		strings.Repeat("a", 80):   "emb_" + strings.Repeat("a", 48) + "_1536",
		"nomic-embed-text:v1.5@sha256:deadbeef12": "emb_nomic_embed_text_v1_5_sha256_deadbeef12_1536",
	}
	for model, want := range cases {
		if got := CollectionName(model, 1536); got != want {
			t.Fatalf("CollectionName(%q) = %q, want %q", model, got, want)
		}
	}
}
