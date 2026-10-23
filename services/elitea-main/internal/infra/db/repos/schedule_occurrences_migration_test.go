package repos

import (
	"strings"
	"testing"

	platformmigrations "github.com/elitea-ng/elitea-platform/services/elitea-main/migrations"
)

func TestScheduledOccurrenceMigrationOwnsFencedBoundedLedger(t *testing.T) {
	raw, err := platformmigrations.Files.ReadFile("shared/0053_scheduled_occurrence_kernel.sql")
	if err != nil {
		t.Fatal(err)
	}
	sql := string(raw)
	required := []string{
		"scheduled_job_cursors",
		"scheduled_occurrences",
		"UNIQUE (job_id, schedule_revision, due_at)",
		"lease_epoch",
		"claim_fence",
		"lease_expires_at",
		"next_attempt_at",
		"attempt_count",
		"scheduled_occurrences_claimable_idx",
		"WHERE state = 'PENDING'",
		"local_bounded",
		"durable_admission",
	}
	for _, fragment := range required {
		if !strings.Contains(sql, fragment) {
			t.Fatalf("scheduled occurrence migration missing %q", fragment)
		}
	}
	if strings.Contains(strings.ToUpper(sql), "OFFSET") {
		t.Fatal("scheduled occurrence migration must not create an offset-based work scan")
	}
	if strings.Contains(strings.ToUpper(sql), "IF NOT EXISTS") {
		t.Fatal("versioned scheduled occurrence migration must fail loudly on schema drift")
	}
}
