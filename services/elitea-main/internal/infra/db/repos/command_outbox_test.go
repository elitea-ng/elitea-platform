package repos

import (
	"context"
	"errors"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	executionapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/execution"
	runtimedomain "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/runtime"
)

func TestListPendingValidationIDsIsBoundedOldestFirstAndPhaseOneOnly(t *testing.T) {
	executor := &scriptedExecutor{rowsResult: &scriptedRows{rows: []scriptedRow{
		{values: []any{"outbox-older"}},
		{values: []any{"outbox-newer"}},
	}}}
	repository, err := newCommandOutboxRepository(&scriptedStore{scriptedExecutor: executor}, "runtime:commands")
	if err != nil {
		t.Fatal(err)
	}

	outboxIDs, err := repository.ListPendingValidationIDs(context.Background(), 2, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(outboxIDs, []string{"outbox-older", "outbox-newer"}) {
		t.Fatalf("unexpected pending outbox IDs: %v", outboxIDs)
	}
	if len(executor.queryCalls) != 1 {
		t.Fatalf("unexpected query count: %d", len(executor.queryCalls))
	}
	call := executor.queryCalls[0]
	for _, fragment := range []string{
		"SELECT candidate.outbox_id",
		"UNION ALL",
		"o.published_at IS NULL",
		"o.published_at IS NOT NULL",
		"COALESCE(o.last_visibility_at, o.published_at)",
		"<= statement_timestamp()",
		"o.authority_granted_at IS NULL",
		"j.state = 'PENDING'",
		"j.state IN ('PENDING', 'DISPATCHED')",
		"j.capability_id = 'configuration.validate.v1'",
		"j.generation = 1",
		"ORDER BY o.created_at ASC, o.outbox_id ASC",
		"ORDER BY COALESCE(o.last_visibility_at, o.published_at) ASC, o.outbox_id ASC",
		"ORDER BY candidate.visibility_order ASC, candidate.outbox_id ASC",
		"LIMIT $2",
	} {
		if !strings.Contains(call.sql, fragment) {
			t.Fatalf("pending outbox query is missing %q", fragment)
		}
	}
	if strings.Contains(call.sql, "DISTINCT ON") || strings.Contains(call.sql, "FOR UPDATE") || strings.Contains(call.sql, "SKIP LOCKED") {
		t.Fatal("discovery query unexpectedly took transaction or row-lock ownership")
	}
	if !reflect.DeepEqual(call.args, []any{"runtime:commands", int32(2), int64(60_000)}) {
		t.Fatalf("unexpected query arguments: %#v", call.args)
	}
}

func TestListPendingValidationIDsRejectsInvalidOrViolatedLimit(t *testing.T) {
	for _, limit := range []int{0, -1, executionapp.MaxOutboxPublisherBatchSize + 1} {
		t.Run("limit", func(t *testing.T) {
			executor := &scriptedExecutor{}
			repository, err := newCommandOutboxRepository(&scriptedStore{scriptedExecutor: executor}, "runtime:commands")
			if err != nil {
				t.Fatal(err)
			}
			if _, err := repository.ListPendingValidationIDs(context.Background(), limit, time.Minute); !errors.Is(err, executionapp.ErrInvalidPendingOutboxLimit) {
				t.Fatalf("expected invalid limit, got %v", err)
			}
			if len(executor.queryCalls) != 0 {
				t.Fatal("invalid limit reached the database")
			}
		})
	}
	for _, visibilityTimeout := range []time.Duration{0, executionapp.MinOutboxVisibilityTimeout - time.Nanosecond, executionapp.MaxOutboxVisibilityTimeout + time.Nanosecond} {
		repository, err := newCommandOutboxRepository(&scriptedStore{scriptedExecutor: &scriptedExecutor{}}, "runtime:commands")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := repository.ListPendingValidationIDs(context.Background(), 1, visibilityTimeout); !errors.Is(err, executionapp.ErrInvalidPendingOutboxLimit) {
			t.Fatalf("expected invalid visibility timeout %s, got %v", visibilityTimeout, err)
		}
	}

	executor := &scriptedExecutor{rowsResult: &scriptedRows{rows: []scriptedRow{
		{values: []any{"outbox-1"}},
		{values: []any{"outbox-2"}},
	}}}
	repository, err := newCommandOutboxRepository(&scriptedStore{scriptedExecutor: executor}, "runtime:commands")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := repository.ListPendingValidationIDs(context.Background(), 1, time.Minute); !errors.Is(err, executionapp.ErrPendingOutboxBatchLimitExceeded) {
		t.Fatalf("expected violated store limit, got %v", err)
	}
}

func TestListPendingValidationIDsRejectsInvalidStoredIdentityAndIterationFailure(t *testing.T) {
	tests := []struct {
		name string
		rows *scriptedRows
	}{
		{name: "empty identity", rows: &scriptedRows{rows: []scriptedRow{{values: []any{""}}}}},
		{name: "scan failure", rows: &scriptedRows{rows: []scriptedRow{{err: errors.New("decode failed")}}}},
		{name: "iteration failure", rows: &scriptedRows{err: errors.New("connection failed")}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			repository, err := newCommandOutboxRepository(&scriptedStore{scriptedExecutor: &scriptedExecutor{rowsResult: test.rows}}, "runtime:commands")
			if err != nil {
				t.Fatal(err)
			}
			if _, err := repository.ListPendingValidationIDs(context.Background(), 1, time.Minute); err == nil {
				t.Fatal("expected invalid stored row to fail closed")
			}
		})
	}
}

func TestStorePreparedValidationReturnsConcurrentCASWinner(t *testing.T) {
	candidate := repositoryPreparedEnvelope("candidate")
	winner := repositoryPreparedEnvelope("winner")
	executor := &scriptedExecutor{rowResults: []scriptedRow{
		{values: []any{
			winner.Bytes, winner.Digest[:], winner.SignatureProfile, winner.KeyID,
			false, false, false, false, "PENDING", "configuration.validate.v1",
		}},
	}}
	store := &scriptedStore{scriptedExecutor: executor}
	repository, err := newCommandOutboxRepository(store, "runtime:commands")
	if err != nil {
		t.Fatal(err)
	}

	selected, err := repository.StorePreparedValidation(context.Background(), "outbox-1", candidate)
	if err != nil {
		t.Fatal(err)
	}
	if selected.Envelope.Digest != winner.Digest || string(selected.Envelope.Bytes) != string(winner.Bytes) || selected.Envelope.KeyID != winner.KeyID || selected.Published {
		t.Fatalf("repository did not return durable CAS winner: %+v", selected)
	}
	if store.txCalls != 1 || len(executor.rowCalls) != 1 {
		t.Fatalf("unexpected CAS query count: tx=%d rows=%d", store.txCalls, len(executor.rowCalls))
	}
	if !strings.Contains(executor.rowCalls[0].sql, "FOR UPDATE OF j, o") || !strings.Contains(executor.rowCalls[0].sql, "prepared_signed_envelope_digest") {
		t.Fatal("prepared-envelope CAS does not lock and return one durable winner")
	}
}

func TestLoadPreparedValidationRejectsCorruptStoredDigest(t *testing.T) {
	envelope := repositoryPreparedEnvelope("stored")
	wrongDigest := runtimedomain.SHA256([]byte("other-envelope"))
	executor := &scriptedExecutor{rowResults: []scriptedRow{{values: []any{
		envelope.Bytes, wrongDigest[:], envelope.SignatureProfile, envelope.KeyID,
		false, false, false,
	}}}}
	repository, err := newCommandOutboxRepository(&scriptedStore{scriptedExecutor: executor}, "runtime:commands")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := repository.LoadPreparedValidation(context.Background(), "outbox-1"); !errors.Is(err, executionapp.ErrInvalidPreparedEnvelope) {
		t.Fatalf("expected corrupt stored envelope rejection, got %v", err)
	}
}

func TestRetireNoAuthorityValidationUsesSeparateBoundedIndexedScans(t *testing.T) {
	executor := &scriptedExecutor{rowsResults: []*scriptedRows{{}, {}}}
	repository, err := newCommandOutboxRepository(&scriptedStore{scriptedExecutor: executor}, "runtime:commands")
	if err != nil {
		t.Fatal(err)
	}
	retired, err := repository.RetireNoAuthorityValidation(context.Background(), 7)
	if err != nil {
		t.Fatal(err)
	}
	if retired != 0 || len(executor.queryCalls) != 2 {
		t.Fatalf("unexpected bounded reconciliation result: retired=%d queries=%d", retired, len(executor.queryCalls))
	}
	cancelled := executor.queryCalls[0]
	for _, fragment := range []string{
		"FROM elitea_runtime.execution_jobs AS j",
		"j.desired_state = 'CANCELLED'",
		"j.state IN ('PENDING', 'DISPATCHED')",
		"j.capability_id = 'configuration.validate.v1'",
		"j.generation = 1",
		"ORDER BY j.admitted_at ASC, j.execution_id ASC, j.generation ASC",
		"LIMIT $2",
		"FOR UPDATE OF j, o SKIP LOCKED",
	} {
		if !strings.Contains(cancelled.sql, fragment) {
			t.Fatalf("cancelled reconciliation query is missing %q", fragment)
		}
	}
	expired := executor.queryCalls[1]
	for _, fragment := range []string{
		"FROM elitea_runtime.command_outbox AS o",
		"o.deadline <= clock_timestamp()",
		"o.retired_at IS NULL",
		"o.authority_granted_at IS NULL",
		"o.stream_name = $1",
		"j.desired_state <> 'CANCELLED'",
		"ORDER BY o.deadline ASC, o.outbox_id ASC",
		"LIMIT $2",
		"FOR UPDATE OF j, o SKIP LOCKED",
	} {
		if !strings.Contains(expired.sql, fragment) {
			t.Fatalf("deadline reconciliation query is missing %q", fragment)
		}
	}
	if strings.Contains(cancelled.sql, " OR ") || strings.Contains(expired.sql, " OR ") {
		t.Fatal("bounded retirement scans were recombined with an unindexable OR")
	}
	if !reflect.DeepEqual(cancelled.args, []any{"runtime:commands", int32(7)}) || !reflect.DeepEqual(expired.args, []any{"runtime:commands", int32(7)}) {
		t.Fatalf("reconciliation lost its hard limits: cancel=%#v expired=%#v", cancelled.args, expired.args)
	}
}

func TestRetireNoAuthorityValidationConsumesCancellationCapacityFirst(t *testing.T) {
	executor := &scriptedExecutor{
		rowsResults: []*scriptedRows{
			{rows: []scriptedRow{{values: []any{"outbox-cancel", "execution-cancel", int64(1), int64(1), "CANCELLED"}}}},
			{rows: []scriptedRow{{values: []any{"outbox-deadline", "execution-deadline", int64(1), int64(1), "RUNNING"}}}},
		},
		rowResults: []scriptedRow{{values: []any{int64(1), "", ""}}, {values: []any{int64(1), "", ""}}},
	}
	repository, err := newCommandOutboxRepository(&scriptedStore{scriptedExecutor: executor}, "runtime:commands")
	if err != nil {
		t.Fatal(err)
	}
	retired, err := repository.RetireNoAuthorityValidation(context.Background(), 3)
	if err != nil {
		t.Fatal(err)
	}
	if retired != 2 || len(executor.queryCalls) != 2 || executor.queryCalls[1].args[1] != int32(2) {
		t.Fatalf("cancellation did not consume bounded capacity first: retired=%d calls=%#v", retired, executor.queryCalls)
	}
	if len(executor.execCalls) != 2 {
		t.Fatalf("retirements omitted replay events: %d", len(executor.execCalls))
	}
	if executor.execCalls[0].args[4] != replayEventRuntimeFailure || !reflect.DeepEqual(executor.execCalls[0].args[5], cancellationRetirementEventBytes) {
		t.Fatal("cancellation retirement changed its established replay contract")
	}
	if executor.execCalls[1].args[4] != deadlineRetirementEventType || !reflect.DeepEqual(executor.execCalls[1].args[5], deadlineRetirementEventBytes) {
		t.Fatal("deadline retirement changed its typed replay contract")
	}
}

func TestRetirementIndexesMatchBoundedScanPredicates(t *testing.T) {
	migration, err := os.ReadFile("../../../../migrations/shared/0030_execution_kernel.sql")
	if err != nil {
		t.Fatal(err)
	}
	sql := string(migration)
	for _, fragment := range []string{
		"ON elitea_runtime.command_outbox (stream_name, deadline, outbox_id)",
		"WHERE retired_at IS NULL AND authority_granted_at IS NULL",
		"ON elitea_runtime.execution_jobs (\n        capability_id, generation, admitted_at, execution_id\n    )",
		"WHERE desired_state = 'CANCELLED' AND state IN ('PENDING', 'DISPATCHED')",
		"WHERE published_at IS NULL AND retired_at IS NULL",
	} {
		if !strings.Contains(sql, fragment) {
			t.Fatalf("retirement index contract is missing %q", fragment)
		}
	}
}

func TestVisibilityRepairMigrationIsExpandCompatible(t *testing.T) {
	migration, err := os.ReadFile("../../../../migrations/shared/0035_command_visibility_repair.sql")
	if err != nil {
		t.Fatal(err)
	}
	sql := string(migration)
	for _, fragment := range []string{
		"ADD COLUMN IF NOT EXISTS last_visibility_at TIMESTAMPTZ",
		"last_visibility_at IS NULL OR published_at IS NOT NULL",
		") NOT VALID",
		"COALESCE(last_visibility_at, published_at)",
		"command_outbox_visibility_repair_idx",
	} {
		if !strings.Contains(sql, fragment) {
			t.Fatalf("visibility-repair migration is missing %q", fragment)
		}
	}
	if strings.Contains(sql, "UPDATE elitea_runtime.command_outbox") || strings.Contains(sql, "last_visibility_at TIMESTAMPTZ NOT NULL") {
		t.Fatal("visibility-repair migration performs an unbounded backfill or rejects N-1 writers")
	}
}

func TestLoadPreparedValidationRejectsRetiredAndExpiredRows(t *testing.T) {
	tests := []struct {
		name    string
		retired bool
		expired bool
		want    error
	}{
		{name: "retired", retired: true, want: executionapp.ErrDispatchRetired},
		{name: "expired", expired: true, want: executionapp.ErrDispatchDeadlineExpired},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			executor := &scriptedExecutor{rowResults: []scriptedRow{{values: []any{
				nil, nil, int32(0), "", false, test.retired, test.expired,
			}}}}
			repository, err := newCommandOutboxRepository(&scriptedStore{scriptedExecutor: executor}, "runtime:commands")
			if err != nil {
				t.Fatal(err)
			}
			if _, err := repository.LoadPreparedValidation(context.Background(), "outbox-1"); !errors.Is(err, test.want) {
				t.Fatalf("got %v want %v", err, test.want)
			}
		})
	}
}

func repositoryPreparedEnvelope(suffix string) executionapp.PreparedCommandEnvelope {
	encoded := []byte("signed-envelope:" + suffix)
	return executionapp.PreparedCommandEnvelope{
		Bytes:            encoded,
		Digest:           runtimedomain.SHA256(encoded),
		SignatureProfile: 1,
		KeyID:            "key-" + suffix,
	}
}
