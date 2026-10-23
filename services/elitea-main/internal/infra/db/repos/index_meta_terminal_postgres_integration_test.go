package repos

import (
	"context"
	"sync"
	"testing"
	"time"

	indexingapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/indexing"
)

// TestPostgresCurrentIndexMetaTerminalClaimRecovery crosses the real source
// discovery CTE, SKIP LOCKED replica exclusion, durable lease expiry and
// reclaim. The registry terminal write is covered by
// index_registry_postgres_integration_test.go.
func TestPostgresCurrentIndexMetaTerminalClaimRecovery(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)

	policy := IndexIngestDispatchPolicy{
		StreamName:        "elitea:runtime:index:commands",
		CapabilityVersion: "1",
		ResourceClass:     "indexing",
		IsolationClass:    "project",
		Priority:          1,
		DeadlineTTL:       time.Hour,
		LimitsRevision:    "index-limits-v1",
		MaxOutstanding:    1,
	}
	jobs, err := NewIndexIngestJobsRepository(pool, policy)
	if err != nil {
		t.Fatal(err)
	}
	admitted, err := newPostgresIndexAdmissionService(
		t,
		jobs,
		"terminal-claim",
	).Submit(
		context.Background(),
		postgresIndexSubmitRequest("terminal-claim-request", "fail"),
	)
	if err != nil || !admitted.Created {
		t.Fatalf("admit index execution: outcome=%+v err=%v", admitted, err)
	}
	if _, err := jobs.MarkIndexMetaInitialized(
		context.Background(),
		indexingapp.IndexMetaInitialization{
			ExecutionID:     admitted.ExecutionID,
			Generation:      admitted.Generation,
			IndexGeneration: admitted.IndexGeneration,
			MetaID:          admitted.IndexMetaID,
			CorrelationID:   admitted.IndexMetaCorrelationID,
		},
	); err != nil {
		t.Fatal(err)
	}

	indexResults, err := NewIndexIngestResultsRepository(pool, IndexIngestOutputPolicy{
		LimitsRevision:    policy.LimitsRevision,
		ArtifactMediaType: "application/json",
		MaxArtifactBytes:  1024 * 1024,
	})
	if err != nil {
		t.Fatal(err)
	}
	expected, err := indexResults.ExpectedIndexIngest(
		context.Background(),
		admitted.ExecutionID,
		admitted.Generation,
	)
	if err != nil {
		t.Fatal(err)
	}
	fence := claimPostgresIndexExecution(t, pool, expected)
	frame := postgresIndexRuntimeFailureFrame(t, expected, fence)
	if _, err := newPostgresRuntimeFailureService(t, pool).IngestFailure(
		context.Background(),
		frame,
	); err != nil {
		t.Fatal(err)
	}

	repository, err := NewCurrentIndexMetaTerminalBindingsRepository(pool)
	if err != nil {
		t.Fatal(err)
	}
	start := make(chan struct{})
	results := make(chan []indexingapp.CurrentIndexMetaTerminalClaim, 2)
	errors := make(chan error, 2)
	var workers sync.WaitGroup
	for _, token := range []string{"replica-1", "replica-2"} {
		workers.Add(1)
		go func() {
			defer workers.Done()
			<-start
			claims, claimErr := repository.ClaimPendingTerminalEffects(
				context.Background(),
				token,
				1,
				time.Minute,
			)
			results <- claims
			errors <- claimErr
		}()
	}
	close(start)
	workers.Wait()
	close(results)
	close(errors)
	for claimErr := range errors {
		if claimErr != nil {
			t.Fatal(claimErr)
		}
	}
	claimed := 0
	for claims := range results {
		claimed += len(claims)
	}
	if claimed != 1 {
		t.Fatalf("claimed=%d, want exactly one replica owner", claimed)
	}

	if _, err := pool.Exec(context.Background(), `
UPDATE elitea_runtime.index_ingest_jobs
SET index_meta_terminal_claim_expires_at = clock_timestamp() - interval '1 second'
WHERE execution_id = $1 AND generation = $2`,
		admitted.ExecutionID,
		int64(admitted.Generation),
	); err != nil {
		t.Fatal(err)
	}
	reclaimed, err := repository.ClaimPendingTerminalEffects(
		context.Background(),
		"replica-after-crash",
		1,
		time.Minute,
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(reclaimed) != 1 ||
		reclaimed[0].ExecutionID != admitted.ExecutionID ||
		reclaimed[0].ClaimToken != "replica-after-crash" {
		t.Fatalf("reclaimed=%+v", reclaimed)
	}
}
