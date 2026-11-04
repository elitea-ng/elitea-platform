package budgetwriteback

import (
	"context"
	"encoding/json"
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
