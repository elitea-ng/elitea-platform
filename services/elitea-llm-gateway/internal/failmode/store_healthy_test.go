package failmode

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// store_healthy_test.go — PersistHealthyDelta ([human decision] 2026-10-16,
// "Healthy write", option A), over the scripted transaction. The Postgres
// integration cases in recovery_postgres_integration_test.go run the same
// paths against the shipped DDL.

func healthyDeltaWithUsage() OutageDelta {
	d := validDelta()
	user := 9
	d.Usage = &UsageDimensions{
		UserID: &user, Provider: "openai", Model: "gpt-4o",
		PromptTokens: 1, CompletionTokens: 2, OccurredAtUnix: 1500, ExecutionID: "exec-1",
	}
	return d
}

func TestPersistHealthyDelta_WritesTheGuardedHealthyUpsertAndTheLedgerRow(t *testing.T) {
	tx := &storeFakeTx{}
	if err := NewStore(&storeFakeDB{tx: tx}).PersistHealthyDelta(context.Background(), healthyDeltaWithUsage()); err != nil {
		t.Fatal(err)
	}
	if !tx.claimRan || !tx.committed {
		t.Fatalf("claim=%v commit=%v", tx.claimRan, tx.committed)
	}
	if len(tx.execSQL) != 2 || tx.execSQL[0] != healthyUpsertSQL || tx.execSQL[1] != usageEventInsertSQL {
		t.Fatalf("statements = %q, want the healthy upsert then the ledger insert", tx.execSQL)
	}
	if strings.Contains(tx.execSQL[0], "outage_mode = true") {
		t.Fatal("the healthy write marks the row outage-owned")
	}
}

func TestPersistHealthyDelta_FallsBackToTheOutageUpsertWhenTheGuardBlocks(t *testing.T) {
	tx := &storeFakeTx{affectedBy: map[string]int64{"WHERE NOT (acc.outage_mode AND NOT acc.reconciled)": 0}}
	if err := NewStore(&storeFakeDB{tx: tx}).PersistHealthyDelta(context.Background(), validDelta()); err != nil {
		t.Fatal(err)
	}
	if len(tx.execSQL) != 2 || tx.execSQL[0] != healthyUpsertSQL || tx.execSQL[1] != outageUpsertSQL {
		t.Fatalf("statements = %q, want the healthy upsert then the outage fallback", tx.execSQL)
	}
	if !tx.committed {
		t.Fatal("the fallback did not commit")
	}
}

// The consumer applied this event first: the claim conflicts and NOTHING is
// written. Deleting that early return double-bills; this test then fails.
func TestPersistHealthyDelta_AnEventTheConsumerAppliedAddsNothing(t *testing.T) {
	tx := &storeFakeTx{dedupTaken: true}
	if err := NewStore(&storeFakeDB{tx: tx}).PersistHealthyDelta(context.Background(), healthyDeltaWithUsage()); err != nil {
		t.Fatal(err)
	}
	if tx.execRan || tx.committed {
		t.Fatalf("an already-applied event wrote %q (committed=%v)", tx.execSQL, tx.committed)
	}
}

func TestPersistHealthyDelta_ErrorsRollBack(t *testing.T) {
	boom := errors.New("boom")
	for name, tc := range map[string]struct {
		tx   *storeFakeTx
		want string
	}{
		"claim":  {&storeFakeTx{dedupErr: boom}, "claim healthy event id"},
		"upsert": {&storeFakeTx{execErrBy: map[string]error{"WHERE NOT (acc.outage_mode": boom}}, "persist healthy delta"},
		"fallback": {&storeFakeTx{
			affectedBy: map[string]int64{"WHERE NOT (acc.outage_mode": 0},
			execErrBy:  map[string]error{"outage_mode = true": boom},
		}, "outage-owned row"},
		"ledger": {&storeFakeTx{execErrBy: map[string]error{"llm_usage_events": boom}}, "persist healthy usage event"},
		"commit": {&storeFakeTx{commitErr: boom}, "commit healthy delta"},
	} {
		err := NewStore(&storeFakeDB{tx: tc.tx}).PersistHealthyDelta(context.Background(), healthyDeltaWithUsage())
		if err == nil || !strings.Contains(err.Error(), tc.want) || !errors.Is(err, boom) {
			t.Errorf("%s: err = %v, want %q wrapping boom", name, err, tc.want)
		}
		if tc.tx.committed {
			t.Errorf("%s: committed after a failure", name)
		}
	}
}

func TestPersistHealthyDelta_ValidationAndBegin(t *testing.T) {
	tx := &storeFakeTx{}
	bad := validDelta()
	bad.EventID = ""
	if err := NewStore(&storeFakeDB{tx: tx}).PersistHealthyDelta(context.Background(), bad); err == nil || tx.execRan {
		t.Fatalf("an invalid delta was written: err=%v", err)
	}
	if err := NewStore(&storeFakeDB{beginErr: errors.New("no conn")}).PersistHealthyDelta(context.Background(), validDelta()); err == nil {
		t.Fatal("a begin error was swallowed")
	}
}

// [human decision] 2026-10-16, "Isolate, then drop": ledger metadata can never
// fail the money write, so an over-long provider, model or execution id is
// trimmed to its column width, on a UTF-8 boundary. Both ledger writers
// (PersistOutageDelta and PersistHealthyDelta) build their arguments here.
func TestUsageEventArgs_TrimToTheColumnWidths(t *testing.T) {
	d := healthyDeltaWithUsage()
	d.Usage.Provider = strings.Repeat("p", 200)
	d.Usage.Model = strings.Repeat("é", 100) // 200 bytes
	d.Usage.ExecutionID = strings.Repeat("x", 300)
	args := usageEventArgs(d)
	provider, model, exec := args[3].(string), args[4].(string), args[11].(string)
	if len(provider) != usageProviderBytes || len(exec) != usageExecutionIDBytes {
		t.Fatalf("provider=%d exec=%d bytes", len(provider), len(exec))
	}
	if len(model) > usageModelBytes || !strings.HasPrefix(d.Usage.Model, model) || strings.ContainsRune(model, '�') {
		t.Fatalf("model %q (%d bytes) is not a clean prefix within %d bytes", model, len(model), usageModelBytes)
	}
	if nullableExecutionID("") != nil {
		t.Fatal("an absent execution id is not NULL")
	}

	// The outage writer trims too.
	tx := &storeFakeTx{}
	if err := NewStore(&storeFakeDB{tx: tx}).PersistOutageDelta(context.Background(), d); err != nil {
		t.Fatal(err)
	}
	if got := tx.execArgs[4].(string); len(got) > usageModelBytes {
		t.Fatalf("the outage writer sent a %d-byte model", len(got))
	}
}
