package repos

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	indexingapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/indexing"
	"github.com/jackc/pgx/v5/pgconn"
)

func TestCurrentIndexManualStopCleanupClaimsOnlySettledTerminalReadyRows(
	t *testing.T,
) {
	store := &scriptedExecutor{rowsResult: &scriptedRows{
		rows: []scriptedRow{{values: []any{"execution-1", int64(3)}}},
	}}
	repository, err := newCurrentIndexManualStopCleanupRepository(store)
	if err != nil {
		t.Fatal(err)
	}
	claims, err := repository.ClaimPendingManualStopCleanups(
		context.Background(),
		"claim-1",
		8,
		time.Minute,
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(claims) != 1 ||
		claims[0] != (indexingapp.CurrentManualStopCleanupClaim{
			CurrentManualStopCleanupRequest: indexingapp.CurrentManualStopCleanupRequest{
				ExecutionID: "execution-1",
				Generation:  3,
			},
			ClaimToken: "claim-1",
		}) {
		t.Fatalf("claims=%+v", claims)
	}
	query := store.queryCalls[0].sql
	for _, predicate := range []string{
		"i.index_manual_cleanup_status = 'PENDING'",
		"i.index_manual_cleanup_next_attempt_at <= clock_timestamp()",
		"j.desired_state = 'CANCELLED'",
		"j.state = 'CANCELLED'",
		"j.settled_at IS NOT NULL",
		"i.index_meta_terminal_state = 'cancelled'",
		"i.index_meta_terminal_status IN ('APPLIED', 'SUPERSEDED')",
		"FOR UPDATE OF i SKIP LOCKED",
		"LIMIT $1",
	} {
		if !strings.Contains(query, predicate) {
			t.Fatalf("cleanup claim SQL is missing %q", predicate)
		}
	}
	for _, forbidden := range []string{
		"output_inbox",
		"command_outbox",
		"cmetadata",
		"connection_string",
	} {
		if strings.Contains(query, forbidden) {
			t.Fatalf("cleanup claim crosses external/source boundary via %q", forbidden)
		}
	}
}

func TestCurrentIndexManualStopCleanupSupersessionOwnsExactClaim(t *testing.T) {
	store := &scriptedExecutor{rowResults: []scriptedRow{
		{values: []any{true, true}},
	}}
	repository, err := newCurrentIndexManualStopCleanupRepository(store)
	if err != nil {
		t.Fatal(err)
	}
	superseded, err := repository.SupersedeManualStopCleanupIfNewerInitialized(
		context.Background(),
		manualStopCleanupClaim(),
	)
	if err != nil || !superseded {
		t.Fatalf("superseded=%v err=%v", superseded, err)
	}
	query := store.rowCalls[0]
	for _, predicate := range []string{
		"i.index_manual_cleanup_status = 'PENDING'",
		"i.index_manual_cleanup_claim_token = $3",
		"stale.index_meta_terminal_status = 'SUPERSEDED'",
		"newer_index.index_generation > stale.index_generation",
		"(newer_job.admitted_at, newer_job.execution_id)",
		"index_manual_cleanup_status = 'SUPERSEDED'",
	} {
		if !strings.Contains(query.sql, predicate) {
			t.Fatalf("cleanup supersession SQL is missing %q", predicate)
		}
	}
}

func TestCurrentIndexManualStopCleanupResolutionAndRetryOwnClaimToken(
	t *testing.T,
) {
	t.Run("resolve", func(t *testing.T) {
		store := &scriptedExecutor{rowResults: []scriptedRow{
			{values: []any{"APPLIED"}},
		}}
		repository, err := newCurrentIndexManualStopCleanupRepository(store)
		if err != nil {
			t.Fatal(err)
		}
		if err := repository.ResolveManualStopCleanup(
			context.Background(),
			manualStopCleanupClaim(),
			indexingapp.CurrentManualStopCleanupApplied,
		); err != nil {
			t.Fatal(err)
		}
		query := store.rowCalls[0].sql
		for _, predicate := range []string{
			"index_manual_cleanup_status = $4",
			"index_manual_cleanup_claim_token = $3",
			"index_manual_cleanup_resolved_at",
		} {
			if !strings.Contains(query, predicate) {
				t.Fatalf("cleanup resolution SQL is missing %q", predicate)
			}
		}
	})

	t.Run("release", func(t *testing.T) {
		store := &scriptedExecutor{
			execTags: []pgconn.CommandTag{pgconn.NewCommandTag("UPDATE 1")},
		}
		repository, err := newCurrentIndexManualStopCleanupRepository(store)
		if err != nil {
			t.Fatal(err)
		}
		if err := repository.ReleaseManualStopCleanup(
			context.Background(),
			manualStopCleanupClaim(),
			"DEPENDENCY_UNAVAILABLE",
		); err != nil {
			t.Fatal(err)
		}
		query := store.execCalls[0].sql
		for _, predicate := range []string{
			"index_manual_cleanup_claim_token = NULL",
			"index_manual_cleanup_next_attempt_at",
			"index_manual_cleanup_last_error_code = $4",
			"index_manual_cleanup_claim_token = $3",
		} {
			if !strings.Contains(query, predicate) {
				t.Fatalf("cleanup retry SQL is missing %q", predicate)
			}
		}
	})
}

func TestCurrentIndexManualStopCleanupRejectsLostClaim(t *testing.T) {
	store := &scriptedExecutor{rowResults: []scriptedRow{
		{err: errors.New("lost claim")},
	}}
	repository, err := newCurrentIndexManualStopCleanupRepository(store)
	if err != nil {
		t.Fatal(err)
	}
	if err := repository.ResolveManualStopCleanup(
		context.Background(),
		manualStopCleanupClaim(),
		indexingapp.CurrentManualStopCleanupApplied,
	); err == nil {
		t.Fatal("lost cleanup claim was accepted")
	}
}

func TestIndexManualStopCleanupMigrationHasNoUnsafeHistoricalBackfill(
	t *testing.T,
) {
	migration, err := os.ReadFile(
		"../../../../migrations/shared/0048_index_manual_stop_cleanup.sql",
	)
	if err != nil {
		t.Fatal(err)
	}
	sql := string(migration)
	for _, fragment := range []string{
		"No historical backfill is safe",
		"ADD COLUMN index_manual_stop_requested_at TIMESTAMPTZ",
		"ADD COLUMN index_manual_cleanup_status TEXT",
		"index_ingest_jobs_manual_cleanup_identity",
		"index_ingest_jobs_manual_cleanup_requires_initialization",
		"WHERE index_manual_cleanup_status = 'PENDING'",
	} {
		if !strings.Contains(sql, fragment) {
			t.Fatalf("manual cleanup migration is missing %q", fragment)
		}
	}
	for _, forbidden := range []string{
		"output_inbox",
		"command_outbox",
		"UPDATE elitea_runtime.index_ingest_jobs",
	} {
		if strings.Contains(sql, forbidden) {
			t.Fatalf("manual cleanup migration attempts unsafe backfill via %q", forbidden)
		}
	}
}

func manualStopCleanupClaim() indexingapp.CurrentManualStopCleanupClaim {
	return indexingapp.CurrentManualStopCleanupClaim{
		CurrentManualStopCleanupRequest: indexingapp.CurrentManualStopCleanupRequest{
			ExecutionID: "execution-1",
			Generation:  3,
		},
		ClaimToken: "claim-1",
	}
}
