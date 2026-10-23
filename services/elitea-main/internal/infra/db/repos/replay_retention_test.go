package repos

import (
	"context"
	"strings"
	"testing"
	"time"

	runtimedomain "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/runtime"
)

func TestReplayRetentionJanitorBoundsCandidatesAndDeletesOnlyProgress(t *testing.T) {
	executor := &scriptedExecutor{
		rowsResult: &scriptedRows{rows: []scriptedRow{{values: []any{
			"execution-1", int64(1), int64(42),
		}}}},
		rowResults: []scriptedRow{
			{values: []any{int64(3), "command-1:3", []byte(`{"progress":3}`), digestBytes(`{"progress":3}`), int64(13), int64(10), int64(2), int64(28)}},
			{values: []any{int64(2)}},
		},
	}
	store := &scriptedStore{scriptedExecutor: executor}
	repository, err := newNodeEventsRepositoryWithPolicy(store, replayRetentionPolicy{
		maxProgressEvents: 8,
		maxProgressBytes:  1024,
		maxProgressAge:    time.Minute,
		janitorBatchSize:  1,
	})
	if err != nil {
		t.Fatal(err)
	}
	deleted, err := repository.PruneExpiredReplayProgress(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if deleted != 2 || store.txCalls != 1 {
		t.Fatalf("unexpected bounded janitor result: deleted=%d tx=%d", deleted, store.txCalls)
	}
	if len(executor.queryCalls) != 1 || executor.queryCalls[0].args[2] != 1 {
		t.Fatalf("janitor candidate query is not bounded: %+v", executor.queryCalls)
	}
	for _, evidence := range []string{"event_type = $1", "LIMIT $3"} {
		if !strings.Contains(executor.queryCalls[0].sql, evidence) {
			t.Fatalf("janitor candidate query is missing %q", evidence)
		}
	}
	if len(executor.rowCalls) != 2 {
		t.Fatalf("unexpected janitor row query count: %d", len(executor.rowCalls))
	}
	deleteQuery := executor.rowCalls[1]
	for _, evidence := range []string{
		"event_type = $4",
		"bounded_expired_progress",
		"LIMIT $6",
		"prefix_bytes <= $7",
		"deleted_progress",
		"execution_replay_state",
		"pruned_through_cursor",
		"retained_progress_events",
		"retained_progress_bytes",
	} {
		if !strings.Contains(deleteQuery.sql, evidence) {
			t.Fatalf("janitor delete query is missing %q", evidence)
		}
	}
	if deleteQuery.args[3] != replayEventNodeEvent {
		t.Fatalf("janitor could target terminal replay events: %v", deleteQuery.args[3])
	}
	if deleteQuery.args[5] != replayJanitorMaxEventsPerExecution ||
		deleteQuery.args[6] != replayJanitorMaxBytesPerExecution {
		t.Fatalf("janitor per-execution delete bounds are missing: %+v", deleteQuery.args)
	}
}

func digestBytes(value string) []byte {
	digest := runtimedomain.SHA256([]byte(value))
	return digest[:]
}
