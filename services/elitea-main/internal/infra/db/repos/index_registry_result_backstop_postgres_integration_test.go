package repos

// The registry's refusal and per-document application in the index output
// projection against a real PostgreSQL. Requires ELITEA_TEST_DATABASE_URL; it
// skips without it.

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	executionapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/execution"
	indexingapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/indexing"
	indexregistryapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/indexregistry"
	outputapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/output"
	"github.com/jackc/pgx/v5/pgxpool"
)

// registryProjectionHarness is the output projection with the registry on, as
// the composition root builds it in rust mode.
type registryProjectionHarness struct {
	t        *testing.T
	pool     *pgxpool.Pool
	registry *IndexRegistryRepository
	results  *IndexIngestResultsRepository
	service  *outputapp.IndexIngestService
}

func newRegistryProjectionHarness(t *testing.T) registryProjectionHarness {
	t.Helper()
	pool := newMigratedPostgresIntegrationPool(t)
	registry, err := NewIndexRegistryRepository(pool)
	if err != nil {
		t.Fatal(err)
	}
	results, err := NewIndexIngestResultsRepository(pool, IndexIngestOutputPolicy{
		LimitsRevision: "index-limits-v1", ArtifactMediaType: "application/json", MaxArtifactBytes: 1024 * 1024,
	})
	if err != nil {
		t.Fatal(err)
	}
	results.WithIndexRegistry()
	return registryProjectionHarness{
		t: t, pool: pool, registry: registry, results: results,
		service: newPostgresIndexOutputService(t, pool, results),
	}
}

// frame admits a run of the index `name` and builds its inline result frame;
// mutate adjusts the domain result (document lists) before it is projected.
func (h registryProjectionHarness) frame(
	name, key string, indexGeneration uint64,
	summary outputapp.IndexIngestSummary,
	mutate func(*outputapp.IndexIngestResult),
) outputapp.IndexIngestFrame {
	h.t.Helper()
	execution, err := admitRegistryRun(h.t, h.pool, h.registry, name, key, indexGeneration)
	if err != nil {
		h.t.Fatalf("admit %s: %v", key, err)
	}
	expected, err := h.results.ExpectedIndexIngest(context.Background(), execution, 1)
	if err != nil {
		h.t.Fatal(err)
	}
	fence := claimPostgresIndexExecution(h.t, h.pool, expected)
	frame := postgresInlineIndexOutputFrame(h.t, expected, fence, summary)
	if mutate != nil {
		mutate(&frame.Result)
	}
	if err := frame.Validate(); err != nil {
		h.t.Fatalf("frame %s: %v", key, err)
	}
	return frame
}

func (h registryProjectionHarness) project(frame outputapp.IndexIngestFrame) outputapp.ProjectionOutcome {
	h.t.Helper()
	outcome, err := h.service.IngestIndex(context.Background(), frame)
	if err != nil || !outcome.Inserted {
		h.t.Fatalf("project %s: %+v err=%v", frame.Fence.ExecutionID, outcome, err)
	}
	return outcome
}

func (h registryProjectionHarness) notifications(frame outputapp.IndexIngestFrame) int {
	h.t.Helper()
	var count int
	if err := h.pool.QueryRow(context.Background(), `SELECT count(*) FROM centry.notifications WHERE uuid = $1::text::uuid`,
		currentIndexTerminalNotificationUUID(frame.LogicalOutputID)).Scan(&count); err != nil {
		h.t.Fatalf("count notifications: %v", err)
	}
	return count
}

func (h registryProjectionHarness) outputRows(executionID string) (results, inbox, replay int) {
	h.t.Helper()
	ctx := context.Background()
	if err := h.pool.QueryRow(ctx, `SELECT count(*) FROM elitea_runtime.index_ingest_results WHERE execution_id = $1`, executionID).Scan(&results); err != nil {
		h.t.Fatal(err)
	}
	if err := h.pool.QueryRow(ctx, `SELECT count(*) FROM elitea_runtime.output_inbox WHERE execution_id = $1`, executionID).Scan(&inbox); err != nil {
		h.t.Fatal(err)
	}
	if err := h.pool.QueryRow(ctx, `SELECT count(*) FROM elitea_runtime.execution_replay_events WHERE execution_id = $1`, executionID).Scan(&replay); err != nil {
		h.t.Fatal(err)
	}
	return results, inbox, replay
}

func (h registryProjectionHarness) documents(indexID string) map[string]indexregistryapp.DocumentVersion {
	h.t.Helper()
	listed, err := h.registry.ListDocuments(context.Background(), indexID)
	if err != nil {
		h.t.Fatal(err)
	}
	out := make(map[string]indexregistryapp.DocumentVersion, len(listed))
	for _, document := range listed {
		out[document.Key] = document
	}
	return out
}

// workerFailedTerminal is what the worker does after a RESULT_REFUSED
// rejection: it settles the run FAILED with a RuntimeError of that code. That
// terminal takes the runtime-failure path, whose index terminal intent the
// registry terminalizer (indexing.RegistryIndexMetaTerminalizer) applies through
// ApplyRegistryTerminal; this is that application, with the fence of the row.
func (h registryProjectionHarness) workerFailedTerminal(row indexregistryapp.Row) error {
	h.t.Helper()
	return h.registry.ApplyRegistryTerminal(context.Background(), indexingapp.RegistryTerminal{
		ProjectID: row.ProjectID,
		CurrentTerminalIndexMeta: indexingapp.CurrentTerminalIndexMeta{
			MetaID: row.MetaID, ExecutionID: row.ExecutionID, Generation: uint64(row.ExecutionGeneration),
			IndexGeneration: uint64(row.IndexGeneration), IndexName: row.Name, ToolkitID: row.ToolkitID,
			State: indexingapp.CurrentIndexMetaFailed, OccurredAt: time.Now().UTC(),
			SafeError: "The index result was refused by the platform.",
		},
	})
}

// ADR-0030 I1: a result that names another embedding space than the index was
// stamped with is REFUSED, not recorded. The worker was told the expected space
// in its command and should have failed before writing; one that did not gets
// outputapp.ErrIndexResultRefused (RESULT_REFUSED on the wire) and the
// projecting transaction rolls back: no projection row, no inbox row, no
// replay event, no notification, and the registry row is exactly as the
// admission left it. The worker then settles FAILED with that code, and the
// registry terminalizer records the run failed. The run's own summary is never
// shown as the run's outcome.
func TestIndexRegistryRefusesAnEmbeddingMismatchBeforeRecordingAnything(t *testing.T) {
	h := newRegistryProjectionHarness(t)
	ctx := context.Background()
	settlements, err := NewSettlementsRepository(h.pool)
	if err != nil {
		t.Fatal(err)
	}

	// Run 1 succeeds and stamps the index with m1/8.
	first := h.frame("docs", "mismatch-1", 1, registrySummary(8, "m1"), nil)
	h.project(first)
	if receipt, err := settlements.PrepareSettlement(ctx, first.Settlement); err != nil || receipt.Outcome != executionapp.SettlementSucceeded {
		t.Fatalf("settle run 1: %+v err=%v", receipt, err)
	}
	stamped := mustFind(t, h.registry, "docs")
	if stamped.State != indexregistryapp.StateCompleted || stamped.Dimension != 8 || stamped.Model != "m1" {
		t.Fatalf("run 1 row = %+v", stamped)
	}

	// Run 2 names another embedding space: refused.
	second := h.frame("docs", "mismatch-2", 2, registrySummary(16, "m2"), nil)
	before := mustFind(t, h.registry, "docs")
	outcome, err := h.service.IngestIndex(ctx, second)
	if !errors.Is(err, outputapp.ErrIndexResultRefused) || outcome != (outputapp.ProjectionOutcome{}) {
		t.Fatalf("a mismatching result was not refused: %+v err=%v", outcome, err)
	}
	if results, inbox, replay := h.outputRows(second.Fence.ExecutionID); results != 0 || inbox != 0 || replay != 0 {
		t.Fatalf("the refused frame left output rows: results=%d inbox=%d replay=%d", results, inbox, replay)
	}
	if h.notifications(second) != 0 {
		t.Fatal("the refused frame left a notification")
	}
	after := mustFind(t, h.registry, "docs")
	if after.State != indexregistryapp.StateInProgress || after.Dimension != 8 || after.Model != "m1" ||
		after.ExecutionID != before.ExecutionID || after.UpdatedAt != before.UpdatedAt || len(after.History) != len(before.History) {
		t.Fatalf("the refusal changed the registry row: before %+v after %+v", before, after)
	}
	// A redelivery of the refused frame is refused again, the same way.
	if _, err := h.service.IngestIndex(ctx, second); !errors.Is(err, outputapp.ErrIndexResultRefused) {
		t.Fatalf("redelivery = %v, want the same refusal", err)
	}

	// The worker's FAILED terminal (RESULT_REFUSED) ends the run on the row.
	if err := h.workerFailedTerminal(after); err != nil {
		t.Fatal(err)
	}
	failed := mustFind(t, h.registry, "docs")
	if failed.State != indexregistryapp.StateFailed || failed.Error == nil || *failed.Error != "The index result was refused by the platform." ||
		failed.Dimension != 8 || failed.Model != "m1" {
		t.Fatalf("after the worker's failed terminal: %+v", failed)
	}
	if last := failed.History[len(failed.History)-1]; last["state"] != "failed" || last["execution_id"] != second.Fence.ExecutionID {
		t.Fatalf("the run's history entry = %+v", last)
	}
	// The registry row is at rest: the next run may start.
	if !failed.CanStartNextRun() {
		t.Fatalf("a failed run still owns the row: %+v", failed)
	}
}

// Under the rust runtime every result must carry the typed summary. One that
// carries only an artifact is refused the same way: nothing is recorded, and
// the row waits for the worker's FAILED terminal.
func TestIndexRegistryRefusesAnArtifactOnlyResult(t *testing.T) {
	h := newRegistryProjectionHarness(t)
	ctx := context.Background()

	execution, err := admitRegistryRun(t, h.pool, h.registry, "docs", "artifact-only", 1)
	if err != nil {
		t.Fatal(err)
	}
	expected, err := h.results.ExpectedIndexIngest(ctx, execution, 1)
	if err != nil {
		t.Fatal(err)
	}
	fence := claimPostgresIndexExecution(t, h.pool, expected)
	frame, artifact := postgresIndexOutputFrame(t, expected, fence)
	seedPostgresIndexArtifactAttestation(t, h.pool, expected, artifact)
	if outcome, err := h.service.IngestIndex(ctx, frame); !errors.Is(err, outputapp.ErrIndexResultRefused) || outcome.Inserted {
		t.Fatalf("an artifact-only result in rust mode: %+v err=%v", outcome, err)
	}
	row := mustFind(t, h.registry, "docs")
	if row.State != indexregistryapp.StateInProgress || row.Stamped() || row.Error != nil {
		t.Fatalf("the refusal changed the row: %+v", row)
	}
	if results, inbox, replay := h.outputRows(frame.Fence.ExecutionID); results != 0 || inbox != 0 || replay != 0 || h.notifications(frame) != 0 {
		t.Fatalf("the refused frame left rows: results=%d inbox=%d replay=%d", results, inbox, replay)
	}
	if err := h.workerFailedTerminal(row); err != nil {
		t.Fatal(err)
	}
	if got := mustFind(t, h.registry, "docs"); got.State != indexregistryapp.StateFailed || got.Stamped() {
		t.Fatalf("after the worker's failed terminal: %+v", got)
	}
}

// ADR-0030 decision 3 and I1: the per-document rows of a run go into
// index_registry_documents in the projecting transaction, the totals come from
// the result (never from counting those rows), a run that reports the whole
// index makes the rows exactly its list, and a truncated list is applied as far
// as it goes.
func TestIndexRegistryAppliesTheRunsDocumentsAndTotalsWithTheResult(t *testing.T) {
	h := newRegistryProjectionHarness(t)
	ctx := context.Background()
	typed := func(totalDocuments, totalChunks uint64) outputapp.IndexIngestSummary {
		summary := registrySummary(8, "m1")
		summary.TotalDocuments, summary.TotalChunks = totalDocuments, totalChunks
		return summary
	}
	document := func(key, version string, chunks uint32) outputapp.IndexIngestDocument {
		return outputapp.IndexIngestDocument{Key: key, Version: version, ChunkCount: chunks}
	}

	// Run 1: a fresh index, the list is the whole index.
	h.project(h.frame("docs", "docs-1", 1, typed(3, 9), func(r *outputapp.IndexIngestResult) {
		r.Documents = []outputapp.IndexIngestDocument{document("a", "1", 2), document("b", "1", 3), document("c", "1", 4)}
	}))
	row := mustFind(t, h.registry, "docs")
	if row.State != indexregistryapp.StateCompleted || row.TotalDocuments != 3 || row.TotalChunks != 9 || row.Indexed != 5 {
		t.Fatalf("run 1 row = %+v", row)
	}
	if got := h.documents(row.IndexID); len(got) != 3 || got["a"].Version != "1" || got["c"].ChunkCount != 4 {
		t.Fatalf("run 1 documents = %+v", got)
	}
	if metadata := indexregistryapp.Metadata(row); metadata["indexed"] != int64(3) || metadata["indexed_chunks"] != int64(9) {
		t.Fatalf("the list shows %v/%v", metadata["indexed"], metadata["indexed_chunks"])
	}

	// Run 2: incremental. b changed, c disappeared; a is unchanged and not in
	// the list. The totals shrink and a stays.
	h.project(h.frame("docs", "docs-2", 2, typed(2, 7), func(r *outputapp.IndexIngestResult) {
		r.Documents = []outputapp.IndexIngestDocument{document("b", "2", 5)}
		r.RemovedDocumentKeys = []string{"c"}
	}))
	row = mustFind(t, h.registry, "docs")
	if row.TotalDocuments != 2 || row.TotalChunks != 7 {
		t.Fatalf("run 2 totals = %d/%d", row.TotalDocuments, row.TotalChunks)
	}
	if got := h.documents(row.IndexID); len(got) != 2 || got["a"].Version != "1" || got["b"].Version != "2" || got["b"].ChunkCount != 5 {
		t.Fatalf("run 2 documents = %+v", got)
	}

	// Run 3: the frame could not carry every document (truncated). The one it
	// carries is applied, nothing else is deleted, and the totals are still the
	// worker's whole-index numbers.
	h.project(h.frame("docs", "docs-3", 3, typed(3, 8), func(r *outputapp.IndexIngestResult) {
		r.Documents = []outputapp.IndexIngestDocument{document("d", "1", 1)}
		r.DocumentsTruncated = true
	}))
	row = mustFind(t, h.registry, "docs")
	if row.TotalDocuments != 3 || row.TotalChunks != 8 {
		t.Fatalf("run 3 totals = %d/%d", row.TotalDocuments, row.TotalChunks)
	}
	if got := h.documents(row.IndexID); len(got) != 3 || got["a"].Version != "1" || got["b"].Version != "2" || got["d"].Version != "1" {
		t.Fatalf("run 3 documents = %+v", got)
	}

	// Run 4: a clean re-index that reports the whole index as one document.
	// Every row the list does not name goes, with no removed list needed.
	h.project(h.frame("docs", "docs-4", 4, typed(1, 1), func(r *outputapp.IndexIngestResult) {
		r.Documents = []outputapp.IndexIngestDocument{document("x", "1", 1)}
	}))
	row = mustFind(t, h.registry, "docs")
	if row.TotalDocuments != 1 || row.TotalChunks != 1 {
		t.Fatalf("run 4 totals = %d/%d", row.TotalDocuments, row.TotalChunks)
	}
	if got := h.documents(row.IndexID); len(got) != 1 || got["x"].Version != "1" {
		t.Fatalf("run 4 documents = %+v", got)
	}

	// Run 5: the source emptied out. Zero documents, nothing truncated: the
	// whole index is reported empty, and the rows go with it.
	frame := h.frame("docs", "docs-5", 5, typed(0, 0), func(r *outputapp.IndexIngestResult) {
		r.RemovedDocumentKeys = []string{"x"}
	})
	h.project(frame)
	row = mustFind(t, h.registry, "docs")
	if row.TotalDocuments != 0 || row.TotalChunks != 0 || len(h.documents(row.IndexID)) != 0 {
		t.Fatalf("run 5 row = %+v documents=%+v", row, h.documents(row.IndexID))
	}

	// The notification of a typed result still carries the run's own counts.
	var meta []byte
	if err := h.pool.QueryRow(ctx, `SELECT meta FROM centry.notifications WHERE uuid = $1::text::uuid`,
		currentIndexTerminalNotificationUUID(frame.LogicalOutputID)).Scan(&meta); err != nil {
		t.Fatalf("load notification: %v", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(meta, &decoded); err != nil || decoded["state"] != "completed" || decoded["indexed"] != float64(5) {
		t.Fatalf("notification = %s err=%v", meta, err)
	}
}
