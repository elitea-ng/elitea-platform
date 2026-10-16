package failmode

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// guardedUpsertContract is the checked-in statement both writers must send
// (libs/proto/contracts/budget-writeback-guarded-upsert-v1.sql). The scheduler
// module reads the same file in its own parity test, so a change to either
// statement fails one side until all three agree.
var guardedUpsertContract = filepath.Join("..", "..", "..", "..",
	"libs", "proto", "contracts", "budget-writeback-guarded-upsert-v1.sql")

// normaliseSQL drops `--` comment lines and collapses all whitespace.
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

func TestHealthyUpsertSQLMatchesTheWriteBackContract(t *testing.T) {
	raw, err := os.ReadFile(guardedUpsertContract) //nolint:gosec // a fixed in-repo path
	if err != nil {
		t.Fatalf("read the contract: %v", err)
	}
	if got, want := normaliseSQL(healthyUpsertSQL), normaliseSQL(string(raw)); got != want {
		t.Fatalf("healthyUpsertSQL drifted from %s:\n got: %s\nwant: %s", guardedUpsertContract, got, want)
	}
	// The integration test's copy of the consumer statement is the same
	// contract too.
	if got, want := normaliseSQL(writeBackUpsertSQL), normaliseSQL(string(raw)); got != want {
		t.Fatalf("writeBackUpsertSQL (integration copy) drifted from %s:\n got: %s\nwant: %s", guardedUpsertContract, got, want)
	}
}
