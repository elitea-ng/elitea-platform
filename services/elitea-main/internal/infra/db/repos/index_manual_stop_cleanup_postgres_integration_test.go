package repos

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	indexingapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/indexing"
)

// TestPostgresServiceBackedCurrentIndexManualStopCleanupIntent crosses real
// cancellation, migration constraints, terminal-readiness gates, lease
// reclaim, and resolution. The registry effect, which deletes no vectors, is
// covered by indexing's TestRegistryManualStopCleanerKeepsTheIndexVectorsAndOnlyVerifiesTheStop.
func TestPostgresServiceBackedCurrentIndexManualStopCleanupIntent(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	policy := IndexIngestDispatchPolicy{
		StreamName:        "elitea:runtime:index:commands",
		CapabilityVersion: "1",
		ResourceClass:     "indexing",
		IsolationClass:    "project",
		Priority:          1,
		DeadlineTTL:       time.Hour,
		LimitsRevision:    "index-limits-v1",
		MaxOutstanding:    8,
	}
	jobs, err := NewIndexIngestJobsRepository(pool, policy)
	if err != nil {
		t.Fatal(err)
	}
	cancellations, err := NewCurrentIndexCancellationRepository(pool)
	if err != nil {
		t.Fatal(err)
	}
	cleanups, err := NewCurrentIndexManualStopCleanupRepository(pool)
	if err != nil {
		t.Fatal(err)
	}

	manual := admitManualStopPostgresExecution(
		t,
		jobs,
		"0123456789abcdef0123456789abcdef",
		"manual-stop",
		"Docs",
	)
	if _, err := jobs.MarkIndexMetaInitialized(
		ctx,
		indexingapp.IndexMetaInitialization{
			ExecutionID:     manual.ExecutionID,
			Generation:      manual.Generation,
			IndexGeneration: manual.IndexGeneration,
			MetaID:          manual.IndexMetaID,
			CorrelationID:   manual.IndexMetaCorrelationID,
		},
	); err != nil {
		t.Fatal(err)
	}
	request := indexingapp.CurrentIndexCancelRequest{
		ProjectID:   1,
		ToolkitID:   19,
		IndexName:   "Docs",
		ExecutionID: manual.ExecutionID,
	}
	transitioned, err := cancellations.RequestCurrentIndexCancellation(ctx, request)
	if err != nil || !transitioned {
		t.Fatalf("manual Stop transitioned=%v err=%v", transitioned, err)
	}
	transitioned, err = cancellations.RequestCurrentIndexCancellation(ctx, request)
	if err != nil || transitioned {
		t.Fatalf("manual Stop retry transitioned=%v err=%v", transitioned, err)
	}
	assertPostgresCount(t, ctx, pool, 1, `
SELECT count(*)
FROM elitea_runtime.execution_jobs AS j
JOIN elitea_runtime.index_ingest_jobs AS i
  ON i.execution_id = j.execution_id
 AND i.generation = j.generation
WHERE j.execution_id = $1
  AND j.desired_state = 'CANCELLED'
  AND i.index_manual_stop_requested_at IS NOT NULL
  AND i.index_manual_cleanup_status = 'PENDING'
  AND i.index_manual_cleanup_attempt_count = 0
  AND i.index_manual_cleanup_claim_token IS NULL`,
		manual.ExecutionID,
	)

	claims, err := cleanups.ClaimPendingManualStopCleanups(
		ctx,
		"claim-before-settlement",
		1,
		time.Minute,
	)
	if err != nil || len(claims) != 0 {
		t.Fatalf("pre-settlement claims=%+v err=%v", claims, err)
	}
	if _, err := pool.Exec(ctx, `
UPDATE elitea_runtime.execution_jobs
SET state = 'CANCELLED',
    settled_at = clock_timestamp()
WHERE execution_id = $1
  AND generation = $2`,
		manual.ExecutionID,
		int64(manual.Generation),
	); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
UPDATE elitea_runtime.index_ingest_jobs
SET index_meta_terminal_state = 'cancelled',
    index_meta_terminal_occurred_at = clock_timestamp(),
    index_meta_terminal_status = 'APPLIED',
    index_meta_terminal_attempt_count = 1,
    index_meta_terminal_next_attempt_at = NULL,
    index_meta_terminalized_at = clock_timestamp()
WHERE execution_id = $1
  AND generation = $2`,
		manual.ExecutionID,
		int64(manual.Generation),
	); err != nil {
		t.Fatal(err)
	}
	claims, err = cleanups.ClaimPendingManualStopCleanups(
		ctx,
		"claim-1",
		1,
		time.Minute,
	)
	if err != nil || len(claims) != 1 ||
		claims[0].ExecutionID != manual.ExecutionID {
		t.Fatalf("terminal-ready claims=%+v err=%v", claims, err)
	}
	replicaClaims, err := cleanups.ClaimPendingManualStopCleanups(
		ctx,
		"claim-replica",
		1,
		time.Minute,
	)
	if err != nil || len(replicaClaims) != 0 {
		t.Fatalf("concurrent replica claims=%+v err=%v", replicaClaims, err)
	}
	if _, err := pool.Exec(ctx, `
UPDATE elitea_runtime.index_ingest_jobs
SET index_manual_cleanup_claim_expires_at =
        clock_timestamp() - interval '1 second'
WHERE execution_id = $1
  AND generation = $2`,
		manual.ExecutionID,
		int64(manual.Generation),
	); err != nil {
		t.Fatal(err)
	}
	reclaimed, err := cleanups.ClaimPendingManualStopCleanups(
		ctx,
		"claim-2",
		1,
		time.Minute,
	)
	if err != nil || len(reclaimed) != 1 {
		t.Fatalf("reclaimed claims=%+v err=%v", reclaimed, err)
	}
	if err := cleanups.ResolveManualStopCleanup(
		ctx,
		reclaimed[0],
		indexingapp.CurrentManualStopCleanupApplied,
	); err != nil {
		t.Fatal(err)
	}
	assertPostgresCount(t, ctx, pool, 1, `
SELECT count(*)
FROM elitea_runtime.index_ingest_jobs
WHERE execution_id = $1
  AND index_manual_cleanup_status = 'APPLIED'
  AND index_manual_cleanup_attempt_count = 2
  AND index_manual_cleanup_claim_token IS NULL
  AND index_manual_cleanup_resolved_at IS NOT NULL`,
		manual.ExecutionID,
	)

	system := admitManualStopPostgresExecution(
		t,
		jobs,
		"1123456789abcdef0123456789abcdef",
		"system-stop",
		"SystemDocs",
	)
	if _, err := jobs.MarkIndexMetaInitialized(
		ctx,
		indexingapp.IndexMetaInitialization{
			ExecutionID:     system.ExecutionID,
			Generation:      system.Generation,
			IndexGeneration: system.IndexGeneration,
			MetaID:          system.IndexMetaID,
			CorrelationID:   system.IndexMetaCorrelationID,
		},
	); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
UPDATE elitea_runtime.execution_jobs
SET desired_state = 'CANCELLED'
WHERE execution_id = $1
  AND generation = $2`,
		system.ExecutionID,
		int64(system.Generation),
	); err != nil {
		t.Fatal(err)
	}
	assertPostgresCount(t, ctx, pool, 1, `
SELECT count(*)
FROM elitea_runtime.index_ingest_jobs
WHERE execution_id = $1
  AND index_manual_stop_requested_at IS NULL
  AND index_manual_cleanup_status IS NULL`,
		system.ExecutionID,
	)

	beforeInit := admitManualStopPostgresExecution(
		t,
		jobs,
		"2123456789abcdef0123456789abcdef",
		"before-init-stop",
		"BeforeInit",
	)
	transitioned, err = cancellations.RequestCurrentIndexCancellation(
		ctx,
		indexingapp.CurrentIndexCancelRequest{
			ProjectID:   1,
			ToolkitID:   19,
			IndexName:   "BeforeInit",
			ExecutionID: beforeInit.ExecutionID,
		},
	)
	if err != nil || !transitioned {
		t.Fatalf(
			"pre-initialization Stop transitioned=%v err=%v",
			transitioned,
			err,
		)
	}
	assertPostgresCount(t, ctx, pool, 1, `
SELECT count(*)
FROM elitea_runtime.execution_jobs AS j
JOIN elitea_runtime.index_ingest_jobs AS i
  ON i.execution_id = j.execution_id
 AND i.generation = j.generation
WHERE j.execution_id = $1
  AND j.desired_state = 'CANCELLED'
  AND i.index_meta_initialized_at IS NULL
  AND i.index_manual_cleanup_status IS NULL`,
		beforeInit.ExecutionID,
	)

	stale := admitManualStopPostgresExecution(
		t,
		jobs,
		"3123456789abcdef0123456789abcdef",
		"stale-manual-stop",
		"StaleDocs",
	)
	if _, err := jobs.MarkIndexMetaInitialized(
		ctx,
		indexingapp.IndexMetaInitialization{
			ExecutionID:     stale.ExecutionID,
			Generation:      stale.Generation,
			IndexGeneration: stale.IndexGeneration,
			MetaID:          stale.IndexMetaID,
			CorrelationID:   stale.IndexMetaCorrelationID,
		},
	); err != nil {
		t.Fatal(err)
	}
	transitioned, err = cancellations.RequestCurrentIndexCancellation(
		ctx,
		indexingapp.CurrentIndexCancelRequest{
			ProjectID:   1,
			ToolkitID:   19,
			IndexName:   "StaleDocs",
			ExecutionID: stale.ExecutionID,
		},
	)
	if err != nil || !transitioned {
		t.Fatalf("stale Stop transitioned=%v err=%v", transitioned, err)
	}
	if _, err := pool.Exec(ctx, `
UPDATE elitea_runtime.execution_jobs
SET state = 'CANCELLED',
    settled_at = clock_timestamp()
WHERE execution_id = $1
  AND generation = $2`,
		stale.ExecutionID,
		int64(stale.Generation),
	); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
UPDATE elitea_runtime.index_ingest_jobs
SET index_meta_terminal_state = 'cancelled',
    index_meta_terminal_occurred_at = clock_timestamp(),
    index_meta_terminal_status = 'APPLIED',
    index_meta_terminal_attempt_count = 1,
    index_meta_terminal_next_attempt_at = NULL,
    index_meta_terminalized_at = clock_timestamp()
WHERE execution_id = $1
  AND generation = $2`,
		stale.ExecutionID,
		int64(stale.Generation),
	); err != nil {
		t.Fatal(err)
	}
	newer := admitManualStopPostgresExecution(
		t,
		jobs,
		"4123456789abcdef0123456789abcdef",
		"newer-index",
		"StaleDocs",
	)
	if newer.IndexGeneration <= stale.IndexGeneration {
		t.Fatalf(
			"newer index generation=%d stale=%d",
			newer.IndexGeneration,
			stale.IndexGeneration,
		)
	}
	if _, err := jobs.MarkIndexMetaInitialized(
		ctx,
		indexingapp.IndexMetaInitialization{
			ExecutionID:     newer.ExecutionID,
			Generation:      newer.Generation,
			IndexGeneration: newer.IndexGeneration,
			MetaID:          newer.IndexMetaID,
			CorrelationID:   newer.IndexMetaCorrelationID,
		},
	); err != nil {
		t.Fatal(err)
	}
	staleClaims, err := cleanups.ClaimPendingManualStopCleanups(
		ctx,
		"claim-stale",
		1,
		time.Minute,
	)
	if err != nil || len(staleClaims) != 1 ||
		staleClaims[0].ExecutionID != stale.ExecutionID {
		t.Fatalf("stale claims=%+v err=%v", staleClaims, err)
	}
	superseded, err := cleanups.SupersedeManualStopCleanupIfNewerInitialized(
		ctx,
		staleClaims[0],
	)
	if err != nil || !superseded {
		t.Fatalf("stale superseded=%v err=%v", superseded, err)
	}
	assertPostgresCount(t, ctx, pool, 1, `
SELECT count(*)
FROM elitea_runtime.index_ingest_jobs
WHERE execution_id = $1
  AND index_manual_cleanup_status = 'SUPERSEDED'
  AND index_manual_cleanup_claim_token IS NULL
  AND index_manual_cleanup_resolved_at IS NOT NULL`,
		stale.ExecutionID,
	)
}

func admitManualStopPostgresExecution(
	t *testing.T,
	jobs *IndexIngestJobsRepository,
	executionID string,
	idempotencyKey string,
	indexName string,
) indexingapp.AdmissionOutcome {
	t.Helper()
	factory, err := indexingapp.NewInputBundleFactory(
		indexingapp.InputProfile{
			Classification:        "project-confidential",
			RequiredGrantAudience: "elitea.runtime.input.read.v1",
		},
		postgresIndexIDs(
			idempotencyKey+"-bundle",
			idempotencyKey+"-toolkit-content",
			idempotencyKey+"-parameters-content",
		),
	)
	if err != nil {
		t.Fatal(err)
	}
	service, err := indexingapp.NewAdmissionService(
		jobs,
		factory,
		time.Now,
		postgresIndexIDs(
			executionID,
			idempotencyKey+"-command",
			idempotencyKey+"-outbox",
			idempotencyKey+"-index-meta",
		),
	)
	if err != nil {
		t.Fatal(err)
	}
	request := postgresIndexSubmitRequest(idempotencyKey, indexName)
	request.Identity.TenantID = "1"
	request.Inputs.ToolkitConfiguration = json.RawMessage(
		`{"id":19,"type":"confluence","settings":{"token":"secret-ref://toolkit/19"}}`,
	)
	outcome, err := service.Submit(context.Background(), request)
	if err != nil || !outcome.Created {
		t.Fatalf("admit execution: outcome=%+v err=%v", outcome, err)
	}
	return outcome
}
