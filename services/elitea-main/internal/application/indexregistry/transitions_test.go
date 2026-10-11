package indexregistry

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"testing"
	"time"

	indexingapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/indexing"
	outputapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/output"
	executiondomain "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/execution"
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
	// The run's execution job is live: the repository reads this from
	// execution_jobs on every load.
	row.Job = RunJob{Found: true, State: executiondomain.JobRunning, DesiredState: "RUNNING"}
	row.ObservedAt = t0
	return row
}

// applyResult is ApplyResult plus the mismatch CheckEmbedding finds for the
// result against the row it is applied to, which the tests assert on.
func applyResult(row Row, result Result) (Row, bool, *DimensionMismatchError, error) {
	var mismatch *DimensionMismatchError
	if row.State == StateInProgress {
		mismatch = CheckEmbedding(row, result.Summary)
	}
	next, changed, err := ApplyResult(row, result)
	return next, changed, mismatch, err
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
	row, _, _, err := applyResult(row, resultFor(1, okSummary(nil)))
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
	next, changed, mismatch, err := applyResult(row, resultFor(1, okSummary(nil)))
	if err != nil || !changed || mismatch != nil {
		t.Fatalf("apply: changed=%v mismatch=%v err=%v", changed, mismatch, err)
	}
	if next.State != StateCompleted || next.Error != nil || next.TaskID == nil {
		t.Fatalf("completed row = %+v", next)
	}
	if next.Model != "Text-Embedding-3-Small" || next.Dimension != 1536 ||
		next.Collection != CollectionName("Text-Embedding-3-Small", 1536) {
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
	row, _, _, err := applyResult(row, resultFor(1, okSummary(nil)))
	if err != nil {
		t.Fatal(err)
	}
	row = started(t, &row, 2)

	next, changed, mismatch, err := applyResult(row, resultFor(2, okSummary(func(s *outputapp.IndexIngestSummary) {
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
	row, _, _, _ = applyResult(row, resultFor(1, okSummary(nil)))
	row = started(t, &row, 2)
	next, _, mismatch, err := applyResult(row, resultFor(2, okSummary(func(s *outputapp.IndexIngestSummary) {
		s.EmbeddingModel = "another-model"
	})))
	if err != nil || mismatch == nil || next.State != StateFailed || !strings.Contains(*next.Error, "another-model") {
		t.Fatalf("model change: next=%+v mismatch=%v err=%v", next, mismatch, err)
	}
}

func TestApplyResultWithoutAStampKeepsTheExistingOne(t *testing.T) {
	row := started(t, nil, 1)
	row, _, _, _ = applyResult(row, resultFor(1, okSummary(nil)))
	row = started(t, &row, 2)
	next, changed, mismatch, err := applyResult(row, resultFor(2, okSummary(func(s *outputapp.IndexIngestSummary) {
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
			next, changed, mismatch, err := applyResult(row, resultFor(1, okSummary(tc.mutate)))
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
	next, _, _, err := applyResult(row, resultFor(1, outputapp.IndexIngestSummary{
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
	if _, _, _, err := applyResult(row, resultFor(2, okSummary(nil))); !errors.Is(err, indexingapp.ErrCurrentIndexMetaConflict) {
		t.Fatalf("result for another run = %v, want conflict", err)
	}
	done, _, _, _ := applyResult(row, resultFor(1, okSummary(nil)))
	// A cancelled row is not revived by a late result.
	cancelled, _, err := ApplyTerminal(row, terminalFor(1, indexingapp.CurrentIndexMetaCancelled, ""))
	if err != nil || cancelled.State != StateCancelled || cancelled.TaskID != nil {
		t.Fatalf("cancel: %+v err=%v", cancelled, err)
	}
	late, changed, _, err := applyResult(cancelled, resultFor(1, okSummary(nil)))
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
		done, _, _, err := applyResult(next, resultFor(n, okSummary(nil)))
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
	done, _, _, _ := applyResult(row, resultFor(1, okSummary(nil)))
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

// sha12 is the first 12 hex digits of the SHA-256 of a model string.
func sha12(model string) string {
	sum := sha256.Sum256([]byte(model))
	return hex.EncodeToString(sum[:])[:12]
}

// vectorSlugPattern is elitea-vector's model-slug rule
// (services/elitea-vector/src/layout.rs, valid_slug).
var vectorSlugPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)

func TestCollectionName(t *testing.T) {
	cases := map[string]string{
		"text-embedding-3-small":  "emb_text-embedding-3-small-" + sha12("text-embedding-3-small") + "_1536",
		"Qwen/Qwen3-Embedding-4B": "emb_qwen-qwen3-embedding-4b-" + sha12("Qwen/Qwen3-Embedding-4B") + "_1536",
		"  ":                      "emb_model-" + sha12("  ") + "_1536",
		strings.Repeat("a", 80):   "emb_" + strings.Repeat("a", 50) + "-" + sha12(strings.Repeat("a", 80)) + "_1536",
		"nomic-embed-text:v1.5@sha256:deadbeef12": "emb_nomic-embed-text-v1-5-sha256-deadbeef12-" +
			sha12("nomic-embed-text:v1.5@sha256:deadbeef12") + "_1536",
	}
	for model, want := range cases {
		if got := CollectionName(model, 1536); got != want {
			t.Fatalf("CollectionName(%q) = %q, want %q", model, got, want)
		}
	}
}

// The readable part of a slug loses case, punctuation and everything past its
// length limit; the hash of the exact model string keeps two such models apart.
func TestCollectionNameIsLossless(t *testing.T) {
	prefix := strings.Repeat("embedding-model-family-", 3)[:48]
	pairs := [][2]string{
		{"Org/Model.A", "org-model-a"},
		{"Model-A", "model-a"},
		{prefix + "-version-one", prefix + "-version-two"},
	}
	for _, pair := range pairs {
		a, b := CollectionName(pair[0], 1024), CollectionName(pair[1], 1024)
		if a == b {
			t.Fatalf("%q and %q share the collection %q", pair[0], pair[1], a)
		}
	}
	if CollectionName("m", 768) == CollectionName("m", 1024) {
		t.Fatal("two dimensions of one model share a collection")
	}
	model := "Org/Model.A"
	if first, again := CollectionName(model, 1024), CollectionName(strings.Clone(model), 1024); first != again {
		t.Fatalf("the name is not deterministic: %q then %q", first, again)
	}
}

// Every name must be one elitea-vector accepts and parses back: a valid slug,
// then '_' and the dimension (Space::from_collection splits on the last '_').
func TestCollectionNameIsAnEliteaVectorSpace(t *testing.T) {
	models := []string{
		"text-embedding-3-small", "Org/Model.A", "  ", "___", "Ünïcødé-模型",
		strings.Repeat("x", 300), strings.Repeat("-", 70) + "z",
		"nomic-embed-text:v1.5@sha256:deadbeef12",
	}
	for _, model := range models {
		name := CollectionName(model, 3072)
		rest, ok := strings.CutPrefix(name, "emb_")
		if !ok {
			t.Fatalf("%q: no emb_ prefix", name)
		}
		cut := strings.LastIndexByte(rest, '_')
		slug, dimension := rest[:cut], rest[cut+1:]
		if dimension != "3072" || !vectorSlugPattern.MatchString(slug) || slug != VectorSpaceSlug(model) {
			t.Fatalf("CollectionName(%q) = %q: slug %q dimension %q is not an elitea-vector space", model, name, slug, dimension)
		}
	}
}

func TestStartRunAfterAMissingJobRecordsItAbandonedAndStartsTheNext(t *testing.T) {
	dead := started(t, nil, 1)
	dead.Job = RunJob{} // its execution job is missing
	if dead.RunStatus() != RunAbandoned || !dead.Dead() || dead.Active() || !dead.CanStartNextRun() {
		t.Fatalf("a row in progress with no job must be abandoned: %+v", dead)
	}
	before := len(dead.History)
	next := started(t, &dead, 2)
	if next.State != StateInProgress || *next.TaskID != "exec-2" || next.Error != nil || next.ExecutionID != "exec-2" {
		t.Fatalf("the new run did not start: %+v", next)
	}
	if len(next.History) != before+1 {
		t.Fatalf("history = %+v", next.History)
	}
	previous := next.History[len(next.History)-2]
	if previous["state"] != "failed" || previous["error"] != "abandoned" || previous["reason"] != "abandoned" ||
		previous["execution_id"] != "exec-1" {
		t.Fatalf("the dead run is not recorded as abandoned: %+v", previous)
	}
	if last := next.History[len(next.History)-1]; last["state"] != "in_progress" || last["execution_id"] != "exec-2" {
		t.Fatalf("the new run's entry = %+v", last)
	}
	// The input row is untouched: StartRun returns a new row.
	if dead.State != StateInProgress || dead.History[len(dead.History)-1]["state"] != "in_progress" {
		t.Fatalf("StartRun changed the row it was given: %+v", dead)
	}
}

func TestRunStatusIsDerivedFromTheJob(t *testing.T) {
	settled := t0.Add(time.Hour)
	cases := []struct {
		name   string
		job    RunJob
		now    time.Time
		status RunStatus
		active bool
	}{
		{"pending", RunJob{Found: true, State: executiondomain.JobPending, DesiredState: "RUNNING"}, settled, RunLive, true},
		{"settling job state", RunJob{Found: true, State: executiondomain.JobSettling, DesiredState: "RUNNING"}, settled, RunLive, true},
		{"running, cancel requested", RunJob{Found: true, State: executiondomain.JobRunning, DesiredState: "CANCELLED"}, settled, RunLive, true},
		{"cancelled", RunJob{Found: true, State: executiondomain.JobCancelled, DesiredState: "CANCELLED", SettledAt: settled}, settled, RunEndedCancelled, false},
		{"failed while cancelling", RunJob{Found: true, State: executiondomain.JobFailed, DesiredState: "CANCELLED", SettledAt: settled}, settled, RunEndedCancelled, false},
		{"failed", RunJob{Found: true, State: executiondomain.JobFailed, DesiredState: "RUNNING", SettledAt: settled}, settled, RunEndedFailed, false},
		{"quarantined", RunJob{Found: true, State: executiondomain.JobQuarantined, DesiredState: "RUNNING", SettledAt: settled}, settled, RunEndedFailed, false},
		{"succeeded, result pending", RunJob{Found: true, State: executiondomain.JobSucceeded, DesiredState: "RUNNING", SettledAt: settled}, settled.Add(SettlingGrace - time.Second), RunSettling, true},
		{"succeeded, grace over", RunJob{Found: true, State: executiondomain.JobSucceeded, DesiredState: "RUNNING", SettledAt: settled}, settled.Add(SettlingGrace), RunAbandoned, false},
		{"missing", RunJob{}, settled, RunAbandoned, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			row := started(t, nil, 1)
			row.Job, row.ObservedAt = tc.job, tc.now
			if got := row.RunStatus(); got != tc.status || row.Active() != tc.active || row.Dead() == tc.active {
				t.Fatalf("status = %s active=%v, want %s active=%v", got, row.Active(), tc.status, tc.active)
			}
			// A row at rest is never owned by a run, whatever its job says.
			row.State = StateCompleted
			if row.RunStatus() != RunAtRest || row.Active() || row.Dead() {
				t.Fatalf("a row at rest reads %s", row.RunStatus())
			}
		})
	}
}

// A stop is a cancelled job; the reindex right after it must record the
// stopped run as cancelled (what its queued cancel effect would have
// written), and that effect must not overwrite anything when it lands later.
func TestStopFollowedByAnImmediateReindexRecordsTheStopAsCancelled(t *testing.T) {
	stopped := started(t, nil, 1)
	stopped.Job = RunJob{Found: true, State: executiondomain.JobCancelled, DesiredState: "CANCELLED", SettledAt: t0.Add(90 * time.Second)}
	if stopped.Active() {
		t.Fatal("a cancelled job is not an active run")
	}
	next := started(t, &stopped, 2)
	previous := next.History[len(next.History)-2]
	if previous["state"] != "cancelled" || previous["task_id"] != nil || previous["error"] != nil ||
		previous["execution_id"] != "exec-1" {
		t.Fatalf("the stopped run is not recorded as cancelled: %+v", previous)
	}
	if _, has := previous["reason"]; has {
		t.Fatalf("a cancelled run has no failure reason: %+v", previous)
	}
	_, changed, err := ApplyTerminal(next, terminalFor(1, indexingapp.CurrentIndexMetaCancelled, ""))
	if !errors.Is(err, indexingapp.ErrCurrentIndexMetaSuperseded) || changed {
		t.Fatalf("the late cancel effect = changed %v err %v, want superseded", changed, err)
	}
	if next.State != StateInProgress || next.History[len(next.History)-2]["state"] != "cancelled" {
		t.Fatalf("the new run was overwritten: %+v", next)
	}
}

func TestStartRunAfterAFailedJobRecordsItFailed(t *testing.T) {
	dead := started(t, nil, 1)
	dead.Job = RunJob{Found: true, State: executiondomain.JobFailed, DesiredState: "RUNNING", SettledAt: t0.Add(2 * time.Minute)}
	next := started(t, &dead, 2)
	previous := next.History[len(next.History)-2]
	if previous["state"] != "failed" || previous["reason"] != executionFailedReason || previous["error"] == nil {
		t.Fatalf("the failed run is not recorded as failed: %+v", previous)
	}
}

func TestASettlingRunIsActiveUntilTheGraceEnds(t *testing.T) {
	row := started(t, nil, 1)
	settled := t0.Add(5 * time.Minute)
	row.Job = RunJob{Found: true, State: executiondomain.JobSucceeded, DesiredState: "RUNNING", SettledAt: settled}
	row.ObservedAt = settled.Add(time.Minute)
	if _, _, err := StartRun(&row, runFor(2)); !errors.Is(err, indexingapp.ErrCurrentIndexMetaConflict) {
		t.Fatalf("a run whose success is still being applied = %v, want conflict", err)
	}
	if got := row.Effective(); got.State != StateInProgress {
		t.Fatalf("a settling run reads %q", got.State)
	}
	row.ObservedAt = settled.Add(SettlingGrace + time.Second)
	next := started(t, &row, 2)
	if previous := next.History[len(next.History)-2]; previous["state"] != "failed" || previous["reason"] != abandonedReason {
		t.Fatalf("a success never applied in time = %+v, want abandoned", previous)
	}
}

func TestEffectiveReadsADeadRunAsItsOutcome(t *testing.T) {
	row := started(t, nil, 1)
	if got := row.Effective(); got.State != StateInProgress {
		t.Fatalf("a live run reads %q", got.State)
	}
	row.Job = RunJob{Found: true, State: executiondomain.JobCancelled, DesiredState: "CANCELLED", SettledAt: t0.Add(time.Hour)}
	effective := row.Effective()
	if effective.State != StateCancelled || effective.TaskID != nil || Metadata(effective)["state"] != "cancelled" {
		t.Fatalf("a cancelled job's row reads %+v", effective)
	}
	if row.State != StateInProgress {
		t.Fatal("Effective changed the row it was given")
	}
}

func TestStartRunRefusesALiveRunHoweverOldItsUpdatedAt(t *testing.T) {
	live := started(t, nil, 1)
	live.UpdatedAt = t0.Add(-72 * time.Hour)
	if !live.Active() || live.Dead() {
		t.Fatalf("a row in progress with a live job is active: %+v", live)
	}
	if _, _, err := StartRun(&live, runFor(2)); !errors.Is(err, indexingapp.ErrCurrentIndexMetaConflict) {
		t.Fatalf("a live run, old updated_at = %v, want conflict", err)
	}
}

func TestTransitionsDoNotChangeTheConfigurationOrHistoryOfTheRowTheyAreGiven(t *testing.T) {
	row := started(t, nil, 1)
	chunking := func(r Row) bool { _, ok := r.IndexConf["chunking_config"]; return ok }
	if !chunking(row) {
		t.Fatal("the stored configuration lost its chunking_config")
	}
	entries := len(row.History)
	next, _, _, err := applyResult(row, resultFor(1, okSummary(nil)))
	if err != nil {
		t.Fatal(err)
	}
	if !chunking(row) || !chunking(next) || len(row.History) != entries || row.State != StateInProgress {
		t.Fatalf("a transition mutated its input: %+v", row)
	}
	// History entries drop the chunking configuration, the row keeps it.
	last := next.History[len(next.History)-1]
	if conf, _ := last["index_configuration"].(map[string]any); conf == nil || conf["chunking_config"] != nil {
		t.Fatalf("history entry configuration = %v", last["index_configuration"])
	}
	// Reading allocates no copy of the configuration: it is the row's own map.
	metadata := Metadata(next)
	if conf, _ := metadata["index_configuration"].(map[string]any); conf == nil || conf["chunking_config"] == nil {
		t.Fatalf("metadata configuration = %v", metadata["index_configuration"])
	}
}

// A first run that failed has said nothing about the embedding space of the
// vectors it may or may not have written: it must not lock the index to its
// model.
func TestAFailedFirstRunDoesNotStampAndALaterRunWithAnotherModelSucceeds(t *testing.T) {
	row := started(t, nil, 1)
	failed, _, mismatch, err := applyResult(row, resultFor(1, okSummary(func(s *outputapp.IndexIngestSummary) {
		s.Status = outputapp.IndexIngestStatusError
		s.TerminalState = outputapp.IndexIngestTerminalFailed
		s.Message = "The source refused the credential."
	})))
	if err != nil || mismatch != nil || failed.State != StateFailed {
		t.Fatalf("failed run: %+v mismatch=%v err=%v", failed, mismatch, err)
	}
	if failed.Stamped() || failed.Model != "" || failed.Collection != "" {
		t.Fatalf("a failed run stamped the index: model %q dimension %d collection %q", failed.Model, failed.Dimension, failed.Collection)
	}
	if _, present := Metadata(failed)["embedding_model"]; present {
		t.Fatal("a failed run's metadata carries an embedding stamp")
	}

	second := started(t, &failed, 2)
	done, _, mismatch, err := applyResult(second, resultFor(2, okSummary(func(s *outputapp.IndexIngestSummary) {
		s.EmbeddingModel, s.EmbeddingDimension = "another-model", 768
	})))
	if err != nil || mismatch != nil || done.State != StateCompleted {
		t.Fatalf("later run with another model: %+v mismatch=%v err=%v", done, mismatch, err)
	}
	if done.Model != "another-model" || done.Dimension != 768 || done.Collection != CollectionName("another-model", 768) {
		t.Fatalf("the later run did not stamp: %q %d %q", done.Model, done.Dimension, done.Collection)
	}
}

func TestOnlyASuccessWithChunksStampsOrIsChecked(t *testing.T) {
	cases := map[string]func(*outputapp.IndexIngestSummary){
		"no chunks written": func(s *outputapp.IndexIngestSummary) { s.IndexedChunks = 0 },
		"error status": func(s *outputapp.IndexIngestSummary) {
			s.Status = outputapp.IndexIngestStatusError
			s.TerminalState = outputapp.IndexIngestTerminalFailed
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			next, _, mismatch, err := applyResult(started(t, nil, 1), resultFor(1, okSummary(mutate)))
			if err != nil || mismatch != nil || next.Stamped() {
				t.Fatalf("stamped=%v mismatch=%v err=%v", next.Stamped(), mismatch, err)
			}
			// ... and such a result never fails against an existing stamp either.
			stamped, _, _, _ := applyResult(started(t, nil, 1), resultFor(1, okSummary(nil)))
			stamped.State = StateInProgress
			if got := CheckEmbedding(stamped, okSummary(func(s *outputapp.IndexIngestSummary) {
				mutate(s)
				s.EmbeddingModel = "other"
			})); got != nil {
				t.Fatalf("a result that stamps nothing was checked: %v", got)
			}
		})
	}
	// A partly indexed result with chunks stamps.
	next, _, _, _ := applyResult(started(t, nil, 1), resultFor(1, okSummary(func(s *outputapp.IndexIngestSummary) {
		s.Status = outputapp.IndexIngestStatusPartlyIndexed
		s.TerminalState = outputapp.IndexIngestTerminalPartlyIndexed
	})))
	if !next.Stamped() {
		t.Fatalf("a partly indexed result with chunks did not stamp: %+v", next)
	}
}

func TestCheckEmbeddingNamesTheMismatchAndItsFailureSummary(t *testing.T) {
	stamped, _, _, _ := applyResult(started(t, nil, 1), resultFor(1, okSummary(nil)))
	row := started(t, &stamped, 2)
	if CheckEmbedding(row, okSummary(nil)) != nil {
		t.Fatal("the same embedding space is not a mismatch")
	}
	mismatch := CheckEmbedding(row, okSummary(func(s *outputapp.IndexIngestSummary) { s.EmbeddingDimension = 768 }))
	if mismatch == nil {
		t.Fatal("another dimension is a mismatch")
	}
	summary := mismatch.FailedSummary()
	if summary.Status != outputapp.IndexIngestStatusError || summary.TerminalState != outputapp.IndexIngestTerminalFailed ||
		summary.Message != mismatch.Error() || summary.HasTypedResult() {
		t.Fatalf("failure summary = %+v", summary)
	}
	if err := summary.Validate(); err != nil {
		t.Fatalf("the failure summary is not a valid summary: %v", err)
	}
	// Applying the failure summary instead of the result gives the same row
	// state as applying the mismatching result does.
	viaSummary, _, _, err := applyResult(row, resultFor(2, summary))
	viaResult, _, _, _ := applyResult(row, resultFor(2, okSummary(func(s *outputapp.IndexIngestSummary) { s.EmbeddingDimension = 768 })))
	if err != nil || viaSummary.State != StateFailed || viaSummary.State != viaResult.State ||
		*viaSummary.Error != *viaResult.Error || viaSummary.Dimension != 1536 {
		t.Fatalf("via summary %+v, via result %+v", viaSummary, viaResult)
	}
}

// The list's `indexed` and `indexed_chunks` are the index's totals (its
// recorded documents); the run's own counts stay in its history entry. An
// incremental run that changed nothing reports zero for itself and leaves
// the totals as they were.
func TestMetadataReportsIndexTotalsAndHistoryKeepsRunCounts(t *testing.T) {
	first, _, _, err := applyResult(started(t, nil, 1), resultFor(1, okSummary(nil)))
	if err != nil {
		t.Fatal(err)
	}
	second, _, _, err := applyResult(started(t, &first, 2), resultFor(2, okSummary(func(s *outputapp.IndexIngestSummary) {
		s.IndexedDocuments, s.IndexedChunks, s.FailedChunks, s.Updated = 0, 0, 0, 0
		s.SkippedJSON = `{"unchanged":3}`
		s.Message = "Nothing changed."
	})))
	if err != nil || second.State != StateCompleted {
		t.Fatalf("no-change run: %+v err=%v", second, err)
	}
	// What index_registry_documents holds after both runs.
	second.TotalDocuments, second.TotalChunks = 3, 9
	metadata := Metadata(second)
	if metadata["indexed"] != int64(3) || metadata["indexed_chunks"] != int64(9) {
		t.Fatalf("totals = indexed %v chunks %v, want 3 and 9", metadata["indexed"], metadata["indexed_chunks"])
	}
	if metadata["updated"] != int64(0) || metadata["failed_chunks"] != int64(0) ||
		metadata["skipped"].(map[string]any)["unchanged"] != uint64(3) {
		t.Fatalf("last run = updated %v failed %v skipped %v", metadata["updated"], metadata["failed_chunks"], metadata["skipped"])
	}
	history := metadata["history"].([]any)
	firstRun, secondRun := history[1].(map[string]any), history[2].(map[string]any)
	if firstRun["indexed"] != int64(3) || firstRun["indexed_chunks"] != int64(9) {
		t.Fatalf("the first run's entry = %+v", firstRun)
	}
	if secondRun["indexed"] != int64(0) || secondRun["indexed_chunks"] != int64(0) {
		t.Fatalf("the no-change run's entry = %+v", secondRun)
	}
}

func TestARustResultWithoutATypedSummaryIsRecordedFailed(t *testing.T) {
	row := started(t, nil, 1)
	recorded, overridden := RecordedSummary(row, outputapp.IndexIngestSummary{})
	if !overridden || recorded.Status != outputapp.IndexIngestStatusError ||
		recorded.Message != NoTypedSummaryReason || recorded.Validate() != nil {
		t.Fatalf("recorded = %+v overridden=%v", recorded, overridden)
	}
	next, changed, err := ApplyResult(row, resultFor(1, outputapp.IndexIngestSummary{}))
	if err != nil || !changed || next.State != StateFailed || next.Error == nil || *next.Error != NoTypedSummaryReason ||
		next.Stamped() {
		t.Fatalf("no summary: %+v changed=%v err=%v", next, changed, err)
	}
	if summary, overridden := RecordedSummary(row, okSummary(nil)); overridden || summary != okSummary(nil) {
		t.Fatalf("a typed summary was overridden: %+v", summary)
	}
}
