package failmode

import (
	"context"
	"errors"
	"testing"
	"time"
)

// ADR-0038 decision 5: a row with a reasoning count carries it while the
// column exists; every other row keeps its statement, so the money in the
// same transaction never fails on the report.
func TestUsageLedgerWritesReasoningTokensOnlyWhenItCan(t *testing.T) {
	d := healthyDeltaWithUsage()
	if statement, args := usageEventInsert(d, true); statement != usageEventInsertSQL || len(args) != 12 {
		t.Fatalf("no reasoning count: %q with %d args", statement, len(args))
	}
	d.Usage.ReasoningTokens = 412
	if statement, _ := usageEventInsert(d, false); statement != usageEventInsertSQL {
		t.Fatal("without the column the row must keep the 12-column statement")
	}
	statement, args := usageEventInsert(d, true)
	if statement != usageEventReasoningInsertSQL || len(args) != 13 || args[12] != int64(412) {
		t.Fatalf("model row with reasoning: %q args %v", statement, args)
	}
	d.Usage.Operation, d.Usage.Units, d.Usage.Unit = UsageOperationTool, 1, "request"
	statement, args = usageEventInsert(d, true)
	if statement != meteredUsageEventReasoningInsertSQL || len(args) != 16 || args[15] != int64(412) {
		t.Fatalf("tool row with reasoning: %q args %v", statement, args)
	}
}

func TestReasoningColumnRemembersPresenceAndRechecksAbsence(t *testing.T) {
	var column ReasoningColumn
	calls, answer, fail := 0, false, false
	query := func(context.Context, string) (bool, error) {
		calls++
		if fail {
			return false, errors.New("down")
		}
		return answer, nil
	}
	if column.Present(context.Background(), query) || calls != 1 {
		t.Fatalf("absent: calls %d", calls)
	}
	if column.Present(context.Background(), query) || calls != 1 {
		t.Fatal("a fresh absent answer is read again before the recheck interval")
	}
	column.checkedAt = time.Now().Add(-reasoningColumnRecheck - time.Second)
	answer = true
	if !column.Present(context.Background(), query) || calls != 2 {
		t.Fatalf("stale absent answer not reread: calls %d", calls)
	}
	fail = true
	if !column.Present(context.Background(), query) || calls != 2 {
		t.Fatal("a present column is final")
	}
}

func TestAStoreNeverProbesForARowWithoutReasoning(t *testing.T) {
	store := NewStore(&storeFakeDB{})
	if store.reasoningColumn(context.Background(), healthyDeltaWithUsage()) {
		t.Fatal("a row with no reasoning count asked for the column")
	}
	if !store.reasoning.checkedAt.IsZero() {
		t.Fatal("the column was probed for a row with no reasoning count")
	}
}
