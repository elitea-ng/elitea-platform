package failmode

import (
	"context"
	"errors"
	"strings"
	"testing"
	"unicode/utf8"
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
	// Each value straddles its limit with a multi-byte rune, so a byte cut
	// would split it: provider (64) with 4-byte runes, model (128) with
	// 3-byte runes, execution_id (128) with 2-byte runes.
	d.Usage.Provider = "pp" + strings.Repeat("😀", 20)    // 82 bytes; a rune spans 62..66
	d.Usage.Model = "m" + strings.Repeat("€", 60)        // 181 bytes; a rune spans 127..130
	d.Usage.ExecutionID = "x" + strings.Repeat("é", 100) // 201 bytes; a rune spans 127..129
	for name, args := range map[string][]any{"healthy/outage args": usageEventArgs(d)} {
		for _, col := range []struct {
			name  string
			got   string
			src   string
			bytes int
		}{
			{"provider", args[3].(string), d.Usage.Provider, 62},
			{"model", args[4].(string), d.Usage.Model, 127},
			{"execution_id", args[11].(string), d.Usage.ExecutionID, 127},
		} {
			if len(col.got) != col.bytes || !utf8.ValidString(col.got) || !strings.HasPrefix(col.src, col.got) {
				t.Errorf("%s %s: %d bytes (valid=%v), want a %d-byte valid prefix", name, col.name, len(col.got), utf8.ValidString(col.got), col.bytes)
			}
		}
	}
	if nullableExecutionID("") != nil {
		t.Fatal("an absent execution id is not NULL")
	}

	// The outage writer sends the same trimmed values.
	tx := &storeFakeTx{}
	if err := NewStore(&storeFakeDB{tx: tx}).PersistOutageDelta(context.Background(), d); err != nil {
		t.Fatal(err)
	}
	if got := tx.execArgs[4].(string); len(got) != 127 || !utf8.ValidString(got) {
		t.Fatalf("the outage writer sent a %d-byte model (valid=%v)", len(got), utf8.ValidString(got))
	}
}
