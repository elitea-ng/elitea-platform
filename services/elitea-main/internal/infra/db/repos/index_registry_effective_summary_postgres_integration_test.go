package repos

// The effective-summary rule of the index output projection against a real
// PostgreSQL. Requires ELITEA_TEST_DATABASE_URL; it skips without it.

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	runtimev1 "github.com/EliteaAI/elitea-platform/libs/proto/gen/go/elitea/runtime/v1"
	executionapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/execution"
	indexregistryapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/indexregistry"
	outputapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/output"
	runtimedomain "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/runtime"
	"google.golang.org/protobuf/proto"
)

// One effective summary everywhere. A result that names another embedding space
// than the index was stamped with is a failure, and it is a failure in every
// place the result is described: the registry row, the projection row, the
// browser replay event, the notification, the settlement proposal and the
// settlement itself, and a redelivery of the same frame. The typed document
// count (not the SDK's `indexed` sentence count) reaches the notification.
func TestIndexRegistryEmbeddingMismatchIsOneFailureEverywhere(t *testing.T) {
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

	// Run 1 succeeds and stamps. Its notification counts the typed documents
	// (5), not the SDK `indexed` field (which a Rust run leaves at 0).
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
	message := *row.Error

	// The projection row.
	var status, completionMessage string
	if err := pool.QueryRow(ctx, `SELECT completion_status, completion_message FROM elitea_runtime.index_ingest_results WHERE execution_id = $1`,
		second.Fence.ExecutionID).Scan(&status, &completionMessage); err != nil || status != "error" || completionMessage != message {
		t.Fatalf("projection row = %q %q err=%v", status, completionMessage, err)
	}
	// The browser replay event.
	var replay []byte
	if err := pool.QueryRow(ctx, `SELECT event_bytes FROM elitea_runtime.execution_replay_events WHERE execution_id = $1 AND event_type = 'index.ingest.completed'`,
		second.Fence.ExecutionID).Scan(&replay); err != nil {
		t.Fatal(err)
	}
	var replayed map[string]any
	if err := json.Unmarshal(replay, &replayed); err != nil || replayed["status"] != "error" || replayed["message"] != message {
		t.Fatalf("replay = %s err=%v", replay, err)
	}
	// The notification.
	if got := notification(second); got["state"] != "failed" || !strings.Contains(fmt.Sprint(got["message"]), "is failed") {
		t.Fatalf("notification = %v", got)
	}
	// The stored settlement proposal is the canonical FAILED one.
	var storedOutcome string
	var storedBytes, storedDigest []byte
	if err := pool.QueryRow(ctx, `SELECT settlement_outcome, settlement_proposal_bytes, settlement_proposal_digest FROM elitea_runtime.output_inbox WHERE execution_id = $1`,
		second.Fence.ExecutionID).Scan(&storedOutcome, &storedBytes, &storedDigest); err != nil {
		t.Fatal(err)
	}
	var wire runtimev1.SettlementProposalV1
	if err := proto.Unmarshal(storedBytes, &wire); err != nil || storedOutcome != "FAILED" ||
		wire.GetRequestedOutcome() != runtimev1.ExecutionOutcomeV1_EXECUTION_OUTCOME_V1_FAILED ||
		runtimedomain.SHA256(storedBytes) != runtimedomain.Digest(storedDigest[:32]) {
		t.Fatalf("stored settlement: outcome=%s wire=%v err=%v", storedOutcome, &wire, err)
	}
	// A redelivery of the same frame is the same output, still described as
	// failed, and adds nothing.
	redelivered, err := service.IngestIndex(ctx, second)
	if err != nil || redelivered.Inserted || redelivered.Cursor != inserted.Cursor {
		t.Fatalf("redelivery: %+v err=%v", redelivered, err)
	}
	var notifications, events int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM centry.notifications WHERE uuid = $1::text::uuid`,
		currentIndexTerminalNotificationUUID(second.LogicalOutputID)).Scan(&notifications); err != nil || notifications != 1 {
		t.Fatalf("notifications = %d err=%v", notifications, err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM elitea_runtime.execution_replay_events WHERE execution_id = $1`, second.Fence.ExecutionID).Scan(&events); err != nil || events != 1 {
		t.Fatalf("replay events = %d err=%v", events, err)
	}
	if got := mustFind(t, registry, "docs"); got.State != indexregistryapp.StateFailed || *got.Error != message {
		t.Fatalf("the redelivery changed the row: %+v", got)
	}

	// The worker still proposes the SUCCEEDED original; the settlement is the
	// stored FAILED one, and replays of either shape agree.
	receipt, err = settlements.PrepareSettlement(ctx, second.Settlement)
	if err != nil || receipt.Outcome != executionapp.SettlementFailed {
		t.Fatalf("settle run 2: %+v err=%v", receipt, err)
	}
	if again, err := settlements.PrepareSettlement(ctx, second.Settlement); err != nil || again != receipt {
		t.Fatalf("replayed settlement: %+v err=%v", again, err)
	}
	var jobState string
	if err := pool.QueryRow(ctx, `SELECT state FROM elitea_runtime.execution_jobs WHERE execution_id = $1`, second.Fence.ExecutionID).Scan(&jobState); err != nil || jobState != "FAILED" {
		t.Fatalf("job state = %q err=%v", jobState, err)
	}

}
