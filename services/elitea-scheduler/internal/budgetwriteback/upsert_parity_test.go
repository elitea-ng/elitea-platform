package budgetwriteback

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// guardedUpsertContract is the checked-in statement both writers must send
// (libs/proto/contracts/budget-writeback-guarded-upsert-v1.sql). The gateway
// module reads the same file for its PersistHealthyDelta statement
// (internal/failmode/upsert_parity_test.go).
var guardedUpsertContract = filepath.Join("..", "..", "..", "..",
	"libs", "proto", "contracts", "budget-writeback-guarded-upsert-v1.sql")

func normaliseSQL(sql string) string {
	var kept []string
	for _, line := range strings.Split(sql, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "--") {
			continue
		}
		kept = append(kept, line)
	}
	return strings.Join(strings.Fields(strings.Join(kept, " ")), " ")
}

func TestUpsertSQLMatchesTheWriteBackContract(t *testing.T) {
	raw, err := os.ReadFile(guardedUpsertContract) //nolint:gosec // a fixed in-repo path
	if err != nil {
		t.Fatalf("read the contract: %v", err)
	}
	if got, want := normaliseSQL(upsertSQL), normaliseSQL(string(raw)); got != want {
		t.Fatalf("upsertSQL drifted from %s:\n got: %s\nwant: %s", guardedUpsertContract, got, want)
	}
}
