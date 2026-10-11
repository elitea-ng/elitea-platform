package repos

// The registry backstop of the index output projection against a real
// PostgreSQL. Requires ELITEA_TEST_DATABASE_URL; it skips without it.

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	executionapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/execution"
	indexregistryapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/indexregistry"
	outputapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/output"
)

// A result that names another embedding space than the index was stamped with
// is a BACKSTOP case: the worker was told the expected space in its command and
// should have failed before writing. Main cannot make it settle FAILED (no
// output refusal does that), so the registry and the notification record the
// failure with its reason while the settlement, the stored projection and the
// replay event keep the worker's own SUCCEEDED outcome. A redelivery changes
// nothing, and the typed document count (not the SDK's `indexed` sentence
// count) reaches the notification.
func TestIndexRegistryEmbeddingMismatchIsRecordedFailedWhileTheSettlementKeepsTheWorkersOutcome(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	ctx := context.Background()
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
	service := newPostgresIndexOutputService(t, pool, results)
	settlements, err := NewSettlementsRepository(pool)
	if err != nil {
		t.Fatal(err)
	}

	project := func(key string, indexGeneration uint64, summary outputapp.IndexIngestSummary) (outputapp.IndexIngestFrame, outputapp.ProjectionOutcome) {
		t.Helper()
		execution, err := admitRegistryRun(t, pool, registry, "docs", key, indexGeneration)
		if err != nil {
			t.Fatalf("admit %s: %v", key, err)
		}
		expected, err := results.ExpectedIndexIngest(ctx, execution, 1)
		if err != nil {
			t.Fatal(err)
		}
		fence := claimPostgresIndexExecution(t, pool, expected)
		frame := postgresInlineIndexOutputFrame(t, expected, fence, summary)
		outcome, err := service.IngestIndex(ctx, frame)
		if err != nil || !outcome.Inserted {
			t.Fatalf("project %s: %+v err=%v", key, outcome, err)
		}
		return frame, outcome
	}
	notification := func(frame outputapp.IndexIngestFrame) map[string]any {
		t.Helper()
		var meta []byte
		if err := pool.QueryRow(ctx, `SELECT meta FROM centry.notifications WHERE uuid = $1::text::uuid`,
			currentIndexTerminalNotificationUUID(frame.LogicalOutputID)).Scan(&meta); err != nil {
			t.Fatalf("load notification: %v", err)
		}
		var decoded map[string]any
		if err := json.Unmarshal(meta, &decoded); err != nil {
			t.Fatal(err)
		}
		return decoded
	}

	// Run 1 succeeds and stamps. Its notification counts the run's typed
	// documents (5), not the SDK `indexed` field (which a Rust run leaves at 0).
	first, _ := project("mismatch-1", 1, registrySummary(8, "m1"))
	if got := notification(first); got["indexed"] != float64(5) || got["state"] != "completed" {
		t.Fatalf("the notification of a typed result must count IndexedDocuments: %v", got)
	}
	receipt, err := settlements.PrepareSettlement(ctx, first.Settlement)
	if err != nil || receipt.Outcome != executionapp.SettlementSucceeded {
		t.Fatalf("settle run 1: %+v err=%v", receipt, err)
	}
	stamped := mustFind(t, registry, "docs")
	if stamped.State != indexregistryapp.StateCompleted || stamped.Dimension != 8 {
		t.Fatalf("run 1 row = %+v", stamped)
	}

	// Run 2 names another embedding space.
	second, inserted := project("mismatch-2", 2, registrySummary(16, "m2"))
	row := mustFind(t, registry, "docs")
	if row.State != indexregistryapp.StateFailed || row.Error == nil || !strings.Contains(*row.Error, "embedding dimension changed") ||
		row.Dimension != 8 || row.Model != "m1" {
		t.Fatalf("registry row = %+v", row)
	}
	if last := row.History[len(row.History)-1]; last["state"] != "failed" || last["error"] != *row.Error {
		t.Fatalf("the run's history entry = %+v", last)
	}
	// The notification says failed.
	if got := notification(second); got["state"] != "failed" || !strings.Contains(fmt.Sprint(got["message"]), "is failed") {
		t.Fatalf("notification = %v", got)
	}

	// The projection row, the replay event and the settlement keep the
	// worker's outcome: nothing in the output plane was rewritten.
	var status string
	if err := pool.QueryRow(ctx, `SELECT completion_status FROM elitea_runtime.index_ingest_results WHERE execution_id = $1`,
		second.Fence.ExecutionID).Scan(&status); err != nil || status != "ok" {
		t.Fatalf("projection row status = %q err=%v", status, err)
	}
	var replay []byte
	if err := pool.QueryRow(ctx, `SELECT event_bytes FROM elitea_runtime.execution_replay_events WHERE execution_id = $1 AND event_type = 'index.ingest.completed'`,
		second.Fence.ExecutionID).Scan(&replay); err != nil {
		t.Fatal(err)
	}
	var replayed map[string]any
	if err := json.Unmarshal(replay, &replayed); err != nil || replayed["status"] != "ok" {
		t.Fatalf("replay = %s err=%v", replay, err)
	}
	var storedOutcome string
	var storedBytes []byte
	if err := pool.QueryRow(ctx, `SELECT settlement_outcome, settlement_proposal_bytes FROM elitea_runtime.output_inbox WHERE execution_id = $1`,
		second.Fence.ExecutionID).Scan(&storedOutcome, &storedBytes); err != nil {
		t.Fatal(err)
	}
	if storedOutcome != "SUCCEEDED" || string(storedBytes) != string(second.EncodedSettlement) {
		t.Fatalf("the stored settlement proposal was rewritten: outcome %s", storedOutcome)
	}

	// A redelivery of the same frame is the same output and adds nothing.
	redelivered, err := service.IngestIndex(ctx, second)
	if err != nil || redelivered.Inserted || redelivered.Cursor != inserted.Cursor {
		t.Fatalf("redelivery: %+v err=%v", redelivered, err)
	}
	var notifications, events int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM centry.notifications WHERE uuid = $1::text::uuid`,
		currentIndexTerminalNotificationUUID(second.LogicalOutputID)).Scan(&notifications); err != nil || notifications != 1 {
		t.Fatalf("notifications = %d err=%v", notifications, err)
	}
	if got := notification(second); got["state"] != "failed" {
		t.Fatalf("a redelivery rewrote the notification: %v", got)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM elitea_runtime.execution_replay_events WHERE execution_id = $1`, second.Fence.ExecutionID).Scan(&events); err != nil || events != 1 {
		t.Fatalf("replay events = %d err=%v", events, err)
	}
	if got := mustFind(t, registry, "docs"); got.State != indexregistryapp.StateFailed || *got.Error != *row.Error {
		t.Fatalf("the redelivery changed the row: %+v", got)
	}

	// The worker's SUCCEEDED proposal settles as proposed: the receipt it
	// requested, so it neither loops nor is refused.
	receipt, err = settlements.PrepareSettlement(ctx, second.Settlement)
	if err != nil || receipt.Outcome != executionapp.SettlementSucceeded {
		t.Fatalf("settle run 2: %+v err=%v", receipt, err)
	}
	if again, err := settlements.PrepareSettlement(ctx, second.Settlement); err != nil || again != receipt {
		t.Fatalf("replayed settlement: %+v err=%v", again, err)
	}
	// The registry row is at rest, so the settled job is not "settling".
	if got := mustFind(t, registry, "docs"); got.State != indexregistryapp.StateFailed || got.Active() {
		t.Fatalf("after settlement: %+v", got)
	}
}

// Under the rust runtime every result must carry the typed summary. One that
// carries only an artifact is not applied as a success: the registry records
// the run failed with the reason, and the notification says failed.
func TestIndexRegistryRecordsAnArtifactOnlyResultAsFailed(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	ctx := context.Background()
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
	service := newPostgresIndexOutputService(t, pool, results)

	execution, err := admitRegistryRun(t, pool, registry, "docs", "artifact-only", 1)
	if err != nil {
		t.Fatal(err)
	}
	expected, err := results.ExpectedIndexIngest(ctx, execution, 1)
	if err != nil {
		t.Fatal(err)
	}
	fence := claimPostgresIndexExecution(t, pool, expected)
	frame, artifact := postgresIndexOutputFrame(t, expected, fence)
	seedPostgresIndexArtifactAttestation(t, pool, expected, artifact)
	if outcome, err := service.IngestIndex(ctx, frame); err != nil || !outcome.Inserted {
		t.Fatalf("project: %+v err=%v", outcome, err)
	}
	row := mustFind(t, registry, "docs")
	if row.State != indexregistryapp.StateFailed || row.Error == nil || *row.Error != indexregistryapp.NoTypedSummaryReason ||
		row.Stamped() {
		t.Fatalf("an artifact-only result in rust mode: %+v", row)
	}
	var meta []byte
	if err := pool.QueryRow(ctx, `SELECT meta FROM centry.notifications WHERE uuid = $1::text::uuid`,
		currentIndexTerminalNotificationUUID(frame.LogicalOutputID)).Scan(&meta); err != nil {
		t.Fatalf("load notification: %v", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(meta, &decoded); err != nil || decoded["state"] != "failed" {
		t.Fatalf("notification = %s err=%v", meta, err)
	}
}
