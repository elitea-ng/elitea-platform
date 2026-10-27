package repos

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	indexingapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/indexing"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/credentialgovernance/profile"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestPostgresIndexMetaInitializationQuarantineConvergesExpiredExecution(
	t *testing.T,
) {
	pool := newMigratedPostgresIntegrationPool(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	policy := IndexIngestDispatchPolicy{

		CredentialProfile: profile.LegacyAdmission(false),
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

		CredentialProfile: profile.LegacyAdmission(false),
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

func postgresIndexMetaConvergenceRequest(
	toolkitID int32,
	idempotencyKey string,
	indexName string,
) indexingapp.SubmitRequest {
	request := postgresIndexSubmitRequest(idempotencyKey, indexName)
	request.Identity.TenantID = request.Identity.ResourceProjectID
	request.ToolkitID = toolkitID
	request.Inputs.ToolkitConfiguration = json.RawMessage(fmt.Sprintf(
		`{"id":%d,"type":"confluence","settings":{}}`,
		toolkitID,
	))
	return request
}
