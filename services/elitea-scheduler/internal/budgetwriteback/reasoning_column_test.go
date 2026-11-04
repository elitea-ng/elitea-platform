package budgetwriteback

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

// ADR-0038 decision 5: the reasoning count is the statement's next column and
// placeholder, on the model insert and on the metered one.
func TestWithReasoningTokensAddsTheNextColumn(t *testing.T) {
	for statement, placeholder := range map[string]string{usageEventSQL: "$13)", meteredUsageEventSQL: "$16)"} {
		got := withReasoningTokens(statement)
		if !strings.Contains(got, "execution_id, reasoning_tokens)") && !strings.Contains(got, "unit, reasoning_tokens)") {
			t.Errorf("no column: %s", got)
		}
		if !strings.Contains(got, ", "+placeholder+"\n\tON CONFLICT") {
			t.Errorf("want placeholder %s: %s", placeholder, got)
		}
	}
}

// probingDB is a fakeDB that also answers the column probe on the pool.
type probingDB struct {
	*fakeDB
	row    fakeColumnRow
	probes int
}

func (d *probingDB) QueryRow(context.Context, string, ...any) Row {
	d.probes++
	return d.row
}

type fakeColumnRow struct {
	present bool
	err     error
}

func (r fakeColumnRow) Scan(dest ...any) error {
	if r.err != nil {
		return r.err
	}
	*(dest[0].(*bool)) = r.present
	return nil
}

// A probe that fails runs on the pool, not in the money transaction, so the
// budget write still commits, with the ledger row and without the count.
func TestAFailingProbeDoesNotRollBackTheBudgetWrite(t *testing.T) {
	tx := &fakeTx{upsertAffected: 1}
	db := &probingDB{fakeDB: &fakeDB{tx: tx}, row: fakeColumnRow{err: errors.New("canceling statement")}}
	store := NewStore(db)
	usage := &UsageDimensions{Provider: "vllm", Model: "qwen3", CompletionTokens: 900, ReasoningTokens: 412, OccurredAtUnix: occurredAt}
	if _, err := store.Apply(context.Background(), []BudgetDelta{delta("e1", 1_000, usage)}); err != nil {
		t.Fatal(err)
	}
	if db.probes != 1 {
		t.Fatalf("probes on the pool = %d, want 1", db.probes)
	}
	if !tx.committed || !tx.upsertRan {
		t.Fatalf("money tx committed=%v upsert=%v, want the budget write committed", tx.committed, tx.upsertRan)
	}
	if len(tx.usageInserts) != 1 || len(tx.usageInserts[0]) != 12 {
		t.Fatalf("ledger inserts = %v, want one 12-argument row", tx.usageInserts)
	}
}

// A present column adds the count; a group without one never asks.
func TestAPresentColumnCarriesTheCount(t *testing.T) {
	tx := &fakeTx{upsertAffected: 1}
	db := &probingDB{fakeDB: &fakeDB{tx: tx}, row: fakeColumnRow{present: true}}
	store := NewStore(db)
	plain := &UsageDimensions{Provider: "vllm", Model: "qwen3", CompletionTokens: 900, OccurredAtUnix: occurredAt}
	if _, err := store.Apply(context.Background(), []BudgetDelta{delta("e0", 1_000, plain)}); err != nil {
		t.Fatal(err)
	}
	if db.probes != 0 {
		t.Fatalf("a group with no count probed %d times", db.probes)
	}
	tx2 := &fakeTx{upsertAffected: 1}
	db.fakeDB = &fakeDB{tx: tx2}
	store.db = db
	reasoning := &UsageDimensions{Provider: "vllm", Model: "qwen3", CompletionTokens: 900, ReasoningTokens: 412, OccurredAtUnix: occurredAt}
	if _, err := store.Apply(context.Background(), []BudgetDelta{delta("e1", 1_000, reasoning)}); err != nil {
		t.Fatal(err)
	}
	if len(tx2.usageInserts) != 1 || len(tx2.usageInserts[0]) != 13 || tx2.usageInserts[0][12] != int64(412) {
		t.Fatalf("ledger inserts = %v, want the count as the 13th argument", tx2.usageInserts)
	}
}

// A ledger that cannot be asked about the column still gets the row, without
// the count: the report never fails the money.
func TestAFailedColumnProbeWritesTheRowWithoutReasoning(t *testing.T) {
	tx := &fakeTx{upsertAffected: 1}
	store := NewStore(&fakeDB{tx: tx})
	usage := &UsageDimensions{Provider: "vllm", Model: "qwen3", CompletionTokens: 900, ReasoningTokens: 412, OccurredAtUnix: occurredAt}
	if _, err := store.Apply(context.Background(), []BudgetDelta{delta("e1", 1_000, usage)}); err != nil {
		t.Fatal(err)
	}
	if len(tx.usageInserts) != 1 || len(tx.usageInserts[0]) != 12 {
		t.Fatalf("ledger inserts = %v, want one 12-argument row", tx.usageInserts)
	}
}

func TestReasoningTokensDecodeFromTheGatewayPayload(t *testing.T) {
	var usage UsageDimensions
	if err := json.Unmarshal([]byte(`{"completion_tokens":900,"reasoning_tokens":412}`), &usage); err != nil {
		t.Fatal(err)
	}
	if usage.ReasoningTokens != 412 {
		t.Fatalf("reasoning_tokens = %d", usage.ReasoningTokens)
	}
}
