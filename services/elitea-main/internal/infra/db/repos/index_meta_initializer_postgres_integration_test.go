package repos

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"testing"
	"time"

	indexingapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/indexing"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/pgvector"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type postgresFrozenToolkitClaimer struct {
	materialized json.RawMessage
}

func (c postgresFrozenToolkitClaimer) ClaimFrozenToolkitConfiguration(
	_ context.Context,
	_ indexingapp.FrozenToolkitConfigurationClaim,
) (json.RawMessage, error) {
	return append(json.RawMessage(nil), c.materialized...), nil
}

// TestPostgresPgvectorIndexMetaInitializationConvergence crosses the runtime
// PostgreSQL transaction, the actual pgx PgVector writer in a separate
// transaction/connection, the idempotent recovery seam, and every dispatch
// gate. The two logical stores share the isolated test database server; this is
// not a multi-cluster, command-bus, worker, TLS, load, or browser E2E.
func TestPostgresPgvectorIndexMetaInitializationConvergence(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	var vectorAvailable bool
	if err := pool.QueryRow(ctx, `SELECT EXISTS (
SELECT 1 FROM pg_catalog.pg_available_extensions WHERE name = 'vector'
)`).Scan(&vectorAvailable); err != nil {
		t.Fatal(err)
	}
	if !vectorAvailable {
		t.Skip("the PostgreSQL integration server does not provide the vector extension")
	}
	if _, err := pool.Exec(ctx, `CREATE EXTENSION IF NOT EXISTS vector`); err != nil {
		t.Skipf("the isolated integration database cannot install vector: %v", err)
	}

	policy := IndexIngestDispatchPolicy{
		StreamName:        "elitea:runtime:index:commands",
		CapabilityVersion: "1",
		ResourceClass:     "indexing",
		IsolationClass:    "project",
		Priority:          1,
		DeadlineTTL:       time.Hour,
		LimitsRevision:    "index-limits-v1",
		MaxOutstanding:    16,
	}
	jobs, err := NewIndexIngestJobsRepository(pool, policy)
	if err != nil {
		t.Fatal(err)
	}
	outbox, err := NewCommandOutboxRepository(pool, policy.StreamName)
	if err != nil {
		t.Fatal(err)
	}

	toolkitID := int32(1_500_000_000 + time.Now().UnixNano()%500_000_000)
	databaseURL := postgresIntegrationPoolURL(t, pool.Config().ConnString(), pool.Config().ConnConfig.Database)
	claimed, err := json.Marshal(map[string]any{
		"id":   toolkitID,
		"type": "confluence",
		"settings": map[string]any{
			"pgvector_configuration": map[string]any{
				"configuration_type":       "pgvector",
				"configuration_project_id": 1,
				"configuration_uuid":       "integration-pgvector",
				"connection_string":        databaseURL,
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	initializer, err := indexingapp.NewCurrentIndexMetaInitializer(
		postgresFrozenToolkitClaimer{materialized: claimed},
		pgvector.NewCurrentIndexMetaWriter(),
	)
	if err != nil {
		t.Fatal(err)
	}

	request := postgresIndexMetaConvergenceRequest(toolkitID, "crash-recovery", "Docs")
	firstAdmissions := newPostgresIndexAdmissionService(t, jobs, "meta-convergence-first")
	admitted, err := firstAdmissions.Submit(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	if err := initializer.MaterializeInitialIndexMeta(ctx, request, admitted); err != nil {
		t.Fatalf("commit external metadata before simulated crash: %v", err)
	}
	pending, err := outbox.ListPendingIndexIngestIDs(ctx, 16, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if containsAdmissionID(pending, "meta-convergence-first-outbox") {
		t.Fatalf("external commit alone opened the dispatch gate: %v", pending)
	}
	assertPostgresPgvectorMeta(t, ctx, pool, toolkitID, "Docs", admitted.IndexMetaID, admitted.ExecutionID, 1)

	replayAdmissions := newPostgresIndexAdmissionService(t, jobs, "meta-convergence-replay")
	recovering, err := indexingapp.NewInitializingAdmissionSubmitter(
		replayAdmissions,
		newPostgresDurableIndexMetaInitializer(
			t,
			jobs,
			initializer,
			"recovery",
		),
	)
	if err != nil {
		t.Fatal(err)
	}
	recovered, err := recovering.Submit(ctx, request)
	if err != nil {
		t.Fatalf("recover cross-store commit: %v", err)
	}
	if recovered.Created || recovered.ExecutionID != admitted.ExecutionID ||
		recovered.IndexMetaID != admitted.IndexMetaID ||
		recovered.IndexMetaInitializedAt == nil || recovered.IndexMetaInitializedAt.IsZero() {
		t.Fatalf("recovered outcome=%+v admitted=%+v", recovered, admitted)
	}
	assertPostgresPgvectorMeta(t, ctx, pool, toolkitID, "Docs", admitted.IndexMetaID, admitted.ExecutionID, 1)
	pending, err = outbox.ListPendingIndexIngestIDs(ctx, 16, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if !containsAdmissionID(pending, "meta-convergence-first-outbox") {
		t.Fatalf("recovery did not open the exact dispatch gate: %v", pending)
	}

	backgroundRequest := postgresIndexMetaConvergenceRequest(
		toolkitID,
		"background-recovery",
		"BackgroundDocs",
	)
	backgroundAdmission, err := newPostgresIndexAdmissionService(
		t,
		jobs,
		"meta-convergence-background",
	).Submit(ctx, backgroundRequest)
	if err != nil {
		t.Fatal(err)
	}
	backgroundInitializer := newPostgresDurableIndexMetaInitializer(
		t,
		jobs,
		initializer,
		"background-claim",
	)
	processed, err := backgroundInitializer.Reconcile(ctx)
	if err != nil || processed != 1 {
		t.Fatalf("background recovery processed=%d error=%v", processed, err)
	}
	assertPostgresPgvectorMeta(
		t,
		ctx,
		pool,
		toolkitID,
		"BackgroundDocs",
		backgroundAdmission.IndexMetaID,
		backgroundAdmission.ExecutionID,
		1,
	)
	pending, err = outbox.ListPendingIndexIngestIDs(ctx, 16, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if !containsAdmissionID(
		pending,
		"meta-convergence-background-outbox",
	) {
		t.Fatalf(
			"background recovery did not open the exact dispatch gate: %v",
			pending,
		)
	}

	corruptRequest := postgresIndexMetaConvergenceRequest(
		toolkitID,
		"corrupt-frozen-intent",
		"CorruptFrozenDocs",
	)
	corruptAdmission, err := newPostgresIndexAdmissionService(
		t,
		jobs,
		"meta-convergence-corrupt",
	).Submit(ctx, corruptRequest)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
UPDATE elitea_runtime.input_bundle_entries AS e
SET semantic_role = 'corrupt.required.entry'
FROM elitea_runtime.index_ingest_jobs AS i
WHERE i.execution_id = $1
  AND i.generation = $2
  AND e.input_bundle_id = i.input_bundle_id
  AND e.entry_id = i.tool_parameters_entry_id`,
		corruptAdmission.ExecutionID,
		int64(corruptAdmission.Generation),
	); err != nil {
		t.Fatal(err)
	}
	_, err = newPostgresDurableIndexMetaInitializer(
		t,
		jobs,
		initializer,
		"corrupt-intent-claim",
	).Initialize(ctx, corruptAdmission)
	if !errors.Is(err, indexingapp.ErrIndexMetaInitializationMismatch) {
		t.Fatalf("corrupt frozen intent initialization=%v", err)
	}
	assertPostgresCount(t, ctx, pool, 1, `
SELECT count(*)
FROM elitea_runtime.execution_jobs AS j
JOIN elitea_runtime.index_ingest_jobs AS i
  ON i.execution_id = j.execution_id
 AND i.generation = j.generation
JOIN elitea_runtime.command_outbox AS o
  ON o.execution_id = j.execution_id
 AND o.generation = j.generation
WHERE j.execution_id = $1
  AND j.state = 'QUARANTINED'
  AND j.desired_state = 'CANCELLED'
  AND i.index_meta_initialization_status = 'QUARANTINED'
  AND i.index_meta_initialization_last_error_code =
      'INITIALIZATION_INTENT_INVALID'
  AND o.retired_at IS NOT NULL
  AND o.retirement_code = 'CANCELLED'
  AND o.prepared_at IS NULL
  AND o.published_at IS NULL
  AND o.authority_granted_at IS NULL`,
		corruptAdmission.ExecutionID,
	)
	var failureEventID, failureEventType string
	var failureEventBytes, failureEventDigest []byte
	if err := pool.QueryRow(ctx, `
SELECT event_id, event_type, event_bytes, event_digest
FROM elitea_runtime.execution_replay_events
WHERE execution_id = $1 AND generation = $2`,
		corruptAdmission.ExecutionID,
		int64(corruptAdmission.Generation),
	).Scan(
		&failureEventID,
		&failureEventType,
		&failureEventBytes,
		&failureEventDigest,
	); err != nil {
		t.Fatal(err)
	}
	if failureEventID !=
		"index-meta-initialization-quarantine:meta-convergence-corrupt-outbox" ||
		failureEventType != replayEventRuntimeFailure ||
		!bytes.Equal(
			failureEventBytes,
			indexMetaInitializationFailureEventBytes,
		) ||
		!bytes.Equal(
			failureEventDigest,
			indexMetaInitializationFailureEventDigest[:],
		) {
		t.Fatalf(
			"unsafe failure replay id=%q type=%q bytes=%s digest=%x",
			failureEventID,
			failureEventType,
			failureEventBytes,
			failureEventDigest,
		)
	}
	replay, err := NewReplayEventsRepository(pool)
	if err != nil {
		t.Fatal(err)
	}
	events, err := replay.Replay(
		ctx,
		corruptRequest.Identity.ProjectionProjectID,
		corruptAdmission.ExecutionID,
		0,
		10,
	)
	if err != nil || len(events) != 1 ||
		events[0].Type != replayEventRuntimeFailure ||
		!bytes.Equal(
			events[0].Data,
			indexMetaInitializationFailureEventBytes,
		) {
		t.Fatalf("quarantine replay events=%+v error=%v", events, err)
	}
	assertPostgresCount(t, ctx, pool, 0, `
SELECT count(*)
FROM elitea_runtime.output_inbox
WHERE execution_id = $1`, corruptAdmission.ExecutionID)
	assertPostgresCount(t, ctx, pool, 0, `
SELECT count(*)
FROM elitea_runtime.execution_settlements
WHERE execution_id = $1`, corruptAdmission.ExecutionID)
	if err := jobs.QuarantineIndexMetaInitialization(
		ctx,
		indexingapp.IndexMetaInitializationClaim{
			ExecutionID: corruptAdmission.ExecutionID,
			Generation:  corruptAdmission.Generation,
			ClaimToken:  "corrupt-intent-claim",
			Attempt:     1,
			ExpiresAt:   time.Now().UTC().Add(time.Minute),
		},
		"INITIALIZATION_INTENT_INVALID",
	); !errors.Is(err, indexingapp.ErrIndexMetaInitializationMismatch) {
		t.Fatalf("replayed quarantine error=%v", err)
	}
	assertPostgresCount(t, ctx, pool, 1, `
SELECT count(*)
FROM elitea_runtime.execution_replay_events
WHERE execution_id = $1`, corruptAdmission.ExecutionID)

	collisionRequest := postgresIndexMetaConvergenceRequest(
		toolkitID,
		"quarantine-event-collision",
		"CollisionDocs",
	)
	collisionAdmission, err := newPostgresIndexAdmissionService(
		t,
		jobs,
		"meta-convergence-collision",
	).Submit(ctx, collisionRequest)
	if err != nil {
		t.Fatal(err)
	}
	collisionClaim, err := jobs.ClaimExactIndexMetaInitialization(
		ctx,
		indexingapp.IndexMetaInitialization{
			ExecutionID:     collisionAdmission.ExecutionID,
			Generation:      collisionAdmission.Generation,
			IndexGeneration: collisionAdmission.IndexGeneration,
			MetaID:          collisionAdmission.IndexMetaID,
			CorrelationID:   collisionAdmission.IndexMetaCorrelationID,
		},
		"collision-claim",
		time.Minute,
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
INSERT INTO elitea_runtime.execution_replay_events (
    event_id, execution_id, generation, projection_project_id,
    event_type, event_bytes, event_digest
) VALUES ($1, $2, $3, $4, 'execution.node_event', $5, $6)`,
		"index-meta-initialization-quarantine:"+
			"meta-convergence-collision-outbox",
		collisionAdmission.ExecutionID,
		int64(collisionAdmission.Generation),
		int32(1),
		indexMetaInitializationFailureEventBytes,
		indexMetaInitializationFailureEventDigest[:],
	); err != nil {
		t.Fatal(err)
	}
	if err := jobs.QuarantineIndexMetaInitialization(
		ctx,
		collisionClaim,
		"INITIALIZATION_INTENT_INVALID",
	); err == nil {
		t.Fatal("quarantine replay collision did not roll back")
	}
	assertPostgresCount(t, ctx, pool, 1, `
SELECT count(*)
FROM elitea_runtime.execution_jobs AS j
JOIN elitea_runtime.index_ingest_jobs AS i
  ON i.execution_id = j.execution_id
 AND i.generation = j.generation
JOIN elitea_runtime.command_outbox AS o
  ON o.execution_id = j.execution_id
 AND o.generation = j.generation
WHERE j.execution_id = $1
  AND j.state = 'PENDING'
  AND j.desired_state = 'RUNNING'
  AND i.index_meta_initialization_status = 'RUNNING'
  AND i.index_meta_initialization_claim_token = 'collision-claim'
  AND o.retired_at IS NULL
  AND o.retirement_code IS NULL`,
		collisionAdmission.ExecutionID,
	)

	leaseRequest := postgresIndexMetaConvergenceRequest(
		toolkitID,
		"lease-race",
		"LeaseDocs",
	)
	leaseAdmission, err := newPostgresIndexAdmissionService(
		t,
		jobs,
		"meta-convergence-lease",
	).Submit(ctx, leaseRequest)
	if err != nil {
		t.Fatal(err)
	}
	type claimResult struct {
		claims []indexingapp.IndexMetaInitializationClaim
		err    error
	}
	startClaims := make(chan struct{})
	claimResults := make(chan claimResult, 2)
	for _, token := range []string{"replica-a", "replica-b"} {
		token := token
		go func() {
			<-startClaims
			claims, claimErr := jobs.ClaimPendingIndexMetaInitializations(
				ctx,
				token,
				16,
				time.Minute,
			)
			claimResults <- claimResult{claims: claims, err: claimErr}
		}()
	}
	close(startClaims)
	var winningClaim indexingapp.IndexMetaInitializationClaim
	totalClaims := 0
	for range 2 {
		result := <-claimResults
		if result.err != nil {
			t.Fatal(result.err)
		}
		totalClaims += len(result.claims)
		if len(result.claims) == 1 {
			winningClaim = result.claims[0]
		}
	}
	if totalClaims != 1 ||
		winningClaim.ExecutionID != leaseAdmission.ExecutionID {
		t.Fatalf(
			"multi-replica claims=%d winning=%+v admission=%+v",
			totalClaims,
			winningClaim,
			leaseAdmission,
		)
	}
	work, err := jobs.LoadIndexMetaInitializationWork(ctx, winningClaim)
	if err != nil || work.Request.ToolkitID != toolkitID ||
		string(work.Request.Inputs.ToolParameters) !=
			string(leaseRequest.Inputs.ToolParameters) {
		t.Fatalf("frozen work=%+v error=%v", work, err)
	}

	// Simulate the winning process dying with its lease. A later replica takes
	// ownership, permanently quarantines the exact pre-authority row, and the
	// target becomes admissible again without publishing to the command bus.
	if _, err := pool.Exec(ctx, `
UPDATE elitea_runtime.index_ingest_jobs
SET index_meta_initialization_claim_expires_at =
        clock_timestamp() - interval '1 second'
WHERE execution_id = $1
  AND generation = $2`,
		winningClaim.ExecutionID,
		int64(winningClaim.Generation),
	); err != nil {
		t.Fatal(err)
	}
	reclaimed, err := jobs.ClaimExactIndexMetaInitialization(
		ctx,
		indexingapp.IndexMetaInitialization{
			ExecutionID:     leaseAdmission.ExecutionID,
			Generation:      leaseAdmission.Generation,
			IndexGeneration: leaseAdmission.IndexGeneration,
			MetaID:          leaseAdmission.IndexMetaID,
			CorrelationID:   leaseAdmission.IndexMetaCorrelationID,
		},
		"replica-c",
		time.Minute,
	)
	if err != nil || reclaimed.Attempt != 2 {
		t.Fatalf("reclaimed=%+v error=%v", reclaimed, err)
	}
	if err := jobs.QuarantineIndexMetaInitialization(
		ctx,
		reclaimed,
		"INITIALIZATION_EXTERNAL_CONFLICT",
	); err != nil {
		t.Fatal(err)
	}
	assertPostgresCount(t, ctx, pool, 1, `
SELECT count(*)
FROM elitea_runtime.execution_jobs AS j
JOIN elitea_runtime.index_ingest_jobs AS i
  ON i.execution_id = j.execution_id
 AND i.generation = j.generation
JOIN elitea_runtime.command_outbox AS o
  ON o.execution_id = j.execution_id
 AND o.generation = j.generation
WHERE j.execution_id = $1
  AND j.state = 'QUARANTINED'
  AND j.desired_state = 'CANCELLED'
  AND i.index_meta_initialization_status = 'QUARANTINED'
  AND i.index_meta_initialized_at IS NULL
  AND o.retired_at IS NOT NULL
  AND o.retirement_code = 'CANCELLED'
  AND o.prepared_at IS NULL
  AND o.published_at IS NULL
  AND o.authority_granted_at IS NULL`,
		leaseAdmission.ExecutionID,
	)
	replacement := postgresIndexMetaConvergenceRequest(
		toolkitID,
		"lease-replacement",
		"LeaseDocs",
	)
	replacementAdmission, err := newPostgresIndexAdmissionService(
		t,
		jobs,
		"meta-convergence-lease-replacement",
	).Submit(ctx, replacement)
	if err != nil || !replacementAdmission.Created {
		t.Fatalf(
			"quarantined target replacement=%+v error=%v",
			replacementAdmission,
			err,
		)
	}

	conflictRequest := postgresIndexMetaConvergenceRequest(toolkitID, "conflict", "Docs")
	conflictingAdmissions := newPostgresIndexAdmissionService(t, jobs, "meta-convergence-conflict")
	conflicting, err := indexingapp.NewInitializingAdmissionSubmitter(
		conflictingAdmissions,
		newPostgresDurableIndexMetaInitializer(
			t,
			jobs,
			initializer,
			"conflict",
		),
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conflicting.Submit(ctx, conflictRequest); !errors.Is(err, indexingapp.ErrCurrentIndexMetaConflict) {
		t.Fatalf("same-index conflicting start=%v", err)
	}
	assertPostgresCount(t, ctx, pool, 0, `
SELECT count(*)
FROM elitea_runtime.index_ingest_jobs
WHERE execution_id = 'meta-convergence-conflict-execution'
  AND index_meta_initialized_at IS NOT NULL`)
	pending, err = outbox.ListPendingIndexIngestIDs(ctx, 16, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if containsAdmissionID(pending, "meta-convergence-conflict-outbox") {
		t.Fatalf("conflicting metadata start became dispatchable: %v", pending)
	}

	cancelRequest := postgresIndexMetaConvergenceRequest(toolkitID, "cancel", "CancelledDocs")
	cancelAdmissions := newPostgresIndexAdmissionService(t, jobs, "meta-convergence-cancel")
	cancelled, err := cancelAdmissions.Submit(ctx, cancelRequest)
	if err != nil {
		t.Fatal(err)
	}
	if err := initializer.MaterializeInitialIndexMeta(ctx, cancelRequest, cancelled); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
UPDATE elitea_runtime.execution_jobs
SET desired_state = 'CANCELLED'
WHERE execution_id = $1 AND generation = $2`,
		cancelled.ExecutionID,
		int64(cancelled.Generation),
	); err != nil {
		t.Fatal(err)
	}
	cancelReplay := newPostgresIndexAdmissionService(t, jobs, "meta-convergence-cancel-replay")
	cancelSubmitter, err := indexingapp.NewInitializingAdmissionSubmitter(
		cancelReplay,
		newPostgresDurableIndexMetaInitializer(
			t,
			jobs,
			initializer,
			"cancel",
		),
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := cancelSubmitter.Submit(ctx, cancelRequest); !errors.Is(err, indexingapp.ErrIndexMetaInitializationMismatch) {
		t.Fatalf("cancelled recovery transition=%v", err)
	}
	assertPostgresCount(t, ctx, pool, 0, `
SELECT count(*)
FROM elitea_runtime.index_ingest_jobs
WHERE execution_id = $1
  AND index_meta_initialized_at IS NOT NULL`, cancelled.ExecutionID)
	pending, err = outbox.ListPendingIndexIngestIDs(ctx, 16, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if containsAdmissionID(pending, "meta-convergence-cancel-outbox") {
		t.Fatalf("cancelled metadata start became dispatchable: %v", pending)
	}
}

func TestPostgresIndexMetaInitializationQuarantineConvergesExpiredExecution(
	t *testing.T,
) {
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
		MaxOutstanding:    16,
	}
	jobs, err := NewIndexIngestJobsRepository(pool, policy)
	if err != nil {
		t.Fatal(err)
	}
	outbox, err := NewCommandOutboxRepository(pool, policy.StreamName)
	if err != nil {
		t.Fatal(err)
	}

	request := postgresIndexMetaConvergenceRequest(
		1_700_000_001,
		"initializer-deadline-race",
		"DeadlineRaceDocs",
	)
	admission, err := newPostgresIndexAdmissionService(
		t,
		jobs,
		"initializer-deadline-race",
	).Submit(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	claim, err := jobs.ClaimExactIndexMetaInitialization(
		ctx,
		indexingapp.IndexMetaInitialization{
			ExecutionID:     admission.ExecutionID,
			Generation:      admission.Generation,
			IndexGeneration: admission.IndexGeneration,
			MetaID:          admission.IndexMetaID,
			CorrelationID:   admission.IndexMetaCorrelationID,
		},
		"initializer-deadline-race-claim",
		time.Minute,
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
UPDATE elitea_runtime.command_outbox
SET deadline = clock_timestamp() - interval '1 second'
WHERE execution_id = $1 AND generation = $2`,
		admission.ExecutionID,
		int64(admission.Generation),
	); err != nil {
		t.Fatal(err)
	}
	retired, err := outbox.RetireNoAuthorityIndexIngest(ctx, 1)
	if err != nil || retired != 1 {
		t.Fatalf("deadline retirement count=%d error=%v", retired, err)
	}

	if err := jobs.QuarantineIndexMetaInitialization(
		ctx,
		claim,
		"INITIALIZATION_EXTERNAL_CONFLICT",
	); err != nil {
		t.Fatalf("converge initializer after deadline retirement: %v", err)
	}

	assertPostgresCount(t, ctx, pool, 1, `
SELECT count(*)
FROM elitea_runtime.index_ingest_jobs AS i
JOIN elitea_runtime.execution_jobs AS j
  ON j.execution_id = i.execution_id
 AND j.generation = i.generation
JOIN elitea_runtime.command_outbox AS o
  ON o.execution_id = i.execution_id
 AND o.generation = i.generation
WHERE i.execution_id = $1
  AND i.index_meta_initialization_status = 'QUARANTINED'
  AND i.index_meta_initialization_claim_token IS NULL
  AND i.index_meta_initialization_claim_expires_at IS NULL
  AND i.index_meta_initialization_last_error_code =
      'INITIALIZATION_EXTERNAL_CONFLICT'
  AND i.index_meta_initialization_failed_at IS NOT NULL
  AND j.state = 'FAILED'
  AND j.desired_state = 'RUNNING'
  AND j.terminal_error_code = 'DEADLINE_EXCEEDED'
  AND j.settled_at IS NOT NULL
  AND o.retired_at IS NOT NULL
  AND o.retirement_code = 'DEADLINE_EXCEEDED'
  AND o.authority_granted_at IS NULL`,
		admission.ExecutionID,
	)
	assertPostgresCount(t, ctx, pool, 1, `
SELECT count(*)
FROM elitea_runtime.execution_replay_events
WHERE execution_id = $1 AND generation = $2`,
		admission.ExecutionID,
		int64(admission.Generation),
	)
	assertPostgresCount(t, ctx, pool, 1, `
SELECT count(*)
FROM elitea_runtime.execution_replay_events
WHERE execution_id = $1
  AND generation = $2
  AND event_id = $3
  AND event_type = 'execution.failed'`,
		admission.ExecutionID,
		int64(admission.Generation),
		"retirement:initializer-deadline-race-outbox",
	)

	activeRequest := postgresIndexMetaConvergenceRequest(
		1_700_000_002,
		"initializer-active-quarantine",
		"ActiveQuarantineDocs",
	)
	activeAdmission, err := newPostgresIndexAdmissionService(
		t,
		jobs,
		"initializer-active-quarantine",
	).Submit(ctx, activeRequest)
	if err != nil {
		t.Fatal(err)
	}
	activeClaim, err := jobs.ClaimExactIndexMetaInitialization(
		ctx,
		indexingapp.IndexMetaInitialization{
			ExecutionID:     activeAdmission.ExecutionID,
			Generation:      activeAdmission.Generation,
			IndexGeneration: activeAdmission.IndexGeneration,
			MetaID:          activeAdmission.IndexMetaID,
			CorrelationID:   activeAdmission.IndexMetaCorrelationID,
		},
		"initializer-active-quarantine-claim",
		time.Minute,
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := jobs.QuarantineIndexMetaInitialization(
		ctx,
		activeClaim,
		"INITIALIZATION_EXTERNAL_CONFLICT",
	); err != nil {
		t.Fatalf("quarantine active pre-authority initializer: %v", err)
	}
	assertPostgresCount(t, ctx, pool, 1, `
SELECT count(*)
FROM elitea_runtime.index_ingest_jobs AS i
JOIN elitea_runtime.execution_jobs AS j
  ON j.execution_id = i.execution_id
 AND j.generation = i.generation
JOIN elitea_runtime.command_outbox AS o
  ON o.execution_id = i.execution_id
 AND o.generation = i.generation
WHERE i.execution_id = $1
  AND i.index_meta_initialization_status = 'QUARANTINED'
  AND j.state = 'QUARANTINED'
  AND j.desired_state = 'CANCELLED'
  AND j.terminal_error_code IS NULL
  AND o.retired_at IS NOT NULL
  AND o.retirement_code = 'CANCELLED'
  AND o.prepared_at IS NULL
  AND o.published_at IS NULL
  AND o.authority_granted_at IS NULL`,
		activeAdmission.ExecutionID,
	)
	assertPostgresCount(t, ctx, pool, 1, `
SELECT count(*)
FROM elitea_runtime.execution_replay_events
WHERE execution_id = $1
  AND generation = $2
  AND event_id = $3
  AND event_type = 'execution.failed'`,
		activeAdmission.ExecutionID,
		int64(activeAdmission.Generation),
		"index-meta-initialization-quarantine:"+
			"initializer-active-quarantine-outbox",
	)
}

func TestPostgresIndexMetaInitializationQuarantineWinsDeadlineRace(
	t *testing.T,
) {
	fixture := newPostgresIndexMetaInitializationRaceFixture(
		t,
		"initializer-quarantine-wins",
		1_700_000_003,
	)
	releaseBarrier := holdPostgresIndexMetaInitializationBarrier(
		t,
		fixture.ctx,
		fixture.pool,
		postgresIndexMetaQuarantineBarrierKey,
	)
	defer releaseBarrier()

	quarantineResult := make(chan error, 1)
	go func() {
		quarantineResult <- fixture.jobs.QuarantineIndexMetaInitialization(
			fixture.ctx,
			fixture.claim,
			"INITIALIZATION_EXTERNAL_CONFLICT",
		)
	}()
	waitForPostgresIndexMetaInitializationLock(
		t,
		fixture.ctx,
		fixture.pool,
		"WITH locked_triple AS MATERIALIZED",
		"quarantine winner barrier",
	)

	retirementResult := make(chan postgresIndexMetaRetirementResult, 1)
	go func() {
		count, err := fixture.outbox.RetireNoAuthorityIndexIngest(
			fixture.ctx,
			1,
		)
		retirementResult <- postgresIndexMetaRetirementResult{
			count: count,
			err:   err,
		}
	}()
	retirement := waitForPostgresIndexMetaRetirement(
		t,
		fixture.ctx,
		retirementResult,
		"deadline loser",
	)
	if retirement.err != nil || retirement.count != 0 {
		t.Fatalf(
			"deadline retirement bypassed quarantine locks: count=%d error=%v",
			retirement.count,
			retirement.err,
		)
	}

	releaseBarrier()
	if err := waitForPostgresIndexMetaQuarantine(
		t,
		fixture.ctx,
		quarantineResult,
		"quarantine winner",
	); err != nil {
		t.Fatalf("quarantine winner: %v", err)
	}
	assertPostgresIndexMetaQuarantineWinner(t, fixture)
}

func TestPostgresIndexMetaInitializationDeadlineWinsQuarantineRace(
	t *testing.T,
) {
	fixture := newPostgresIndexMetaInitializationRaceFixture(
		t,
		"initializer-deadline-wins",
		1_700_000_004,
	)
	releaseBarrier := holdPostgresIndexMetaInitializationBarrier(
		t,
		fixture.ctx,
		fixture.pool,
		postgresIndexMetaDeadlineBarrierKey,
	)
	defer releaseBarrier()

	retirementResult := make(chan postgresIndexMetaRetirementResult, 1)
	go func() {
		count, err := fixture.outbox.RetireNoAuthorityIndexIngest(
			fixture.ctx,
			1,
		)
		retirementResult <- postgresIndexMetaRetirementResult{
			count: count,
			err:   err,
		}
	}()
	waitForPostgresIndexMetaInitializationLock(
		t,
		fixture.ctx,
		fixture.pool,
		"WITH retired AS (",
		"deadline winner barrier",
	)

	quarantineResult := make(chan error, 1)
	go func() {
		quarantineResult <- fixture.jobs.QuarantineIndexMetaInitialization(
			fixture.ctx,
			fixture.claim,
			"INITIALIZATION_EXTERNAL_CONFLICT",
		)
	}()
	waitForPostgresIndexMetaInitializationLock(
		t,
		fixture.ctx,
		fixture.pool,
		"WITH locked_triple AS MATERIALIZED",
		"quarantine loser row lock",
	)

	releaseBarrier()
	retirement := waitForPostgresIndexMetaRetirement(
		t,
		fixture.ctx,
		retirementResult,
		"deadline winner",
	)
	if retirement.err != nil || retirement.count != 1 {
		t.Fatalf(
			"deadline winner count=%d error=%v",
			retirement.count,
			retirement.err,
		)
	}
	if err := waitForPostgresIndexMetaQuarantine(
		t,
		fixture.ctx,
		quarantineResult,
		"quarantine convergence",
	); err != nil {
		t.Fatalf("quarantine convergence after deadline: %v", err)
	}
	assertPostgresIndexMetaDeadlineWinner(t, fixture)
}

const (
	postgresIndexMetaQuarantineBarrierKey int64 = 9_100_000_001
	postgresIndexMetaDeadlineBarrierKey   int64 = 9_100_000_002
)

type postgresIndexMetaInitializationRaceFixture struct {
	ctx       context.Context
	pool      *pgxpool.Pool
	jobs      *IndexIngestJobsRepository
	outbox    *CommandOutboxRepository
	admission indexingapp.AdmissionOutcome
	outboxID  string
	claim     indexingapp.IndexMetaInitializationClaim
}

type postgresIndexMetaRetirementResult struct {
	count int
	err   error
}

func newPostgresIndexMetaInitializationRaceFixture(
	t *testing.T,
	prefix string,
	toolkitID int32,
) postgresIndexMetaInitializationRaceFixture {
	t.Helper()
	pool := newMigratedPostgresIntegrationPool(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	t.Cleanup(cancel)
	installPostgresIndexMetaInitializationRaceBarriers(t, ctx, pool)
	var isolation string
	if err := pool.QueryRow(
		ctx,
		"SHOW default_transaction_isolation",
	).Scan(&isolation); err != nil {
		t.Fatal(err)
	}
	if isolation != "read committed" {
		t.Fatalf("race fixture isolation=%q want read committed", isolation)
	}

	policy := IndexIngestDispatchPolicy{
		StreamName:        "elitea:runtime:index:commands",
		CapabilityVersion: "1",
		ResourceClass:     "indexing",
		IsolationClass:    "project",
		Priority:          1,
		DeadlineTTL:       time.Hour,
		LimitsRevision:    "index-limits-v1",
		MaxOutstanding:    16,
	}
	jobs, err := NewIndexIngestJobsRepository(pool, policy)
	if err != nil {
		t.Fatal(err)
	}
	outbox, err := NewCommandOutboxRepository(pool, policy.StreamName)
	if err != nil {
		t.Fatal(err)
	}
	request := postgresIndexMetaConvergenceRequest(
		toolkitID,
		prefix,
		"RaceDocs",
	)
	admission, err := newPostgresIndexAdmissionService(
		t,
		jobs,
		prefix,
	).Submit(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	claim, err := jobs.ClaimExactIndexMetaInitialization(
		ctx,
		indexingapp.IndexMetaInitialization{
			ExecutionID:     admission.ExecutionID,
			Generation:      admission.Generation,
			IndexGeneration: admission.IndexGeneration,
			MetaID:          admission.IndexMetaID,
			CorrelationID:   admission.IndexMetaCorrelationID,
		},
		prefix+"-claim",
		time.Minute,
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
UPDATE elitea_runtime.command_outbox
SET deadline = clock_timestamp() - interval '1 second'
WHERE execution_id = $1 AND generation = $2`,
		admission.ExecutionID,
		int64(admission.Generation),
	); err != nil {
		t.Fatal(err)
	}
	return postgresIndexMetaInitializationRaceFixture{
		ctx:       ctx,
		pool:      pool,
		jobs:      jobs,
		outbox:    outbox,
		admission: admission,
		outboxID:  prefix + "-outbox",
		claim:     claim,
	}
}

func installPostgresIndexMetaInitializationRaceBarriers(
	t *testing.T,
	ctx context.Context,
	pool *pgxpool.Pool,
) {
	t.Helper()
	if _, err := pool.Exec(ctx, `
CREATE FUNCTION elitea_runtime.test_index_meta_quarantine_barrier()
RETURNS trigger
LANGUAGE plpgsql
AS $$
BEGIN
    PERFORM pg_advisory_xact_lock(9100000001);
    RETURN NEW;
END
$$;
CREATE TRIGGER test_index_meta_quarantine_barrier
BEFORE UPDATE OF index_meta_initialization_status
ON elitea_runtime.index_ingest_jobs
FOR EACH ROW
WHEN (
    OLD.index_meta_initialization_status = 'RUNNING'
    AND NEW.index_meta_initialization_status = 'QUARANTINED'
)
EXECUTE FUNCTION elitea_runtime.test_index_meta_quarantine_barrier();

CREATE FUNCTION elitea_runtime.test_index_meta_deadline_barrier()
RETURNS trigger
LANGUAGE plpgsql
AS $$
BEGIN
    PERFORM pg_advisory_xact_lock(9100000002);
    RETURN NEW;
END
$$;
CREATE TRIGGER test_index_meta_deadline_barrier
BEFORE UPDATE OF retired_at
ON elitea_runtime.command_outbox
FOR EACH ROW
WHEN (
    OLD.retired_at IS NULL
    AND NEW.retirement_code = 'DEADLINE_EXCEEDED'
)
EXECUTE FUNCTION elitea_runtime.test_index_meta_deadline_barrier();
`); err != nil {
		t.Fatalf("install index metadata race barriers: %v", err)
	}
}

func holdPostgresIndexMetaInitializationBarrier(
	t *testing.T,
	ctx context.Context,
	pool *pgxpool.Pool,
	key int64,
) func() {
	t.Helper()
	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire index metadata race barrier: %v", err)
	}
	if _, err := conn.Exec(ctx, "SELECT pg_advisory_lock($1)", key); err != nil {
		conn.Release()
		t.Fatalf("hold index metadata race barrier: %v", err)
	}
	released := false
	return func() {
		if released {
			return
		}
		released = true
		var unlocked bool
		if err := conn.QueryRow(
			context.Background(),
			"SELECT pg_advisory_unlock($1)",
			key,
		).Scan(&unlocked); err != nil {
			t.Errorf("release index metadata race barrier: %v", err)
		} else if !unlocked {
			t.Errorf("index metadata race barrier was not held")
		}
		conn.Release()
	}
}

func waitForPostgresIndexMetaInitializationLock(
	t *testing.T,
	ctx context.Context,
	pool *pgxpool.Pool,
	queryFragment string,
	name string,
) {
	t.Helper()
	ticker := time.NewTicker(5 * time.Millisecond)
	defer ticker.Stop()
	for {
		var waiting bool
		err := pool.QueryRow(ctx, `
SELECT EXISTS (
    SELECT 1
    FROM pg_stat_activity
    WHERE datname = current_database()
      AND pid <> pg_backend_pid()
      AND state = 'active'
      AND wait_event_type = 'Lock'
      AND position($1::text in query) > 0
)`, queryFragment).Scan(&waiting)
		if err != nil {
			t.Fatalf("inspect %s: %v", name, err)
		}
		if waiting {
			return
		}
		select {
		case <-ticker.C:
		case <-ctx.Done():
			t.Fatalf("wait for %s: %v", name, ctx.Err())
		}
	}
}

func waitForPostgresIndexMetaRetirement(
	t *testing.T,
	ctx context.Context,
	results <-chan postgresIndexMetaRetirementResult,
	name string,
) postgresIndexMetaRetirementResult {
	t.Helper()
	select {
	case result := <-results:
		return result
	case <-ctx.Done():
		t.Fatalf("wait for %s: %v", name, ctx.Err())
		return postgresIndexMetaRetirementResult{}
	}
}

func waitForPostgresIndexMetaQuarantine(
	t *testing.T,
	ctx context.Context,
	results <-chan error,
	name string,
) error {
	t.Helper()
	select {
	case err := <-results:
		return err
	case <-ctx.Done():
		t.Fatalf("wait for %s: %v", name, ctx.Err())
		return ctx.Err()
	}
}

func assertPostgresIndexMetaQuarantineWinner(
	t *testing.T,
	fixture postgresIndexMetaInitializationRaceFixture,
) {
	t.Helper()
	assertPostgresCount(t, fixture.ctx, fixture.pool, 1, `
SELECT count(*)
FROM elitea_runtime.index_ingest_jobs AS i
JOIN elitea_runtime.execution_jobs AS j
  ON j.execution_id = i.execution_id
 AND j.generation = i.generation
JOIN elitea_runtime.command_outbox AS o
  ON o.execution_id = i.execution_id
 AND o.generation = i.generation
WHERE i.execution_id = $1
  AND i.index_meta_initialization_status = 'QUARANTINED'
  AND i.index_meta_initialization_claim_token IS NULL
  AND j.state = 'QUARANTINED'
  AND j.desired_state = 'CANCELLED'
  AND j.terminal_error_code IS NULL
  AND o.retired_at IS NOT NULL
  AND o.retirement_code = 'CANCELLED'
  AND o.authority_granted_at IS NULL`,
		fixture.admission.ExecutionID,
	)
	assertPostgresCount(t, fixture.ctx, fixture.pool, 1, `
SELECT count(*)
FROM elitea_runtime.execution_replay_events
WHERE execution_id = $1
  AND generation = $2
  AND event_id = $3
  AND event_type = 'execution.failed'`,
		fixture.admission.ExecutionID,
		int64(fixture.admission.Generation),
		"index-meta-initialization-quarantine:"+
			fixture.outboxID,
	)
	assertPostgresCount(t, fixture.ctx, fixture.pool, 1, `
SELECT count(*)
FROM elitea_runtime.execution_replay_events
WHERE execution_id = $1 AND generation = $2`,
		fixture.admission.ExecutionID,
		int64(fixture.admission.Generation),
	)
}

func assertPostgresIndexMetaDeadlineWinner(
	t *testing.T,
	fixture postgresIndexMetaInitializationRaceFixture,
) {
	t.Helper()
	assertPostgresCount(t, fixture.ctx, fixture.pool, 1, `
SELECT count(*)
FROM elitea_runtime.index_ingest_jobs AS i
JOIN elitea_runtime.execution_jobs AS j
  ON j.execution_id = i.execution_id
 AND j.generation = i.generation
JOIN elitea_runtime.command_outbox AS o
  ON o.execution_id = i.execution_id
 AND o.generation = i.generation
WHERE i.execution_id = $1
  AND i.index_meta_initialization_status = 'QUARANTINED'
  AND i.index_meta_initialization_claim_token IS NULL
  AND i.index_meta_initialization_last_error_code =
      'INITIALIZATION_EXTERNAL_CONFLICT'
  AND j.state = 'FAILED'
  AND j.desired_state = 'RUNNING'
  AND j.terminal_error_code = 'DEADLINE_EXCEEDED'
  AND j.settled_at = o.retired_at
  AND o.retirement_code = 'DEADLINE_EXCEEDED'
  AND o.authority_granted_at IS NULL`,
		fixture.admission.ExecutionID,
	)
	assertPostgresCount(t, fixture.ctx, fixture.pool, 1, `
SELECT count(*)
FROM elitea_runtime.execution_replay_events
WHERE execution_id = $1
  AND generation = $2
  AND event_id = $3
  AND event_type = 'execution.failed'`,
		fixture.admission.ExecutionID,
		int64(fixture.admission.Generation),
		"retirement:"+fixture.outboxID,
	)
	assertPostgresCount(t, fixture.ctx, fixture.pool, 1, `
SELECT count(*)
FROM elitea_runtime.execution_replay_events
WHERE execution_id = $1 AND generation = $2`,
		fixture.admission.ExecutionID,
		int64(fixture.admission.Generation),
	)
}

func newPostgresDurableIndexMetaInitializer(
	t *testing.T,
	jobs *IndexIngestJobsRepository,
	materializer indexingapp.IndexMetaMaterializer,
	claimID string,
) *indexingapp.DurableIndexMetaInitializer {
	t.Helper()
	initializer, err := indexingapp.NewDurableIndexMetaInitializer(
		jobs,
		materializer,
		func() (string, error) { return claimID, nil },
		indexingapp.IndexMetaInitializationReconcilerConfig{
			PollInterval:  time.Millisecond,
			ClaimLease:    time.Minute,
			BatchSize:     4,
			MaxConcurrent: 2,
			ReportFailure: func(error) {},
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	return initializer
}

func postgresIntegrationPoolURL(t *testing.T, original, database string) string {
	t.Helper()
	parsed, err := url.Parse(original)
	if err != nil || (parsed.Scheme != "postgres" && parsed.Scheme != "postgresql") ||
		parsed.Host == "" || database == "" {
		t.Fatal("ELITEA_TEST_DATABASE_URL must be a PostgreSQL URL")
	}
	parsed.Path = "/" + database
	parsed.RawPath = ""
	return parsed.String()
}

func postgresIndexMetaConvergenceRequest(
	toolkitID int32,
	idempotencyKey string,
	indexName string,
) indexingapp.SubmitRequest {
	request := postgresIndexSubmitRequest(idempotencyKey, indexName)
	request.Identity.TenantID = request.Identity.ResourceProjectID
	request.ToolkitID = toolkitID
	request.Inputs.ToolkitConfiguration = json.RawMessage(fmt.Sprintf(
		`{"id":%d,"type":"confluence","settings":{"pgvector_configuration":{"__elitea_frozen_configuration_v1":true,"configuration_type":"pgvector","configuration_project_id":1,"configuration_uuid":"integration-pgvector","connection_string":"{{secret.PGVECTOR}}"}}}`,
		toolkitID,
	))
	return request
}

func assertPostgresPgvectorMeta(
	t *testing.T,
	ctx context.Context,
	pool interface {
		QueryRow(context.Context, string, ...any) pgx.Row
	},
	toolkitID int32,
	indexName string,
	metaID string,
	executionID string,
	historyLength int,
) {
	t.Helper()
	schema := pgx.Identifier{strconv.FormatInt(int64(toolkitID), 10)}.Sanitize()
	var raw []byte
	if err := pool.QueryRow(ctx, `
SELECT cmetadata
FROM `+schema+`.langchain_pg_embedding
WHERE cmetadata->>'collection' = $1
  AND cmetadata->>'type' = 'index_meta'`, indexName).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	var metadata map[string]any
	if err := json.Unmarshal(raw, &metadata); err != nil {
		t.Fatal(err)
	}
	historyEncoded, ok := metadata["history"].(string)
	if !ok {
		t.Fatalf("history type=%T", metadata["history"])
	}
	var history []map[string]any
	if err := json.Unmarshal([]byte(historyEncoded), &history); err != nil {
		t.Fatal(err)
	}
	if metadata["index_meta_id"] != metaID || metadata["execution_id"] != executionID ||
		metadata["state"] != "in_progress" || len(history) != historyLength {
		t.Fatalf("metadata=%#v history=%#v", metadata, history)
	}
}
