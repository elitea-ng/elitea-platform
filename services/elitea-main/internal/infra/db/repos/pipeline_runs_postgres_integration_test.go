package repos

// Acceptance for PipelineRunsRepo (migrations/shared/0124_pipeline_runs.sql)
// over a REAL Postgres database, using the package-wide migrated template
// (configuration_validation_postgres_integration_test.go's TestMain). Skips
// with no ELITEA_TEST_DATABASE_URL, same as every other
// _postgres_integration_test.go in this package.

import (
	"context"
	"testing"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/pipelineruns"
)

func TestPipelineRunsRepoRecordsAndClaimsExactlyOnce(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	ctx := context.Background()
	repo := NewPipelineRunsRepo(pool)

	run := pipelineruns.Run{
		ExecutionID:      "exec-repo-1",
		ProjectID:        "proj-repo-1",
		ApplicationID:    11,
		VersionID:        3,
		ConversationUUID: "conv-repo-1",
		Origin:           "Webhook",
	}
	if err := repo.RecordRunStart(ctx, run); err != nil {
		t.Fatalf("RecordRunStart: %v", err)
	}

	// A duplicate start (a retried admission upstream, say) must not fail —
	// ON CONFLICT DO NOTHING.
	if err := repo.RecordRunStart(ctx, run); err != nil {
		t.Fatalf("RecordRunStart (duplicate): %v", err)
	}

	if err := repo.RecordExecutionError(ctx, run.ExecutionID, "the tool call timed out"); err != nil {
		t.Fatalf("RecordExecutionError: %v", err)
	}

	claimedRun, startedAt, errorSummary, found, err := repo.ClaimForEvent(ctx, run.ExecutionID)
	if err != nil {
		t.Fatalf("ClaimForEvent: %v", err)
	}
	if !found {
		t.Fatal("ClaimForEvent did not find the recorded row")
	}
	if claimedRun != run {
		t.Errorf("claimed run = %+v, want %+v", claimedRun, run)
	}
	if errorSummary != "the tool call timed out" {
		t.Errorf("error_summary = %q", errorSummary)
	}
	// A wide tolerance rather than "close to time.Now()": the test host and
	// the database container do not necessarily share a clock, and this
	// assertion only needs to catch a genuinely broken value (the zero time,
	// or a column that silently didn't populate), not measure clock skew.
	if startedAt.IsZero() || startedAt.Year() < 2020 {
		t.Errorf("started_at = %v, not a plausible timestamp", startedAt)
	}

	// The exactly-once contract: a second claim of the SAME execution id
	// finds nothing, even though the row still exists.
	_, _, _, foundAgain, err := repo.ClaimForEvent(ctx, run.ExecutionID)
	if err != nil {
		t.Fatalf("ClaimForEvent (second): %v", err)
	}
	if foundAgain {
		t.Fatal("ClaimForEvent claimed the same execution id twice")
	}
}

func TestPipelineRunsRepoClaimForEventFindsNothingForAnUntrackedExecution(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	repo := NewPipelineRunsRepo(pool)

	_, _, _, found, err := repo.ClaimForEvent(context.Background(), "never-recorded")
	if err != nil {
		t.Fatalf("ClaimForEvent: %v", err)
	}
	if found {
		t.Fatal("ClaimForEvent found a row for an execution id nobody recorded")
	}
}

// TestPipelineRunsRepoRecordExecutionErrorIsANoOpForAnUntrackedExecution
// proves the Tracker interface's documented contract: setting error_summary
// for an execution id that names no row is not an error — the caller
// (output.RuntimeFailureService's observer) has no way to know in advance
// whether an arbitrary failing execution is a tracked pipeline run.
func TestPipelineRunsRepoRecordExecutionErrorIsANoOpForAnUntrackedExecution(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	repo := NewPipelineRunsRepo(pool)

	if err := repo.RecordExecutionError(context.Background(), "never-recorded", "boom"); err != nil {
		t.Fatalf("RecordExecutionError on an untracked execution returned an error: %v", err)
	}
}

// TestPipelineRunsRepoConcurrentClaimsSeeExactlyOneWinner drives ten
// goroutines at the same execution id's claim — the real race the
// AfterSettle hook is exposed to if a settlement is ever retried while a
// previous attempt's hook goroutine is still in flight.
func TestPipelineRunsRepoConcurrentClaimsSeeExactlyOneWinner(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	ctx := context.Background()
	repo := NewPipelineRunsRepo(pool)

	run := pipelineruns.Run{ExecutionID: "exec-repo-race", ProjectID: "proj-repo-race", ApplicationID: 1, VersionID: 1, ConversationUUID: "conv-race", Origin: "Webhook"}
	if err := repo.RecordRunStart(ctx, run); err != nil {
		t.Fatalf("RecordRunStart: %v", err)
	}

	const attempts = 10
	results := make(chan bool, attempts)
	for i := 0; i < attempts; i++ {
		go func() {
			_, _, _, found, err := repo.ClaimForEvent(ctx, run.ExecutionID)
			if err != nil {
				t.Errorf("ClaimForEvent: %v", err)
				results <- false
				return
			}
			results <- found
		}()
	}
	winners := 0
	for i := 0; i < attempts; i++ {
		if <-results {
			winners++
		}
	}
	if winners != 1 {
		t.Fatalf("winners = %d, want exactly 1", winners)
	}
}
