package requestlog

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
)

// meteredExecer refuses any statement that names cost_usd with 42703 while
// missing0174 is set: a database the shared migration 0174 has not reached.
type meteredExecer struct {
	missing0174 bool
	statements  []string
	args        [][]any
}

func (e *meteredExecer) Exec(_ context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	e.statements = append(e.statements, sql)
	e.args = append(e.args, args)
	if e.missing0174 && strings.Contains(sql, "cost_usd") {
		return pgconn.CommandTag{}, &pgconn.PgError{Code: "42703"}
	}
	return pgconn.CommandTag{}, nil
}

func meteredBatch() []Record {
	return []Record{
		{Provider: "openai", Model: "gpt-4o"},
		{Provider: "tavily", Model: "web_search", Metered: &MeteredFields{Units: 2, Unit: "credit", CostNanoUSD: 16_000_000}},
	}
}

func TestRequestLogWritesTheMeteredColumnsForAToolRow(t *testing.T) {
	db := &meteredExecer{}
	store := NewStore(db)
	if err := store.WriteBatch(context.Background(), meteredBatch()); err != nil {
		t.Fatal(err)
	}
	if len(db.statements) != 1 || !strings.Contains(db.statements[0], "operation, units, unit, cost_usd") {
		t.Fatalf("statements = %q, want one insert with the 0174 columns", db.statements)
	}
	args := db.args[0]
	if len(args) != 2*meteredColumnsPerRow {
		t.Fatalf("%d arguments, want %d", len(args), 2*meteredColumnsPerRow)
	}
	llm := args[meteredColumnsPerRow-4 : meteredColumnsPerRow]
	if llm[0] != operationLLM || llm[1] != nil || llm[2] != nil || llm[3] != nil {
		t.Fatalf("the model row's metered columns = %v, want llm and three NULLs", llm)
	}
	tool := args[2*meteredColumnsPerRow-4:]
	cost := tool[3].(pgtype.Numeric)
	if tool[0] != operationTool || tool[1] != int64(2) || tool[2] != "credit" || cost.Int.Int64() != 16_000_000 || cost.Exp != -9 {
		t.Fatalf("the tool row's metered columns = %v, want tool/2/credit/16000000e-9", tool)
	}
}

func TestRequestLogFallsBackWhen0174IsAbsent(t *testing.T) {
	db := &meteredExecer{missing0174: true}
	store := NewStore(db)
	clock := time.Unix(1_700_000_000, 0)
	store.now = func() time.Time { return clock }

	if err := store.WriteBatch(context.Background(), meteredBatch()); err != nil {
		t.Fatalf("a missing 0174 must not lose the batch: %v", err)
	}
	if len(db.statements) != 2 || strings.Contains(db.statements[1], "cost_usd") {
		t.Fatalf("statements = %q, want the metered insert and then the plain one", db.statements)
	}

	// The latch holds: the next batch skips the refused statement.
	if err := store.WriteBatch(context.Background(), meteredBatch()); err != nil {
		t.Fatal(err)
	}
	if len(db.statements) != 3 || strings.Contains(db.statements[2], "cost_usd") {
		t.Fatalf("a latched store tried the 0174 columns again: %q", db.statements)
	}

	// A batch with no tool row never names the 0174 columns.
	db.statements = nil
	if err := store.WriteBatch(context.Background(), meteredBatch()[:1]); err != nil {
		t.Fatal(err)
	}
	if len(db.statements) != 1 || strings.Contains(db.statements[0], "cost_usd") {
		t.Fatalf("a model-only batch named the 0174 columns: %q", db.statements)
	}
}
