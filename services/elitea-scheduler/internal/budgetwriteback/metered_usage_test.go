package budgetwriteback

// metered_usage_test.go — the metered tool rows of the usage ledger (#1238).

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// A delta the gateway marked as a tool row writes the operation, units and unit
// columns of shared migration 0174 beside the usual row.
func TestUsageLedgerWritesOperationAndUnits(t *testing.T) {
	tx := &fakeTx{upsertAffected: 1}
	store := NewStore(&fakeDB{tx: tx})
	credits := int64(2)
	usage := &UsageDimensions{
		UserID: intPtr(7), Provider: "tavily", Model: "web_search_advanced",
		OccurredAtUnix: occurredAt, ExecutionID: "exec-1",
		Operation: "tool", Units: &credits, Unit: "credit",
	}
	if _, err := store.Apply(context.Background(), []BudgetDelta{delta("e1", 16_000_000, usage)}); err != nil {
		t.Fatal(err)
	}
	if len(tx.usageInserts) != 1 {
		t.Fatalf("ledger inserts = %d, want 1", len(tx.usageInserts))
	}
	args := tx.usageInserts[0]
	if len(args) != 15 {
		t.Fatalf("tool ledger insert took %d args, want 15", len(args))
	}
	if args[3] != "tavily" || args[4] != "web_search_advanced" || args[7] != int64(16_000_000) {
		t.Fatalf("provider/model/cost = %v/%v/%v", args[3], args[4], args[7])
	}
	if args[12] != "tool" {
		t.Fatalf("operation = %v, want tool", args[12])
	}
	if units, ok := args[13].(*int64); !ok || units == nil || *units != 2 {
		t.Fatalf("units = %v, want 2", args[13])
	}
	if args[14] != "credit" {
		t.Fatalf("unit = %v, want credit", args[14])
	}
	if !strings.Contains(meteredUsageEventSQL, "operation, units, unit") {
		t.Fatal("the tool statement does not name the 0174 columns")
	}
}

// A payload from a gateway that predates #1238 (or any model call) carries no
// operation, and is written with the original 12-column statement: the column
// default records it as "llm".
func TestOldPayloadDefaultsToLLM(t *testing.T) {
	var d BudgetDelta
	raw := `{"event_id":"e1","scope":"project","scope_id":"42","project_id":42,"period_start":1000,` +
		`"period_end":2000,"delta_nano_usd":5,"usage":{"provider":"openai","model":"gpt-4o",` +
		`"prompt_tokens":1,"completion_tokens":2,"occurred_at":1767312000}}`
	if err := json.Unmarshal([]byte(raw), &d); err != nil {
		t.Fatal(err)
	}
	if d.Usage.Operation != "" || d.Usage.Units != nil || d.Usage.Unit != "" {
		t.Fatalf("an old payload decoded with metered fields: %+v", d.Usage)
	}
	tx := &fakeTx{upsertAffected: 1}
	if _, err := NewStore(&fakeDB{tx: tx}).Apply(context.Background(), []BudgetDelta{d}); err != nil {
		t.Fatal(err)
	}
	if len(tx.usageInserts) != 1 || len(tx.usageInserts[0]) != 12 {
		t.Fatalf("model row inserts = %v, want one 12-column insert", tx.usageInserts)
	}
	// A model row encodes without the metered keys, so an older consumer
	// reads exactly what it read before.
	encoded, err := json.Marshal(UsageDimensions{Provider: "openai", Model: "gpt-4o"})
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{`"operation"`, `"units"`, `"unit"`} {
		if strings.Contains(string(encoded), key) {
			t.Fatalf("a model row encodes %s: %s", key, encoded)
		}
	}
}
