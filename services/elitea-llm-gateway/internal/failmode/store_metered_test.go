package failmode

import (
	"context"
	"testing"
)

// A metered tool row (#1238) writes operation, units and unit; a model row
// keeps the 12-column statement, so a database without shared/0174 still takes
// every model row.
func TestUsageLedgerWritesOperationAndUnits(t *testing.T) {
	d := healthyDeltaWithUsage()
	d.Usage.Provider, d.Usage.Model = "tavily", "web_search"
	d.Usage.Operation, d.Usage.Units, d.Usage.Unit = UsageOperationTool, 3, "credit"

	for name, persist := range map[string]func(*Store, context.Context, OutageDelta) error{
		"outage":  (*Store).PersistOutageDelta,
		"healthy": (*Store).PersistHealthyDelta,
	} {
		tx := &storeFakeTx{}
		if err := persist(NewStore(&storeFakeDB{tx: tx}), context.Background(), d); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if last := tx.execSQL[len(tx.execSQL)-1]; last != meteredUsageEventInsertSQL {
			t.Fatalf("%s: the ledger statement is %q, want the metered insert", name, last)
		}
	}

	args := usageEventArgs(d)
	if len(args) != 15 {
		t.Fatalf("a tool row has %d arguments, want 15", len(args))
	}
	if args[12] != UsageOperationTool || *args[13].(*int64) != 3 || args[14] != "credit" {
		t.Fatalf("operation/units/unit = %v/%v/%v, want tool/3/credit", args[12], *args[13].(*int64), args[14])
	}
}

func TestOldPayloadDefaultsToLLM(t *testing.T) {
	d := healthyDeltaWithUsage()
	if usageEventStatement(d) != usageEventInsertSQL {
		t.Fatal("a model row must keep the statement without the 0174 columns")
	}
	if n := len(usageEventArgs(d)); n != 12 {
		t.Fatalf("a model row has %d arguments, want 12", n)
	}
}
