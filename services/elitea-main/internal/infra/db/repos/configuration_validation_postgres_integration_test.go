package repos

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	runtimev1 "github.com/EliteaAI/elitea-platform/libs/proto/gen/go/elitea/runtime/v1"
	executionapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/execution"
	outputapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/output"
	configurationdomain "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/configurations"
	executiondomain "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/execution"
	runtimedomain "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/runtime"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/db/dbtest"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/db/migrate"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/db/tenant"
	platformmigrations "github.com/EliteaAI/elitea-platform/services/elitea-main/migrations"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/protobuf/proto"
)

const postgresIntegrationDatabaseURL = "ELITEA_TEST_DATABASE_URL"

func TestTenantBoundariesRejectMissingLegacySchema(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := pool.Exec(ctx, `INSERT INTO centry.project (id) VALUES (2)`); err != nil {
		t.Fatal(err)
	}

	runner := migrate.New(pool, platformmigrations.Files)
	if err := runner.ApplyTenant(ctx, 2); err == nil || !strings.Contains(err.Error(), "tenant schema p_2 does not exist") {
		t.Fatalf("missing-schema migration error = %v", err)
	}

	callbackCalled := false
	executor := tenant.NewExecutor(pool)
	err := executor.WithinTx(ctx, tenant.Project{ID: 2}, pgx.TxOptions{}, func(context.Context, pgx.Tx) error {
		callbackCalled = true
		return nil
	})
	if err == nil || !strings.Contains(err.Error(), "schema p_2 does not exist") {
		t.Fatalf("missing-schema transaction error = %v", err)
	}
	if callbackCalled {
		t.Fatal("tenant callback ran without its legacy schema")
	}

	for _, relation := range []string{"configuration_revisions", "configuration_validation_projection"} {
		var createdInCatalog bool
		if err := pool.QueryRow(ctx, `SELECT to_regclass('pg_catalog.' || $1) IS NOT NULL`, relation).Scan(&createdInCatalog); err != nil {
			t.Fatal(err)
		}
		if createdInCatalog {
			t.Fatalf("tenant relation %s escaped into pg_catalog", relation)
		}
	}
}

func TestTenantMigrationPreservesLegacyConfigurationDelete(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	digest := make([]byte, 32)

	if _, err := pool.Exec(ctx, `
INSERT INTO p_1.configuration_revisions (
    revision_id, configuration_id, configuration_type, settings_entry_id,
    settings_entry_version, settings_content_digest, input_bundle_id,
    catalog_revision, catalog_digest, schema_id, schema_revision,
    schema_digest, created_by
) VALUES (
    'legacy-delete-shadow', 1, 'openapi', 'settings', '1', $1, 'bundle',
    'catalog', $1, 'schema', '1', $1, 'test'
)`, digest); err != nil {
		t.Fatalf("seed shadow revision: %v", err)
	}
	if _, err := pool.Exec(ctx, `DELETE FROM p_1.configuration WHERE id = 1`); err != nil {
		t.Fatalf("tenant expansion blocked legacy configuration delete: %v", err)
	}
	var configurationID *int
	if err := pool.QueryRow(ctx, `
SELECT configuration_id
FROM p_1.configuration_revisions
WHERE revision_id = 'legacy-delete-shadow'`).Scan(&configurationID); err != nil {
		t.Fatalf("read detached shadow revision: %v", err)
	}
	if configurationID != nil {
		t.Fatalf("deleted legacy configuration remains attached to shadow revision: %d", *configurationID)
	}
}

// TestPostgresServiceBackedCancellationControlPlane is a real PostgreSQL 16-18
// service-integration test. It intentionally does not claim full transport
// E2E, penetration, performance, or soak coverage.
func TestPostgresServiceBackedCancellationControlPlane(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)

	t.Run("canonical cancellation is durable idempotent and recoverable cross pod", func(t *testing.T) {
		frame := postgresValidationFrame(t, "durability")
		seed := seedPostgresValidationExecution(t, pool, frame, runtimedomain.DesiredCancelled)
		service := newPostgresValidationService(t, pool)

		_, err := service.Ingest(context.Background(), frame)
		if !errors.Is(err, outputapp.ErrOutputCancelled) {
			t.Fatalf("expected cancellation to win output linearization, got %v", err)
		}
		assertPostgresCanonicalCancellation(t, pool, frame, seed.claimID)

		// The worker may replay its original frame after losing the response.
		// That replay must recover the already-durable cancellation winner and
		// must not append any duplicate durable rows.
		_, err = service.Ingest(context.Background(), frame)
		if !errors.Is(err, outputapp.ErrOutputCancelled) {
			t.Fatalf("expected replay to recover cancellation winner, got %v", err)
		}
		assertPostgresCanonicalCancellation(t, pool, frame, seed.claimID)

		expirePostgresClaim(t, pool, seed.claimID)
		claimRepository, err := NewClaimsRepository(pool)
		if err != nil {
			t.Fatal(err)
		}
		claimService, err := executionapp.NewClaimService(claimRepository, time.Now, 30*time.Second)
		if err != nil {
			t.Fatal(err)
		}
		request := executionapp.ClaimRequest{
			CommandID:            frame.Fence.CommandID,
			OutboxID:             seed.outboxID,
			ExecutionID:          frame.Fence.ExecutionID,
			Generation:           frame.Fence.Generation,
			CapabilityID:         executiondomain.ConfigurationValidationCapability,
			SignedEnvelopeDigest: seed.envelopeDigest,
			WorkloadIdentity:     "spiffe://elitea.test/worker/replacement",
			WorkloadSessionID:    "session-replacement",
			ProducerID:           "producer-replacement",
		}
		decision, err := claimService.Claim(context.Background(), request)
		if err != nil {
			t.Fatalf("recover terminal claim: %v", err)
		}
		assertPostgresTerminalRecovery(t, pool, frame, seed, request, decision)
		assertPostgresCanonicalCancellation(t, pool, frame, seed.claimID)
	})

	t.Run("committed cancellation wins a blocked output transaction", func(t *testing.T) {
		frame := postgresValidationFrame(t, "lock-race")
		seed := seedPostgresValidationExecution(t, pool, frame, runtimedomain.DesiredRunning)
		service := newPostgresValidationService(t, pool)

		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		cancelTx, err := pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted, AccessMode: pgx.ReadWrite})
		if err != nil {
			t.Fatal(err)
		}
		finished := false
		defer func() {
			if !finished {
				_ = cancelTx.Rollback(context.Background())
			}
		}()

		// Hold both rows used by insertOutputInbox's authority CTE before
		// publishing cancellation. The output goroutine can perform its
		// read-only binding checks, then must wait at the real PostgreSQL lock.
		var lockedClaim string
		var cancellationBackendPID int32
		if err := cancelTx.QueryRow(ctx, `
SELECT c.claim_id, pg_backend_pid()
FROM elitea_runtime.execution_claims AS c
JOIN elitea_runtime.execution_jobs AS j
  ON j.execution_id = c.execution_id AND j.generation = c.generation
JOIN elitea_runtime.command_outbox AS o
  ON o.execution_id = j.execution_id AND o.generation = j.generation
WHERE c.execution_id = $1 AND c.generation = $2
FOR UPDATE OF j, o, c`, frame.Fence.ExecutionID, int64(frame.Fence.Generation)).Scan(&lockedClaim, &cancellationBackendPID); err != nil {
			t.Fatalf("lock cancellation authority: %v", err)
		}
		if lockedClaim != seed.claimID {
			t.Fatalf("locked unexpected claim %q", lockedClaim)
		}
		if _, err := cancelTx.Exec(ctx, `
UPDATE elitea_runtime.execution_jobs
SET desired_state = 'CANCELLED'
WHERE execution_id = $1 AND generation = $2`, frame.Fence.ExecutionID, int64(frame.Fence.Generation)); err != nil {
			t.Fatalf("stage cancellation: %v", err)
		}

		result := make(chan error, 1)
		go func() {
			_, ingestErr := service.Ingest(ctx, frame)
			result <- ingestErr
		}()

		blocked := waitForPostgresAuthorityLock(t, ctx, pool, cancellationBackendPID)
		if err := cancelTx.Commit(ctx); err != nil {
			t.Fatalf("commit cancellation: %v", err)
		}
		finished = true
		ingestErr := <-result
		if !blocked {
			t.Fatal("output transaction never reached the PostgreSQL authority lock")
		}
		if !errors.Is(ingestErr, outputapp.ErrOutputCancelled) {
			t.Fatalf("committed cancellation did not win blocked output: %v", ingestErr)
		}
		assertPostgresCanonicalCancellation(t, pool, frame, seed.claimID)
	})
}

// TestPostgresServiceBackedInvocationFence proves the two restart classes on a
// real PostgreSQL state owner: PREPARING is recoverable, while a durably
// authorized invocation is recovery-only and cannot receive SDK inputs again.
func TestPostgresServiceBackedInvocationFence(t *testing.T) {
	for _, capability := range []string{
		executiondomain.IndexIngestCapability,
		executiondomain.ToolkitExecuteReadCapability,
		executiondomain.ToolkitCallToolCapability,
		executiondomain.ToolkitAvailableToolsCapability,
	} {
		t.Run(capability, func(t *testing.T) { testPostgresInvocationFence(t, capability) })
	}
}

func testPostgresInvocationFence(t *testing.T, capability string) {
	t.Helper()
	pool := newMigratedPostgresIntegrationPool(t)
	frame := postgresValidationFrame(t, "invocation-fence")
	seed := seedPostgresValidationExecution(t, pool, frame, runtimedomain.DesiredRunning)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := pool.Exec(ctx, `
UPDATE elitea_runtime.execution_jobs
SET capability_id = $3,
    capability_version = '1',
    configuration_revision_id = NULL,
    configuration_type = NULL,
    catalog_revision = NULL,
    catalog_digest = NULL,
    schema_id = NULL,
    schema_revision = NULL,
    schema_digest = NULL,
    settings_entry_id = NULL
WHERE execution_id = $1 AND generation = $2`,
		frame.Fence.ExecutionID,
		int64(frame.Fence.Generation), capability,
	); err != nil {
		t.Fatal(err)
	}
	repository, err := NewClaimsRepository(pool)
	if err != nil {
		t.Fatal(err)
	}
	service, err := executionapp.NewClaimService(repository, time.Now, 30*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if disposition, err := service.BeginExecution(ctx, frame.Fence); err != nil || disposition != executionapp.BeginExecutionStartedNow {
		t.Fatalf("begin initial execution disposition=%q err=%v", disposition, err)
	}
	expirePostgresClaim(t, pool, seed.claimID)
	request := executionapp.ClaimRequest{
		CommandID:            frame.Fence.CommandID,
		OutboxID:             seed.outboxID,
		ExecutionID:          frame.Fence.ExecutionID,
		Generation:           frame.Fence.Generation,
		CapabilityID:         capability,
		SignedEnvelopeDigest: seed.envelopeDigest,
		WorkloadIdentity:     "spiffe://elitea.test/workload/replacement-preparing",
		WorkloadSessionID:    "replacement-preparing-session",
		ProducerID:           "replacement-preparing-producer",
	}
	preparing, err := service.Claim(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	if preparing.Disposition != executionapp.ClaimAccepted {
		t.Fatalf("PREPARING execution did not receive executable recovery: %+v", preparing)
	}
	if disposition, err := service.BeginExecution(ctx, preparing.Lease.Fence); err != nil || disposition != executionapp.BeginExecutionStartedNow {
		t.Fatalf("resume pre-submission disposition=%q err=%v", disposition, err)
	}
	if disposition, err := service.AuthorizeInvocation(ctx, preparing.Lease.Fence); err != nil || disposition != executionapp.AuthorizeInvocationNow {
		t.Fatalf("authorize invocation disposition=%q err=%v", disposition, err)
	}
	if disposition, err := service.AuthorizeInvocation(ctx, preparing.Lease.Fence); err != nil || disposition != executionapp.AuthorizeInvocationAlready {
		t.Fatalf("invocation authorization replay=%q err=%v", disposition, err)
	}
	ageAndExpirePostgresClaim(t, pool, preparing.Lease.ClaimID)
	request.WorkloadIdentity = "spiffe://elitea.test/workload/replacement-ambiguous"
	request.WorkloadSessionID = "replacement-ambiguous-session"
	request.ProducerID = "replacement-ambiguous-producer"
	ambiguous, err := service.Claim(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	if ambiguous.Disposition != executionapp.ClaimRecoverAmbiguousInvocationNoACK {
		t.Fatalf("MAY_HAVE_STARTED execution received executable authority: %+v", ambiguous)
	}
	var invocationState string
	if err := pool.QueryRow(ctx, `
SELECT invocation_state
FROM elitea_runtime.execution_jobs
WHERE execution_id = $1 AND generation = $2`,
		frame.Fence.ExecutionID,
		int64(frame.Fence.Generation),
	).Scan(&invocationState); err != nil {
		t.Fatal(err)
	}
	if invocationState != "MAY_HAVE_STARTED" {
		t.Fatalf("durable invocation state=%q", invocationState)
	}
}

func TestPostgresServiceBackedDatabaseDeadlineOutputLinearization(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	frame := postgresValidationFrame(t, "database-deadline-output")
	seed := seedPostgresValidationExecution(t, pool, frame, runtimedomain.DesiredRunning)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := pool.Exec(ctx, `
UPDATE elitea_runtime.command_outbox
SET deadline = clock_timestamp() - interval '1 minute'
WHERE outbox_id = $1`, seed.outboxID); err != nil {
		t.Fatal(err)
	}

	_, err := newPostgresValidationService(t, pool).Ingest(ctx, frame)
	if !errors.Is(err, outputapp.ErrOutputDeadlineExceeded) {
		t.Fatalf("late success did not lose database deadline: %v", err)
	}
	assertPostgresCount(t, ctx, pool, 0, `SELECT count(*) FROM elitea_runtime.output_inbox WHERE execution_id = $1`, frame.Fence.ExecutionID)
	assertPostgresCount(t, ctx, pool, 0, `SELECT count(*) FROM elitea_runtime.execution_replay_events WHERE execution_id = $1`, frame.Fence.ExecutionID)
	assertPostgresCount(t, ctx, pool, 0, `SELECT count(*) FROM elitea_runtime.configuration_validation_results WHERE execution_id = $1`, frame.Fence.ExecutionID)

	deadlineFrame := postgresDeadlineFailureFrame(t, frame)
	outcome, err := newPostgresRuntimeFailureService(t, pool).IngestFailure(ctx, deadlineFrame)
	if err != nil || !outcome.Inserted || outcome.CommittedSequence != 1 {
		t.Fatalf("canonical deadline output was not admitted: outcome=%+v err=%v", outcome, err)
	}
	var payload []byte
	if err := pool.QueryRow(ctx, `SELECT payload_bytes FROM elitea_runtime.output_inbox WHERE execution_id = $1`, frame.Fence.ExecutionID).Scan(&payload); err != nil {
		t.Fatal(err)
	}
	failure := &runtimev1.RuntimeErrorV1{}
	if err := proto.Unmarshal(payload, failure); err != nil {
		t.Fatal(err)
	}
	if failure.GetCode() != runtimev1.RuntimeErrorCodeV1_RUNTIME_ERROR_CODE_V1_DEADLINE_EXCEEDED || failure.GetSafeMessage() != outputapp.DeadlineExceededSafeMessage || !failure.GetRetryable() {
		t.Fatalf("stored noncanonical deadline failure: %v", failure)
	}
	assertPostgresCount(t, ctx, pool, 1, `SELECT count(*) FROM elitea_runtime.output_inbox WHERE execution_id = $1`, frame.Fence.ExecutionID)
	assertPostgresCount(t, ctx, pool, 1, `SELECT count(*) FROM elitea_runtime.execution_replay_events WHERE execution_id = $1 AND event_type = $2`, frame.Fence.ExecutionID, replayEventRuntimeFailure)
	assertPostgresCount(t, ctx, pool, 0, `SELECT count(*) FROM elitea_runtime.configuration_validation_results WHERE execution_id = $1`, frame.Fence.ExecutionID)

	// The deadline guard applies only to the first durable output. Once success
	// and replay are committed before the deadline, their exact identity remains
	// authoritative when the clock later crosses it.
	replayFrame := postgresValidationFrame(t, "durable-output-before-deadline")
	replaySeed := seedPostgresValidationExecution(t, pool, replayFrame, runtimedomain.DesiredRunning)
	replayService := newPostgresValidationService(t, pool)
	first, err := replayService.Ingest(ctx, replayFrame)
	if err != nil || !first.Inserted {
		t.Fatalf("commit success before deadline: outcome=%+v err=%v", first, err)
	}
	if _, err := pool.Exec(ctx, `UPDATE elitea_runtime.command_outbox SET deadline = clock_timestamp() - interval '1 minute' WHERE outbox_id = $1`, replaySeed.outboxID); err != nil {
		t.Fatal(err)
	}
	replayed, err := replayService.Ingest(ctx, replayFrame)
	if err != nil || replayed.Inserted || replayed.Cursor != first.Cursor || replayed.CommittedSequence != first.CommittedSequence {
		t.Fatalf("durable success lost to a later clock crossing: first=%+v replay=%+v err=%v", first, replayed, err)
	}
	assertPostgresCount(t, ctx, pool, 1, `SELECT count(*) FROM elitea_runtime.output_inbox WHERE execution_id = $1`, replayFrame.Fence.ExecutionID)
	assertPostgresCount(t, ctx, pool, 1, `SELECT count(*) FROM elitea_runtime.execution_replay_events WHERE execution_id = $1`, replayFrame.Fence.ExecutionID)
	assertPostgresCount(t, ctx, pool, 1, `SELECT count(*) FROM elitea_runtime.configuration_validation_results WHERE execution_id = $1`, replayFrame.Fence.ExecutionID)
}

// TestPostgresServiceBackedOutboxReconciliation exercises PostgreSQL-specific
// ordering and filtering semantics. Unit tests still cover fail-closed row
// decoding; this gate proves that phase one dispatches only admitted generation
// one rows and can walk them in bounded oldest-first order.
func TestPostgresServiceBackedOutboxReconciliation(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	assertPostgresOutboxDispatchIndex(t, pool)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	base := time.Now().UTC()
	seedPostgresOutboxOrderingFixtures(t, pool, base)

	repository, err := NewCommandOutboxRepository(pool, "elitea:runtime:commands")
	if err != nil {
		t.Fatal(err)
	}

	got, err := repository.ListPendingValidationIDs(ctx, 10, time.Minute)
	if err != nil {
		t.Fatalf("list initial pending validation outbox: %v", err)
	}
	want := []string{"outbox-order-b-1", "outbox-order-a-1"}
	if !equalPostgresStrings(got, want) {
		t.Fatalf("unexpected initial outbox order: got %v want %v", got, want)
	}

	got, err = repository.ListPendingValidationIDs(ctx, 1, time.Minute)
	if err != nil {
		t.Fatalf("list bounded pending validation outbox: %v", err)
	}
	want = []string{"outbox-order-b-1"}
	if !equalPostgresStrings(got, want) {
		t.Fatalf("unexpected bounded outbox order: got %v want %v", got, want)
	}

	assertPostgresPreparedEnvelopeCAS(t, ctx, repository, "outbox-order-b-1")
	prepared, err := repository.StorePreparedValidation(ctx, "outbox-order-a-1", postgresPreparedEnvelope("publish-a-1"))
	if err != nil {
		t.Fatalf("prepare earliest generation fixture: %v", err)
	}
	publishedDigest := prepared.Envelope.Digest
	wrongDigest := runtimedomain.SHA256([]byte("different-signed-envelope"))
	if err := repository.MarkValidationPublished(ctx, "outbox-order-a-1", wrongDigest); !errors.Is(err, ErrOutboxPublishConflict) {
		t.Fatalf("expected mismatched prepared digest to fail publication, got %v", err)
	}
	if err := repository.MarkValidationPublished(ctx, "outbox-order-a-1", publishedDigest); err != nil {
		t.Fatalf("publish earliest generation fixture: %v", err)
	}
	if err := repository.MarkValidationPublished(ctx, "outbox-order-a-1", publishedDigest); err != nil {
		t.Fatalf("reconcile already-published generation: %v", err)
	}

	got, err = repository.ListPendingValidationIDs(ctx, 10, time.Minute)
	if err != nil {
		t.Fatalf("list advanced pending validation outbox: %v", err)
	}
	// Generation two was deliberately created before every other candidate.
	// It remains unsupported after generation one is published; promotion needs
	// a future terminal/supersession transaction, not publication ordering.
	want = []string{"outbox-order-b-1"}
	if !equalPostgresStrings(got, want) {
		t.Fatalf("unexpected advanced outbox order: got %v want %v", got, want)
	}
	if _, err := pool.Exec(ctx, `
UPDATE elitea_runtime.command_outbox
SET last_visibility_at = clock_timestamp() - interval '2 minutes'
WHERE outbox_id = 'outbox-order-published'`); err != nil {
		t.Fatalf("age published visibility fixture: %v", err)
	}
	got, err = repository.ListPendingValidationIDs(ctx, 10, time.Minute)
	if err != nil {
		t.Fatalf("list stale published validation outbox: %v", err)
	}
	want = []string{"outbox-order-published", "outbox-order-b-1"}
	if !equalPostgresStrings(got, want) {
		t.Fatalf("stale visibility was not repaired oldest-first: got %v want %v", got, want)
	}
}

func TestPostgresServiceBackedNoAuthorityRetirementRacesAndExclusion(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	assertPostgresRetirementIndexes(t, pool)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	repository, err := NewCommandOutboxRepository(pool, "elitea:runtime:commands")
	if err != nil {
		t.Fatal(err)
	}
	claims, err := NewClaimsRepository(pool)
	if err != nil {
		t.Fatal(err)
	}

	// Both Redis-XADD outcomes are authoritative: an unpublished prepared row
	// models unknown XADD success, while a published row models a completed mark.
	type retirementFixture struct {
		frame     outputapp.ConfigurationValidationFrame
		seed      postgresValidationSeed
		published bool
	}
	fixtures := make([]retirementFixture, 0, 2)
	for _, published := range []bool{false, true} {
		suffix := map[bool]string{false: "deadline-unpublished", true: "deadline-published"}[published]
		frame := postgresValidationFrame(t, suffix)
		seed := seedPostgresValidationExecution(t, pool, frame, runtimedomain.DesiredRunning)
		resetPostgresExecutionToNoAuthority(t, pool, frame, seed, published, runtimedomain.DesiredRunning, time.Now().UTC().Add(-time.Minute))
		fixtures = append(fixtures, retirementFixture{frame: frame, seed: seed, published: published})
	}
	retired, err := repository.RetireNoAuthorityValidation(ctx, 8)
	if err != nil || retired != 2 {
		t.Fatalf("retire published/unpublished deadlines: retired=%d err=%v", retired, err)
	}
	retired, err = repository.RetireNoAuthorityValidation(ctx, 8)
	if err != nil || retired != 0 {
		t.Fatalf("deadline retirement is not idempotent: retired=%d err=%v", retired, err)
	}
	for _, fixture := range fixtures {
		var state, terminalCode, retirementCode string
		var published, authority bool
		if err := pool.QueryRow(ctx, `
SELECT j.state, COALESCE(j.terminal_error_code, ''),
       o.retirement_code, o.published_at IS NOT NULL,
       o.authority_granted_at IS NOT NULL
FROM elitea_runtime.execution_jobs AS j
JOIN elitea_runtime.command_outbox AS o
  ON o.execution_id = j.execution_id AND o.generation = j.generation
WHERE j.execution_id = $1 AND j.generation = 1`, fixture.frame.Fence.ExecutionID).Scan(&state, &terminalCode, &retirementCode, &published, &authority); err != nil {
			t.Fatal(err)
		}
		if state != "FAILED" || terminalCode != retirementCodeDeadlineExceeded || retirementCode != retirementCodeDeadlineExceeded || published != fixture.published || authority {
			t.Fatalf("unexpected deadline retirement state: state=%s terminal=%s retirement=%s published=%t authority=%t", state, terminalCode, retirementCode, published, authority)
		}
		decision, err := claims.ClaimValidation(ctx, executionapp.ClaimRequest{
			CommandID:            fixture.frame.Fence.CommandID,
			OutboxID:             fixture.seed.outboxID,
			ExecutionID:          fixture.frame.Fence.ExecutionID,
			Generation:           1,
			CapabilityID:         executiondomain.ConfigurationValidationCapability,
			SignedEnvelopeDigest: fixture.seed.envelopeDigest,
			WorkloadIdentity:     "spiffe://elitea.test/worker/retired",
			WorkloadSessionID:    "retired-session",
			ProducerID:           "retired-producer",
		}, executionapp.MaxClaimLeaseTTLMillis)
		if err != nil || decision.Disposition != executionapp.ClaimRetiredACK || decision.RetirementReason != executionapp.RetirementDeadlineExceeded || decision.Lease != (runtimedomain.ActiveLease{}) {
			t.Fatalf("leaked retired command did not receive typed ACK: decision=%+v err=%v", decision, err)
		}
		assertPostgresCount(t, ctx, pool, 0, `SELECT count(*) FROM elitea_runtime.execution_claims WHERE execution_id = $1`, fixture.frame.Fence.ExecutionID)
		assertPostgresCount(t, ctx, pool, 0, `SELECT count(*) FROM elitea_runtime.output_inbox WHERE execution_id = $1`, fixture.frame.Fence.ExecutionID)
		assertPostgresCount(t, ctx, pool, 0, `SELECT count(*) FROM elitea_runtime.execution_settlements WHERE execution_id = $1`, fixture.frame.Fence.ExecutionID)
		assertPostgresCount(t, ctx, pool, 1, `SELECT count(*) FROM elitea_runtime.execution_replay_events WHERE execution_id = $1 AND event_type = 'execution.failed' AND event_bytes = $2`, fixture.frame.Fence.ExecutionID, deadlineRetirementEventBytes)
	}

	// Cancellation and expiry contend on the same durable rows. Either serial
	// winner is valid, but the row must not remain non-terminal or produce mixed
	// FAILED/CANCELLED evidence.
	raceFrame := postgresValidationFrame(t, "cancel-deadline-race")
	raceSeed := seedPostgresValidationExecution(t, pool, raceFrame, runtimedomain.DesiredRunning)
	resetPostgresExecutionToNoAuthority(t, pool, raceFrame, raceSeed, true, runtimedomain.DesiredRunning, time.Now().UTC().Add(-time.Minute))
	start := make(chan struct{})
	errCh := make(chan error, 2)
	go func() {
		<-start
		_, raceErr := pool.Exec(ctx, `
UPDATE elitea_runtime.execution_jobs
SET desired_state = 'CANCELLED'
WHERE execution_id = $1 AND generation = 1
  AND state IN ('PENDING', 'DISPATCHED')`, raceFrame.Fence.ExecutionID)
		errCh <- raceErr
	}()
	go func() {
		<-start
		_, raceErr := repository.RetireNoAuthorityValidation(ctx, 1)
		errCh <- raceErr
	}()
	close(start)
	for range 2 {
		if err := <-errCh; err != nil {
			t.Fatalf("cancellation/deadline race: %v", err)
		}
	}
	_, _ = repository.RetireNoAuthorityValidation(ctx, 1)
	var raceState, raceDesired, raceCode string
	if err := pool.QueryRow(ctx, `
SELECT j.state, j.desired_state, o.retirement_code
FROM elitea_runtime.execution_jobs AS j
JOIN elitea_runtime.command_outbox AS o USING (execution_id, generation)
WHERE j.execution_id = $1`, raceFrame.Fence.ExecutionID).Scan(&raceState, &raceDesired, &raceCode); err != nil {
		t.Fatal(err)
	}
	if (raceState != "FAILED" || raceCode != retirementCodeDeadlineExceeded) &&
		(raceState != "CANCELLED" || raceDesired != "CANCELLED" || raceCode != retirementCodeCancelled) {
		t.Fatalf("mixed or stuck cancellation/deadline race: state=%s desired=%s code=%s", raceState, raceDesired, raceCode)
	}

	// A repository-accepted claim sets authority in the same transaction. Once
	// granted, moving the deadline into the past cannot retire the command.
	authorityFrame := postgresValidationFrame(t, "authority-exclusion")
	authoritySeed := seedPostgresValidationExecution(t, pool, authorityFrame, runtimedomain.DesiredRunning)
	resetPostgresExecutionToNoAuthority(t, pool, authorityFrame, authoritySeed, true, runtimedomain.DesiredRunning, time.Now().UTC().Add(time.Minute))
	decision, err := claims.ClaimValidation(ctx, executionapp.ClaimRequest{
		CommandID: authorityFrame.Fence.CommandID, OutboxID: authoritySeed.outboxID,
		ExecutionID: authorityFrame.Fence.ExecutionID, Generation: 1,
		CapabilityID:         executiondomain.ConfigurationValidationCapability,
		SignedEnvelopeDigest: authoritySeed.envelopeDigest,
		WorkloadIdentity:     "spiffe://elitea.test/worker/accepted", WorkloadSessionID: "accepted-session", ProducerID: "accepted-producer",
	}, executionapp.MaxClaimLeaseTTLMillis)
	if err != nil || decision.Disposition != executionapp.ClaimAccepted {
		t.Fatalf("accept claim before deadline: decision=%+v err=%v", decision, err)
	}
	if _, err := pool.Exec(ctx, `UPDATE elitea_runtime.command_outbox SET deadline = clock_timestamp() - interval '1 minute' WHERE outbox_id = $1`, authoritySeed.outboxID); err != nil {
		t.Fatal(err)
	}
	_, err = repository.RetireNoAuthorityValidation(ctx, 8)
	if err != nil {
		t.Fatal(err)
	}
	assertPostgresCount(t, ctx, pool, 1, `SELECT count(*) FROM elitea_runtime.command_outbox WHERE outbox_id = $1 AND authority_granted_at IS NOT NULL AND retired_at IS NULL`, authoritySeed.outboxID)
}

func assertPostgresPreparedEnvelopeCAS(t *testing.T, ctx context.Context, repository *CommandOutboxRepository, outboxID string) {
	t.Helper()
	candidates := []executionapp.PreparedCommandEnvelope{
		postgresPreparedEnvelope("concurrent-a"),
		postgresPreparedEnvelope("concurrent-b"),
	}
	type result struct {
		selected executionapp.StoredPreparedEnvelope
		err      error
	}
	start := make(chan struct{})
	results := make(chan result, len(candidates))
	for _, candidate := range candidates {
		candidate := candidate
		go func() {
			<-start
			selected, err := repository.StorePreparedValidation(ctx, outboxID, candidate)
			results <- result{selected: selected, err: err}
		}()
	}
	close(start)

	selected := make([]executionapp.StoredPreparedEnvelope, 0, len(candidates))
	for range candidates {
		result := <-results
		if result.err != nil {
			t.Fatalf("concurrent prepared-envelope CAS: %v", result.err)
		}
		selected = append(selected, result.selected)
	}
	if selected[0].Envelope.Digest != selected[1].Envelope.Digest || !bytes.Equal(selected[0].Envelope.Bytes, selected[1].Envelope.Bytes) || selected[0].Envelope.KeyID != selected[1].Envelope.KeyID {
		t.Fatalf("concurrent publishers selected different durable envelopes: %+v", selected)
	}
	if selected[0].Envelope.Digest != candidates[0].Digest && selected[0].Envelope.Digest != candidates[1].Digest {
		t.Fatal("database selected bytes outside the bounded candidate set")
	}
	loaded, err := repository.LoadPreparedValidation(ctx, outboxID)
	if err != nil {
		t.Fatalf("reload prepared CAS winner: %v", err)
	}
	if loaded == nil || loaded.Published || loaded.Envelope.Digest != selected[0].Envelope.Digest || !bytes.Equal(loaded.Envelope.Bytes, selected[0].Envelope.Bytes) {
		t.Fatalf("reloaded prepared winner changed: %+v", loaded)
	}
}

func postgresPreparedEnvelope(suffix string) executionapp.PreparedCommandEnvelope {
	encoded := []byte("signed-envelope:" + suffix)
	return executionapp.PreparedCommandEnvelope{
		Bytes:            encoded,
		Digest:           runtimedomain.SHA256(encoded),
		SignatureProfile: 1,
		KeyID:            "postgres-key-" + suffix,
	}
}

type postgresValidationSeed struct {
	claimID        string
	outboxID       string
	envelopeDigest runtimedomain.Digest
}

type postgresOutboxOrderingFixture struct {
	outboxID    string
	executionID string
	generation  int64
	state       string
	capability  string
	stream      string
	createdAt   time.Time
	published   bool
}

// postgresIntegrationDeadline bounds each stage of the shared PostgreSQL
// harness: connect and CREATE DATABASE, and the drop at cleanup.
//
// It used to be 20 s. Applying the embedded shared and tenant migration set
// costs about 1.5 s to 3 s on an idle machine, so 20 s looked generous. It is
// not. A run of this package on a busy machine failed with
// "apply shared/0073_mcp_tool_registry.sql: timeout: context deadline
// exceeded". The test was not wrong, and the migration was not hung. The
// machine was slow. The failure then reads as a broken migration rather than
// as a slow suite, which is expensive to diagnose (#409 item 2).
//
// The migration replay is no longer on this path (#425). The template build
// pays it one time under its own deadline. What is left here is CREATE
// DATABASE and DROP DATABASE, which queue behind the same statements from
// every other package in a parallel run. That queue is server load, not a
// hang, so the deadline must still sit far from the real cost. A true hang is
// still bounded: scripts/go/workspace-run.sh caps the whole package.
const postgresIntegrationDeadline = 120 * time.Second

// postgresIntegrationTenant is the legacy project whose tenant history the
// seed and the template cover.
const postgresIntegrationTenant int64 = 1

// postgresIntegrationTemplate names the template database that
// newMigratedPostgresIntegrationPool copies. TestMain sets it.
var postgresIntegrationTemplate string

// postgresIntegrationURL resolves the PostgreSQL server for this package.
func postgresIntegrationURL() string {
	databaseURL := os.Getenv(postgresIntegrationDatabaseURL)
	if databaseURL == "" && os.Getenv("ELITEA_TEST_USE_SERVICE_DATABASE_URL") == "1" {
		databaseURL = os.Getenv("DATABASE_URL")
	}
	return databaseURL
}

// TestMain builds one template database for the whole package.
//
// Before #425 each of the 78 migrated tests replayed the ledgered migration
// corpus into its own database. One replay costs about 1.5 s to 3 s, so the
// replays were most of the package duration and pushed it past the 600 s Go
// default timeout. The template holds the same schema, and every test copies
// it with CREATE DATABASE ... TEMPLATE.
//
// The template is built by the same migrate.Runner that a deployment uses, and
// its name carries the SHA-256 checksum of every migration. A new migration
// makes a new name, so a stale template can never be selected.
func TestMain(m *testing.M) {
	databaseURL := postgresIntegrationURL()
	if databaseURL == "" {
		// Every PostgreSQL test in this package skips itself. Run the rest.
		os.Exit(m.Run())
	}

	ctx, cancel := dbtest.BuildContext(context.Background())
	adminConfig, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		fmt.Fprintf(os.Stderr, "parse %s: %v\n", postgresIntegrationDatabaseURL, err)
		cancel()
		os.Exit(1)
	}
	adminConfig.MaxConns = 4
	adminPool, err := pgxpool.NewWithConfig(ctx, adminConfig)
	if err != nil {
		fmt.Fprintf(os.Stderr, "open PostgreSQL admin pool: %v\n", err)
		cancel()
		os.Exit(1)
	}
	templateName, err := dbtest.EnsureTemplate(ctx, adminPool, postgresIntegrationSpec())
	adminPool.Close()
	cancel()
	if err != nil {
		fmt.Fprintf(os.Stderr, "build PostgreSQL integration template: %v\n", err)
		os.Exit(1)
	}
	postgresIntegrationTemplate = templateName
	os.Exit(m.Run())
}

// postgresIntegrationSpec describes the migrated schema that the template
// holds. The migration corpus is the embedded ledgered corpus, never a dump.
func postgresIntegrationSpec() dbtest.Spec {
	return dbtest.Spec{
		Files:   platformmigrations.Files,
		Seed:    postgresIntegrationSeedSQL,
		Tenants: []int64{postgresIntegrationTenant},
	}
}

// newMigratedPostgresIntegrationPool returns a private database that already
// holds the seed and the full ledgered migration history.
func newMigratedPostgresIntegrationPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	if postgresIntegrationURL() == "" {
		t.Skipf("set %s to run the PostgreSQL 16-18 service-integration test", postgresIntegrationDatabaseURL)
	}
	if postgresIntegrationTemplate == "" {
		t.Fatalf("TestMain did not build the PostgreSQL integration template")
	}
	pool := newPostgresIntegrationPoolFromTemplate(t, postgresIntegrationTemplate)
	verifyPostgresIntegrationSchema(t, pool)
	return pool
}

// newPostgresIntegrationPool returns a private empty database.
func newPostgresIntegrationPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	return newPostgresIntegrationPoolFromTemplate(t, "")
}

func newPostgresIntegrationPoolFromTemplate(t *testing.T, templateName string) *pgxpool.Pool {
	t.Helper()
	databaseURL := postgresIntegrationURL()
	if databaseURL == "" {
		t.Skipf("set %s to run the PostgreSQL 16-18 service-integration test", postgresIntegrationDatabaseURL)
	}
	ctx, cancel := context.WithTimeout(context.Background(), postgresIntegrationDeadline)
	defer cancel()

	adminConfig, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		t.Fatalf("parse %s: %v", postgresIntegrationDatabaseURL, err)
	}
	adminConfig.MaxConns = 4
	adminPool, err := pgxpool.NewWithConfig(ctx, adminConfig)
	if err != nil {
		t.Fatalf("open PostgreSQL admin pool: %v", err)
	}
	if err := adminPool.Ping(ctx); err != nil {
		adminPool.Close()
		t.Fatalf("ping PostgreSQL: %v", err)
	}

	databaseName := fmt.Sprintf("elitea_it_%d_%d", os.Getpid(), time.Now().UnixNano())
	quotedDatabase := pgx.Identifier{databaseName}.Sanitize()
	if templateName == "" {
		if _, err := adminPool.Exec(ctx, "CREATE DATABASE "+quotedDatabase); err != nil {
			adminPool.Close()
			t.Fatalf("create isolated PostgreSQL integration database: %v", err)
		}
	} else if err := dbtest.CreateFromTemplate(ctx, adminPool, templateName, databaseName); err != nil {
		adminPool.Close()
		t.Fatalf("create isolated PostgreSQL integration database: %v", err)
	}

	testConfig := adminConfig.Copy()
	testConfig.ConnConfig.Database = databaseName
	testConfig.MaxConns = 12
	pool, err := pgxpool.NewWithConfig(ctx, testConfig)
	if err != nil {
		_, _ = adminPool.Exec(context.Background(), "DROP DATABASE "+quotedDatabase+" WITH (FORCE)")
		adminPool.Close()
		t.Fatalf("open isolated PostgreSQL integration database: %v", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		_, _ = adminPool.Exec(context.Background(), "DROP DATABASE "+quotedDatabase+" WITH (FORCE)")
		adminPool.Close()
		t.Fatalf("ping isolated PostgreSQL integration database: %v", err)
	}

	var serverVersion int
	if err := pool.QueryRow(ctx, "SELECT current_setting('server_version_num')::integer").Scan(&serverVersion); err != nil {
		pool.Close()
		_, _ = adminPool.Exec(context.Background(), "DROP DATABASE "+quotedDatabase+" WITH (FORCE)")
		adminPool.Close()
		t.Fatalf("read PostgreSQL server version: %v", err)
	}
	if serverVersion < 160000 || serverVersion >= 190000 {
		pool.Close()
		_, _ = adminPool.Exec(context.Background(), "DROP DATABASE "+quotedDatabase+" WITH (FORCE)")
		adminPool.Close()
		t.Fatalf("service-integration gate requires PostgreSQL 16 through 18, got server_version_num=%d", serverVersion)
	}

	t.Cleanup(func() {
		pool.Close()
		dropCtx, dropCancel := context.WithTimeout(context.Background(), postgresIntegrationDeadline)
		defer dropCancel()
		if _, err := adminPool.Exec(dropCtx, "DROP DATABASE "+quotedDatabase+" WITH (FORCE)"); err != nil {
			t.Errorf("drop isolated PostgreSQL integration database: %v", err)
		}
		adminPool.Close()
	})
	return pool
}

// postgresIntegrationSeedSQL is the minimum legacy project schema. It runs
// before the ledgered migrations, because the tenant history alters these
// legacy tables. It is part of the template fingerprint, so an edit here makes
// a new template.
const postgresIntegrationSeedSQL = `
CREATE SCHEMA centry;
CREATE TABLE centry.project (
    id INTEGER PRIMARY KEY,
    create_success BOOLEAN NOT NULL DEFAULT TRUE,
    suspended BOOLEAN NOT NULL DEFAULT FALSE
);
INSERT INTO centry.project (id, create_success, suspended)
VALUES (1, TRUE, FALSE);
CREATE TABLE centry.notifications (
    id SERIAL PRIMARY KEY,
    uuid UUID NOT NULL UNIQUE,
    is_seen BOOLEAN NOT NULL,
    project_id INTEGER NOT NULL,
    user_id INTEGER NOT NULL,
    meta JSONB NOT NULL,
    event_type VARCHAR NOT NULL,
    created_at TIMESTAMP NOT NULL DEFAULT now(),
    updated_at TIMESTAMP
);
CREATE INDEX ix_centry_notifications_user_id
    ON centry.notifications (user_id);
CREATE SCHEMA p_1;
CREATE TABLE p_1.application_versions (
    id SERIAL PRIMARY KEY,
    llm_settings JSONB NOT NULL DEFAULT '{}'::jsonb
);
-- The skills pair is part of the "minimum legacy project schema" because the
-- tenant history now alters it (tenant/0122_skill_publishing.sql, #249), the
-- same way application_versions above is here for 0121. It is defined HERE and
-- nowhere else in this package: seedCurrentAgentContinuationSchema (#268) used
-- to declare its own copy, and two CREATE TABLEs for one table is a duplicate-
-- relation error for every test that runs both. The author/owner columns carry
-- fixture defaults so the terser inserts in that seed still work.
CREATE TABLE p_1.skills (
    id SERIAL PRIMARY KEY,
    name VARCHAR(128) NOT NULL,
    description VARCHAR(2304) NOT NULL,
    owner_id INTEGER NOT NULL DEFAULT 1,
    author_id INTEGER NOT NULL DEFAULT 1,
    meta JSONB DEFAULT '{}'::jsonb
);
CREATE TABLE p_1.skill_versions (
    id SERIAL PRIMARY KEY,
    skill_id INTEGER NOT NULL REFERENCES p_1.skills(id) ON DELETE CASCADE,
    name VARCHAR(128) NOT NULL DEFAULT 'base',
    instructions TEXT NOT NULL,
    author_id INTEGER NOT NULL DEFAULT 1,
    meta JSONB DEFAULT '{}'::jsonb
);
CREATE TABLE p_1.configuration (
    id SERIAL PRIMARY KEY,
    uuid UUID NOT NULL UNIQUE,
    project_id INTEGER NOT NULL,
    label VARCHAR,
    elitea_title VARCHAR NOT NULL UNIQUE,
    type VARCHAR NOT NULL,
    section VARCHAR NOT NULL,
    data JSONB NOT NULL,
    meta JSONB NOT NULL,
    shared BOOLEAN NOT NULL,
    status_ok BOOLEAN NOT NULL,
    status_logs TEXT,
    source VARCHAR NOT NULL,
    author_id INTEGER,
    created_at TIMESTAMP NOT NULL DEFAULT now(),
    updated_at TIMESTAMP
);
INSERT INTO p_1.configuration (
    id, uuid, project_id, label, elitea_title, type, section, data, meta,
    shared, status_ok, status_logs, source, author_id
) VALUES (
    1, '00000000-0000-0000-0000-000000000001', 1, 'Integration fixture',
    'integration_fixture', 'openapi', 'credentials', '{}'::jsonb, '{}'::jsonb,
    false, false, NULL, 'user', 1
);
SELECT setval(pg_get_serial_sequence('p_1.configuration', 'id'), (SELECT MAX(id) FROM p_1.configuration));`

// verifyPostgresIntegrationSchema proves that the database this test received
// carries the ledgered migration history, not a copy that drifted from it.
//
// It runs on every copied database, so a template that does not match the
// corpus fails the first test that uses it. CheckHead compares the recorded
// SHA-256 of every migration against the embedded corpus. The column check
// then proves that the tenant history really ran against the legacy table.
func verifyPostgresIntegrationSchema(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), postgresIntegrationDeadline)
	defer cancel()

	runner := migrate.New(pool, platformmigrations.Files)
	if err := runner.CheckHead(ctx, migrate.ScopeShared, "platform"); err != nil {
		t.Fatalf("verify shared migration head: %v", err)
	}
	if err := runner.CheckHead(ctx, migrate.ScopeTenant, "1"); err != nil {
		t.Fatalf("verify tenant migration head: %v", err)
	}
	expectedConfigurationColumns := []string{
		"id", "uuid", "project_id", "label", "elitea_title", "type", "section",
		"data", "meta", "shared", "status_ok", "status_logs", "source",
		"author_id", "created_at", "updated_at",
	}
	var configurationColumns []string
	if err := pool.QueryRow(ctx, `
SELECT array_agg(column_name::text ORDER BY ordinal_position)
FROM information_schema.columns
WHERE table_schema = 'p_1' AND table_name = 'configuration'`).Scan(&configurationColumns); err != nil {
		t.Fatalf("read legacy configuration columns after tenant migration: %v", err)
	}
	if strings.Join(configurationColumns, ",") != strings.Join(expectedConfigurationColumns, ",") {
		t.Fatalf("tenant migration changed the legacy configuration shape: got %v", configurationColumns)
	}
}

func seedPostgresOutboxOrderingFixtures(t *testing.T, pool *pgxpool.Pool, base time.Time) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	tx, err := pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()

	manifest := []byte{1}
	digest := runtimedomain.SHA256(manifest)
	postgresMustExec(t, ctx, tx, `
INSERT INTO elitea_runtime.input_bundles (
    input_bundle_id, immutable_version, media_type, resource_project_id,
    manifest_digest, manifest_size, manifest_bytes, created_by
) VALUES ('bundle-outbox-order', 'v1', 'application/x-protobuf', 1,
          $1, $2, $3, 'postgres-outbox-order-test')`,
		digest[:], int64(len(manifest)), manifest)

	fixtures := []postgresOutboxOrderingFixture{
		// A later generation has the oldest timestamp on purpose. Phase one must
		// leave it undispatched even after generation one is published.
		{outboxID: "outbox-order-a-2", executionID: "execution-order-a", generation: 2, state: "PENDING", capability: "configuration.validate.v1", stream: "elitea:runtime:commands", createdAt: base.Add(-10 * time.Minute)},
		{outboxID: "outbox-order-wrong-stream", executionID: "execution-order-wrong-stream", generation: 1, state: "PENDING", capability: "configuration.validate.v1", stream: "other:runtime:commands", createdAt: base.Add(-9 * time.Minute)},
		{outboxID: "outbox-order-published", executionID: "execution-order-published", generation: 1, state: "PENDING", capability: "configuration.validate.v1", stream: "elitea:runtime:commands", createdAt: base.Add(-8 * time.Minute), published: true},
		{outboxID: "outbox-order-dispatched", executionID: "execution-order-dispatched", generation: 1, state: "DISPATCHED", capability: "configuration.validate.v1", stream: "elitea:runtime:commands", createdAt: base.Add(-7 * time.Minute)},
		// index.ingest.v1 is the other capability admitted by the shared
		// execution table. It is a valid row, but must never be selected by the
		// configuration-validation outbox reader.
		{outboxID: "outbox-order-wrong-capability", executionID: "execution-order-wrong-capability", generation: 1, state: "PENDING", capability: "index.ingest.v1", stream: "elitea:runtime:commands", createdAt: base.Add(-6 * time.Minute)},
		{outboxID: "outbox-order-b-1", executionID: "execution-order-b", generation: 1, state: "PENDING", capability: "configuration.validate.v1", stream: "elitea:runtime:commands", createdAt: base.Add(time.Minute)},
		{outboxID: "outbox-order-a-1", executionID: "execution-order-a", generation: 1, state: "PENDING", capability: "configuration.validate.v1", stream: "elitea:runtime:commands", createdAt: base.Add(2 * time.Minute)},
	}

	for _, fixture := range fixtures {
		commandID := "command-" + fixture.outboxID
		postgresMustExec(t, ctx, tx, `
INSERT INTO elitea_runtime.execution_jobs (
    execution_id, generation, command_id, tenant_id,
    resource_project_id, projection_project_id, actor_id, principal_ref,
    capability_id, capability_version, input_bundle_id,
    request_digest, idempotency_scope, idempotency_key,
    configuration_revision_id, configuration_type, catalog_revision,
    catalog_digest, schema_id, schema_revision, schema_digest,
    settings_entry_id, state, desired_state
) VALUES (
    $1, $2, $3, 'tenant-postgres',
    1, 1, 'actor-postgres-test', 'principal-postgres-test',
    $4, 'v1', 'bundle-outbox-order',
    $5, 'postgres-outbox-order', $3,
    CASE WHEN $4 = 'configuration.validate.v1' THEN $6::text END,
    CASE WHEN $4 = 'configuration.validate.v1' THEN 'openapi' END,
    CASE WHEN $4 = 'configuration.validate.v1' THEN 'catalog-postgres-v1' END,
    CASE WHEN $4 = 'configuration.validate.v1' THEN $5::bytea END,
    CASE WHEN $4 = 'configuration.validate.v1' THEN 'openapi' END,
    CASE WHEN $4 = 'configuration.validate.v1' THEN 'schema-postgres-v1' END,
    CASE WHEN $4 = 'configuration.validate.v1' THEN $5::bytea END,
    CASE WHEN $4 = 'configuration.validate.v1' THEN 'settings-postgres-outbox-order' END,
    $7, 'RUNNING'
)`,
			fixture.executionID,
			fixture.generation,
			commandID,
			fixture.capability,
			digest[:],
			"revision-"+fixture.outboxID,
			fixture.state,
		)

		var publishedAt any
		var publishedDigest any
		var preparedBytes any
		var preparedDigest any
		var preparedProfile any
		var preparedKeyID any
		var preparedAt any
		if fixture.published {
			publishedAt = time.Now().UTC()
			storedBytes := []byte(fixture.outboxID + ":published")
			storedDigest := runtimedomain.SHA256(storedBytes)
			publishedDigest = storedDigest[:]
			preparedBytes = storedBytes
			preparedDigest = storedDigest[:]
			preparedProfile = int32(1)
			preparedKeyID = "postgres-fixture-key"
			preparedAt = fixture.createdAt.Add(15 * time.Second)
		}
		postgresMustExec(t, ctx, tx, `
INSERT INTO elitea_runtime.command_outbox (
    outbox_id, execution_id, generation, stream_name, dispatch_ordinal,
    resource_class, isolation_class, priority, deadline, limits_revision,
    created_at, prepared_signed_envelope_bytes,
    prepared_signed_envelope_digest, prepared_signature_profile,
    prepared_key_id, prepared_at,
    published_at, last_visibility_at, published_envelope_digest, publish_attempts
) VALUES ($1, $2, $3, $4, 1,
          'validation', 'project', 1, $5, 'limits-v1',
          $6, $7, $8, $9, $10, $11, $12, NULL, $13, $14)`,
			fixture.outboxID,
			fixture.executionID,
			fixture.generation,
			fixture.stream,
			time.Now().UTC().Add(time.Hour),
			fixture.createdAt,
			preparedBytes,
			preparedDigest,
			preparedProfile,
			preparedKeyID,
			preparedAt,
			publishedAt,
			publishedDigest,
			map[bool]int32{false: 0, true: 1}[fixture.published],
		)
	}

	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit PostgreSQL outbox ordering fixtures: %v", err)
	}
}

func equalPostgresStrings(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

func assertPostgresOutboxDispatchIndex(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	var definition string
	if err := pool.QueryRow(ctx, `
SELECT indexdef
FROM pg_indexes
WHERE schemaname = 'elitea_runtime'
  AND tablename = 'command_outbox'
  AND indexname = 'command_outbox_unpublished_idx'`).Scan(&definition); err != nil {
		t.Fatalf("load PostgreSQL outbox dispatch index: %v", err)
	}
	for _, fragment := range []string{
		"(stream_name, created_at, outbox_id)",
		"INCLUDE (execution_id, generation)",
		"published_at IS NULL",
		"retired_at IS NULL",
	} {
		if !strings.Contains(definition, fragment) {
			t.Fatalf("PostgreSQL outbox dispatch index is missing %q: %s", fragment, definition)
		}
	}
	if err := pool.QueryRow(ctx, `
SELECT indexdef
FROM pg_indexes
WHERE schemaname = 'elitea_runtime'
  AND tablename = 'command_outbox'
  AND indexname = 'command_outbox_visibility_repair_idx'`).Scan(&definition); err != nil {
		t.Fatalf("load PostgreSQL visibility-repair index: %v", err)
	}
	for _, fragment := range []string{
		"stream_name",
		"COALESCE(last_visibility_at, published_at)",
		"outbox_id",
		"published_at IS NOT NULL",
		"authority_granted_at IS NULL",
		"retired_at IS NULL",
	} {
		if !strings.Contains(definition, fragment) {
			t.Fatalf("PostgreSQL visibility-repair index is missing %q: %s", fragment, definition)
		}
	}
}

func assertPostgresRetirementIndexes(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	tests := []struct {
		name      string
		fragments []string
	}{
		{
			name: "command_outbox_deadline_idx",
			fragments: []string{
				"(stream_name, deadline, outbox_id)",
				"retired_at IS NULL",
				"authority_granted_at IS NULL",
			},
		},
		{
			name: "execution_jobs_cancel_pending_idx",
			fragments: []string{
				"(capability_id, generation, admitted_at, execution_id)",
				"desired_state = 'CANCELLED'",
				"state = ANY (ARRAY['PENDING'::text, 'DISPATCHED'::text])",
			},
		},
	}
	for _, test := range tests {
		var definition string
		if err := pool.QueryRow(ctx, `SELECT indexdef FROM pg_indexes WHERE schemaname = 'elitea_runtime' AND indexname = $1`, test.name).Scan(&definition); err != nil {
			t.Fatalf("load PostgreSQL retirement index %s: %v", test.name, err)
		}
		for _, fragment := range test.fragments {
			if !strings.Contains(definition, fragment) {
				t.Fatalf("PostgreSQL retirement index %s is missing %q: %s", test.name, fragment, definition)
			}
		}
	}
}

func postgresValidationFrame(t *testing.T, suffix string) outputapp.ConfigurationValidationFrame {
	t.Helper()
	executionID := "execution-postgres-" + suffix
	commandID := "command-postgres-" + suffix
	revisionID := "revision-postgres-" + suffix
	bundleID := "bundle-postgres-" + suffix
	settingsEntryID := "settings-postgres-" + suffix
	manifestBytes := []byte("manifest:" + bundleID)
	settingsBytes := []byte("{}\n")

	frame := outputapp.ConfigurationValidationFrame{
		StreamID:            executionID + ":1",
		TenantID:            "tenant-postgres",
		ResourceProjectID:   "1",
		ProjectionProjectID: "1",
		WorkloadSessionID:   "session-postgres-" + suffix,
		ProducerID:          "producer-postgres-" + suffix,
		EventID:             commandID + ":1",
		LogicalOutputID:     "configuration-validation:" + revisionID,
		Sequence:            1,
		OccurredAt:          time.Date(2026, time.July, 17, 9, 30, 0, 0, time.UTC),
	}
	frame.Fence = runtimedomain.Fence{
		CommandID:         commandID,
		ExecutionID:       executionID,
		Generation:        1,
		WorkloadIdentity:  "spiffe://elitea.test/worker/" + suffix,
		WorkloadSessionID: frame.WorkloadSessionID,
		ProducerID:        frame.ProducerID,
		ClaimAttempt:      1,
		LeaseEpoch:        1,
		Token:             runtimedomain.FenceToken(runtimedomain.SHA256([]byte("token:" + suffix))),
	}
	frame.Result = configurationdomain.ValidationResult{
		Binding: configurationdomain.ValidationBinding{
			Command: configurationdomain.ValidationCommand{
				ConfigurationRevisionID: revisionID,
				ConfigurationType:       "openapi",
				CatalogRevision:         "catalog-postgres-v1",
				CatalogDigest:           runtimedomain.SHA256([]byte("catalog:" + suffix)),
				SchemaID:                "openapi",
				SchemaRevision:          "schema-postgres-v1",
				SchemaDigest:            runtimedomain.SHA256([]byte("schema:" + suffix)),
				SettingsEntryID:         settingsEntryID,
			},
			InputBundleID:         bundleID,
			InputBundleDigest:     runtimedomain.SHA256(manifestBytes),
			SettingsEntryVersion:  revisionID,
			SettingsContentDigest: runtimedomain.SHA256(settingsBytes),
		},
		Valid: true,
	}

	result := &runtimev1.ConfigurationValidationResultV1{
		ConfigurationRevisionId: revisionID,
		ConfigurationType:       frame.Result.Binding.Command.ConfigurationType,
		CatalogRevision:         frame.Result.Binding.Command.CatalogRevision,
		CatalogDigest:           postgresDigestV1(frame.Result.Binding.Command.CatalogDigest),
		SchemaId:                frame.Result.Binding.Command.SchemaID,
		SchemaRevision:          frame.Result.Binding.Command.SchemaRevision,
		SchemaDigest:            postgresDigestV1(frame.Result.Binding.Command.SchemaDigest),
		InputBundleId:           bundleID,
		InputBundleDigest:       postgresDigestV1(frame.Result.Binding.InputBundleDigest),
		SettingsEntryId:         settingsEntryID,
		SettingsEntryVersion:    frame.Result.Binding.SettingsEntryVersion,
		SettingsContentDigest:   postgresDigestV1(frame.Result.Binding.SettingsContentDigest),
		Valid:                   true,
	}
	encodedResult, err := proto.MarshalOptions{Deterministic: true}.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	frame.EncodedResult = encodedResult
	frame.PayloadDigest = runtimedomain.SHA256(encodedResult)
	frame.Settlement = executionapp.SettlementProposal{
		Fence:                   frame.Fence,
		ProposalID:              commandID + ":settlement",
		Outcome:                 executionapp.SettlementSucceeded,
		TerminalLogicalOutputID: frame.LogicalOutputID,
		TerminalEventID:         frame.EventID,
		TerminalSequence:        frame.Sequence,
		TerminalPayloadDigest:   frame.PayloadDigest,
		IdempotencyKey:          commandID + ":prepare-settlement",
	}
	wireProposal := &runtimev1.SettlementProposalV1{
		ProposalId:              frame.Settlement.ProposalID,
		RequestedOutcome:        runtimev1.ExecutionOutcomeV1_EXECUTION_OUTCOME_V1_SUCCEEDED,
		TerminalLogicalOutputId: frame.LogicalOutputID,
		TerminalEventId:         frame.EventID,
		TerminalSequence:        frame.Sequence,
		TerminalPayloadDigest:   postgresDigestV1(frame.PayloadDigest),
		PrepareIdempotencyKey:   frame.Settlement.IdempotencyKey,
	}
	frame.EncodedSettlement, err = proto.MarshalOptions{Deterministic: true}.Marshal(wireProposal)
	if err != nil {
		t.Fatal(err)
	}
	frame.Settlement.ProposalDigest = runtimedomain.SHA256(frame.EncodedSettlement)
	if err := frame.Validate(); err != nil {
		t.Fatalf("build PostgreSQL integration frame: %v", err)
	}
	return frame
}

func postgresDigestV1(digest runtimedomain.Digest) *runtimev1.DigestV1 {
	return &runtimev1.DigestV1{
		Algorithm: runtimev1.DigestAlgorithmV1_DIGEST_ALGORITHM_V1_SHA256,
		Value:     append([]byte(nil), digest[:]...),
	}
}

func postgresDigestBytes(digest runtimedomain.Digest) []byte {
	return append([]byte(nil), digest[:]...)
}

func seedPostgresValidationExecution(t *testing.T, pool *pgxpool.Pool, frame outputapp.ConfigurationValidationFrame, desired runtimedomain.DesiredState) postgresValidationSeed {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	tx, err := pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()

	binding := frame.Result.Binding
	manifestBytes := []byte("manifest:" + binding.InputBundleID)
	settingsBytes := []byte("{}\n")
	claimID := "claim-postgres-" + frame.Fence.CommandID
	outboxID := "outbox-postgres-" + frame.Fence.CommandID
	envelopeBytes := []byte("signed-envelope:" + frame.Fence.CommandID)
	envelopeDigest := runtimedomain.SHA256(envelopeBytes)
	now := time.Now().UTC()
	claimedAt := now.Add(-2 * time.Hour)
	leaseExpiresAt := now.Add(30 * time.Minute)

	postgresMustExec(t, ctx, tx, `
INSERT INTO elitea_runtime.input_bundles (
    input_bundle_id, immutable_version, media_type, resource_project_id,
    manifest_digest, manifest_size, manifest_bytes, created_by
) VALUES ($1, $2, 'application/x-protobuf', 1, $3, $4, $5, $6)`,
		binding.InputBundleID,
		binding.SettingsEntryVersion,
		binding.InputBundleDigest[:],
		int64(len(manifestBytes)),
		manifestBytes,
		"postgres-integration-test",
	)
	postgresMustExec(t, ctx, tx, `
INSERT INTO elitea_runtime.input_bundle_entries (
    input_bundle_id, entry_id, entry_version, semantic_role, media_type,
    content_digest, content_size, content_reference, classification,
    required_grant_audience, content_bytes
) VALUES ($1, $2, $3, 'configuration.settings', 'application/json',
          $4, $5, $6, 'internal', 'elitea-main', $7)`,
		binding.InputBundleID,
		binding.Command.SettingsEntryID,
		binding.SettingsEntryVersion,
		binding.SettingsContentDigest[:],
		int64(len(settingsBytes)),
		"content://postgres-integration/"+binding.Command.SettingsEntryID,
		settingsBytes,
	)
	postgresMustExec(t, ctx, tx, `
INSERT INTO p_1.configuration_revisions (
    revision_id, configuration_id, configuration_type, settings_entry_id,
    settings_entry_version, settings_content_digest, input_bundle_id,
    catalog_revision, catalog_digest, schema_id, schema_revision,
    schema_digest, created_by
) VALUES ($1, 1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)`,
		binding.Command.ConfigurationRevisionID,
		binding.Command.ConfigurationType,
		binding.Command.SettingsEntryID,
		binding.SettingsEntryVersion,
		binding.SettingsContentDigest[:],
		binding.InputBundleID,
		binding.Command.CatalogRevision,
		binding.Command.CatalogDigest[:],
		binding.Command.SchemaID,
		binding.Command.SchemaRevision,
		binding.Command.SchemaDigest[:],
		"postgres-integration-test",
	)
	postgresMustExec(t, ctx, tx, `
INSERT INTO elitea_runtime.execution_jobs (
    execution_id, generation, command_id, tenant_id,
    resource_project_id, projection_project_id, actor_id, principal_ref,
    capability_id, capability_version, input_bundle_id,
    request_digest, idempotency_scope, idempotency_key,
    configuration_revision_id, configuration_type, catalog_revision,
    catalog_digest, schema_id, schema_revision, schema_digest,
    settings_entry_id, state, desired_state
) VALUES (
    $1, $2, $3, $4, 1, 1, $5, $6,
    'configuration.validate.v1', 'v1', $7, $8, $9, $10,
    $11, $12, $13, $14, $15, $16, $17, $18, 'CLAIMED', $19
)`,
		frame.Fence.ExecutionID,
		int64(frame.Fence.Generation),
		frame.Fence.CommandID,
		frame.TenantID,
		"actor-postgres-test",
		"principal-postgres-test",
		binding.InputBundleID,
		postgresDigestBytes(runtimedomain.SHA256([]byte("request:"+frame.Fence.CommandID))),
		"postgres-integration",
		frame.Fence.CommandID,
		binding.Command.ConfigurationRevisionID,
		binding.Command.ConfigurationType,
		binding.Command.CatalogRevision,
		binding.Command.CatalogDigest[:],
		binding.Command.SchemaID,
		binding.Command.SchemaRevision,
		binding.Command.SchemaDigest[:],
		binding.Command.SettingsEntryID,
		string(desired),
	)
	postgresMustExec(t, ctx, tx, `
INSERT INTO elitea_runtime.command_outbox (
    outbox_id, execution_id, generation, stream_name, resource_class,
    isolation_class, priority, deadline, limits_revision,
    prepared_signed_envelope_bytes, prepared_signed_envelope_digest,
    prepared_signature_profile, prepared_key_id, prepared_at,
    published_at, last_visibility_at, published_envelope_digest, authority_granted_at, publish_attempts
) VALUES ($1, $2, $3, 'elitea:runtime:commands', 'validation',
          'project', 1, $4, 'limits-v1', $5, $6, 1,
          'postgres-cancellation-key', $7, $7, $7, $6, $7, 1)`,
		outboxID,
		frame.Fence.ExecutionID,
		int64(frame.Fence.Generation),
		now.Add(time.Hour),
		envelopeBytes,
		envelopeDigest[:],
		now,
	)
	postgresMustExec(t, ctx, tx, `
INSERT INTO elitea_runtime.execution_claims (
    claim_id, execution_id, generation, workload_session_id,
    workload_identity, producer_id, claim_attempt, lease_epoch,
    fence_token, claimed_at, lease_expires_at
) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)`,
		claimID,
		frame.Fence.ExecutionID,
		int64(frame.Fence.Generation),
		frame.Fence.WorkloadSessionID,
		frame.Fence.WorkloadIdentity,
		frame.Fence.ProducerID,
		int64(frame.Fence.ClaimAttempt),
		int64(frame.Fence.LeaseEpoch),
		frame.Fence.Token[:],
		claimedAt,
		leaseExpiresAt,
	)
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit PostgreSQL integration seed: %v", err)
	}
	return postgresValidationSeed{claimID: claimID, outboxID: outboxID, envelopeDigest: envelopeDigest}
}

func resetPostgresExecutionToNoAuthority(t *testing.T, pool *pgxpool.Pool, frame outputapp.ConfigurationValidationFrame, seed postgresValidationSeed, published bool, desired runtimedomain.DesiredState, deadline time.Time) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	if _, err := tx.Exec(ctx, `DELETE FROM elitea_runtime.execution_claims WHERE execution_id = $1 AND generation = 1`, frame.Fence.ExecutionID); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `
UPDATE elitea_runtime.execution_jobs
SET state = 'DISPATCHED', desired_state = $2,
    settled_at = NULL, terminal_error_code = NULL
WHERE execution_id = $1 AND generation = 1`, frame.Fence.ExecutionID, string(desired)); err != nil {
		t.Fatal(err)
	}
	if published {
		_, err = tx.Exec(ctx, `
UPDATE elitea_runtime.command_outbox
SET deadline = $2, authority_granted_at = NULL,
    retired_at = NULL, retirement_code = NULL
WHERE outbox_id = $1`, seed.outboxID, deadline.UTC())
	} else {
		_, err = tx.Exec(ctx, `
UPDATE elitea_runtime.command_outbox
SET deadline = $2, authority_granted_at = NULL,
    published_at = NULL, published_envelope_digest = NULL,
    last_visibility_at = NULL,
    publish_attempts = 0, retired_at = NULL, retirement_code = NULL
WHERE outbox_id = $1`, seed.outboxID, deadline.UTC())
	}
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
}

func postgresMustExec(t *testing.T, ctx context.Context, tx pgx.Tx, query string, args ...any) {
	t.Helper()
	if _, err := tx.Exec(ctx, query, args...); err != nil {
		t.Fatalf("seed PostgreSQL integration fixture: %v", err)
	}
}

func newPostgresValidationService(t *testing.T, pool *pgxpool.Pool) *outputapp.ConfigurationValidationService {
	t.Helper()
	bindingRepository, err := NewOutputInboxRepository(pool)
	if err != nil {
		t.Fatal(err)
	}
	claimRepository, err := NewClaimsRepository(pool)
	if err != nil {
		t.Fatal(err)
	}
	claimService, err := executionapp.NewClaimService(claimRepository, time.Now, 30*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	projector, err := NewConfigurationValidationResultsRepository(pool)
	if err != nil {
		t.Fatal(err)
	}
	service, err := outputapp.NewConfigurationValidationService(bindingRepository, claimService, projector)
	if err != nil {
		t.Fatal(err)
	}
	return service
}

func newPostgresRuntimeFailureService(t *testing.T, pool *pgxpool.Pool) *outputapp.RuntimeFailureService {
	t.Helper()
	bindings, err := NewOutputInboxRepository(pool)
	if err != nil {
		t.Fatal(err)
	}
	claims, err := NewClaimsRepository(pool)
	if err != nil {
		t.Fatal(err)
	}
	claimService, err := executionapp.NewClaimService(claims, time.Now, 30*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	projector, err := NewRuntimeFailureResultsRepository(pool)
	if err != nil {
		t.Fatal(err)
	}
	service, err := outputapp.NewRuntimeFailureService(bindings, claimService, projector)
	if err != nil {
		t.Fatal(err)
	}
	return service
}

func postgresDeadlineFailureFrame(t *testing.T, source outputapp.ConfigurationValidationFrame) outputapp.RuntimeFailureFrame {
	t.Helper()
	failure := &runtimev1.RuntimeErrorV1{
		Code:        runtimev1.RuntimeErrorCodeV1_RUNTIME_ERROR_CODE_V1_DEADLINE_EXCEEDED,
		SafeMessage: outputapp.DeadlineExceededSafeMessage,
		Retryable:   true,
	}
	payload, err := proto.MarshalOptions{Deterministic: true}.Marshal(failure)
	if err != nil {
		t.Fatal(err)
	}
	payloadDigest := runtimedomain.SHA256(payload)
	proposal := source.Settlement
	proposal.Outcome = executionapp.SettlementFailed
	proposal.TerminalPayloadDigest = payloadDigest
	wireProposal := &runtimev1.SettlementProposalV1{
		ProposalId: proposal.ProposalID, RequestedOutcome: runtimev1.ExecutionOutcomeV1_EXECUTION_OUTCOME_V1_FAILED,
		TerminalLogicalOutputId: proposal.TerminalLogicalOutputID, TerminalEventId: proposal.TerminalEventID,
		TerminalSequence: proposal.TerminalSequence, TerminalPayloadDigest: postgresDigestV1(payloadDigest),
		PrepareIdempotencyKey: proposal.IdempotencyKey,
	}
	encodedProposal, err := proto.MarshalOptions{Deterministic: true}.Marshal(wireProposal)
	if err != nil {
		t.Fatal(err)
	}
	proposal.ProposalDigest = runtimedomain.SHA256(encodedProposal)
	frame := outputapp.RuntimeFailureFrame{
		StreamID: source.StreamID, TenantID: source.TenantID,
		ResourceProjectID: source.ResourceProjectID, ProjectionProjectID: source.ProjectionProjectID,
		WorkloadSessionID: source.WorkloadSessionID, ProducerID: source.ProducerID,
		EventID: source.EventID, LogicalOutputID: source.LogicalOutputID,
		Sequence: source.Sequence, ClaimHandoffWatermark: source.ClaimHandoffWatermark,
		OccurredAt: time.Now().UTC(), Fence: source.Fence,
		PayloadDigest: payloadDigest, EncodedFailure: payload,
		Settlement: proposal, EncodedSettlement: encodedProposal,
		Failure: outputapp.RuntimeFailure{Code: "DEADLINE_EXCEEDED", SafeMessage: outputapp.DeadlineExceededSafeMessage, Retryable: true},
	}
	if err := frame.Validate(); err != nil {
		t.Fatalf("build PostgreSQL deadline failure frame: %v", err)
	}
	return frame
}

func assertPostgresCanonicalCancellation(t *testing.T, pool *pgxpool.Pool, frame outputapp.ConfigurationValidationFrame, sourceClaimID string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	var (
		claimID, payloadType, settlementOutcome, proposalID, settlementKey string
		payloadDigest, payloadBytes, proposalBytes, proposalDigest         []byte
		occurredAt, projectedAt                                            time.Time
		sequence, watermark                                                int64
	)
	err := pool.QueryRow(ctx, `
SELECT claim_id, payload_type, payload_digest, payload_bytes,
       settlement_outcome, settlement_proposal_id,
       settlement_proposal_bytes, settlement_proposal_digest,
       settlement_idempotency_key, occurred_at, projected_at,
       sequence, claim_handoff_watermark
FROM elitea_runtime.output_inbox
WHERE execution_id = $1 AND generation = $2`, frame.Fence.ExecutionID, int64(frame.Fence.Generation)).Scan(
		&claimID,
		&payloadType,
		&payloadDigest,
		&payloadBytes,
		&settlementOutcome,
		&proposalID,
		&proposalBytes,
		&proposalDigest,
		&settlementKey,
		&occurredAt,
		&projectedAt,
		&sequence,
		&watermark,
	)
	if err != nil {
		t.Fatalf("load canonical cancellation: %v", err)
	}
	if claimID != sourceClaimID || payloadType != payloadTypeRuntimeFailure || settlementOutcome != string(executionapp.SettlementCancelled) {
		t.Fatalf("unexpected canonical cancellation identity: claim=%q payload=%q outcome=%q", claimID, payloadType, settlementOutcome)
	}
	if sequence != int64(frame.Sequence) || watermark != int64(frame.ClaimHandoffWatermark) {
		t.Fatalf("unexpected durable output position: sequence=%d watermark=%d", sequence, watermark)
	}
	if proposalID != frame.Fence.CommandID+":settlement" || settlementKey != frame.Fence.CommandID+":prepare-settlement" {
		t.Fatalf("unexpected canonical settlement identity: proposal=%q key=%q", proposalID, settlementKey)
	}
	if !occurredAt.Equal(frame.OccurredAt) {
		t.Fatalf("source timestamp was not preserved exactly: got=%s want=%s", occurredAt.Format(time.RFC3339Nano), frame.OccurredAt.Format(time.RFC3339Nano))
	}
	if projectedAt.IsZero() {
		t.Fatal("canonical cancellation output is not marked projected")
	}
	if !bytes.Equal(payloadDigest, postgresDigestBytes(runtimedomain.SHA256(payloadBytes))) {
		t.Fatal("stored cancellation payload digest does not bind payload bytes")
	}
	if !bytes.Equal(proposalDigest, postgresDigestBytes(runtimedomain.SHA256(proposalBytes))) {
		t.Fatal("stored cancellation proposal digest does not bind proposal bytes")
	}

	failure := &runtimev1.RuntimeErrorV1{}
	if err := proto.Unmarshal(payloadBytes, failure); err != nil {
		t.Fatalf("decode cancellation payload: %v", err)
	}
	if failure.GetCode() != runtimev1.RuntimeErrorCodeV1_RUNTIME_ERROR_CODE_V1_CANCELLED || failure.GetSafeMessage() != "Execution was cancelled." || failure.GetRetryable() {
		t.Fatalf("unexpected cancellation payload: %+v", failure)
	}
	proposal := &runtimev1.SettlementProposalV1{}
	if err := proto.Unmarshal(proposalBytes, proposal); err != nil {
		t.Fatalf("decode cancellation proposal: %v", err)
	}
	if proposal.GetProposalId() != proposalID || proposal.GetRequestedOutcome() != runtimev1.ExecutionOutcomeV1_EXECUTION_OUTCOME_V1_CANCELLED || proposal.GetTerminalLogicalOutputId() != frame.LogicalOutputID || proposal.GetTerminalEventId() != frame.EventID || proposal.GetTerminalSequence() != frame.Sequence || proposal.GetPrepareIdempotencyKey() != settlementKey || !bytes.Equal(proposal.GetTerminalPayloadDigest().GetValue(), payloadDigest) {
		t.Fatalf("unexpected cancellation settlement proposal: %+v", proposal)
	}

	var replayType string
	var replayBytes, replayDigest []byte
	if err := pool.QueryRow(ctx, `
SELECT event_type, event_bytes, event_digest
FROM elitea_runtime.execution_replay_events
WHERE event_id = $1`, frame.EventID).Scan(&replayType, &replayBytes, &replayDigest); err != nil {
		t.Fatalf("load cancellation replay event: %v", err)
	}
	wantReplay := []byte(`{"code":"CANCELLED","safe_message":"Execution was cancelled.","retryable":false}`)
	if replayType != replayEventRuntimeFailure || !bytes.Equal(replayBytes, wantReplay) || !bytes.Equal(replayDigest, postgresDigestBytes(runtimedomain.SHA256(replayBytes))) {
		t.Fatalf("unexpected cancellation replay event: type=%q bytes=%s", replayType, replayBytes)
	}

	assertPostgresCount(t, ctx, pool, 1, `
SELECT count(*) FROM elitea_runtime.output_inbox
WHERE execution_id = $1 AND generation = $2`, frame.Fence.ExecutionID, int64(frame.Fence.Generation))
	assertPostgresCount(t, ctx, pool, 1, `
SELECT count(*) FROM elitea_runtime.execution_replay_events
WHERE execution_id = $1 AND generation = $2`, frame.Fence.ExecutionID, int64(frame.Fence.Generation))
	assertPostgresCount(t, ctx, pool, 0, `
SELECT count(*) FROM elitea_runtime.configuration_validation_results
WHERE execution_id = $1 AND generation = $2`, frame.Fence.ExecutionID, int64(frame.Fence.Generation))
	assertPostgresCount(t, ctx, pool, 0, `
SELECT count(*) FROM p_1.configuration_validation_projection
WHERE execution_id = $1 AND execution_generation = $2`, frame.Fence.ExecutionID, int64(frame.Fence.Generation))
}

func expirePostgresClaim(t *testing.T, pool *pgxpool.Pool, claimID string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	tag, err := pool.Exec(ctx, `
UPDATE elitea_runtime.execution_claims
SET lease_expires_at = clock_timestamp() - interval '30 minutes'
WHERE claim_id = $1
  AND claimed_at < clock_timestamp() - interval '60 minutes'
  AND released_at IS NULL`, claimID)
	if err != nil {
		t.Fatalf("expire source claim: %v", err)
	}
	if tag.RowsAffected() != 1 {
		t.Fatalf("expire source claim affected %d rows", tag.RowsAffected())
	}
}

func ageAndExpirePostgresClaim(t *testing.T, pool *pgxpool.Pool, claimID string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	tag, err := pool.Exec(ctx, `
UPDATE elitea_runtime.execution_claims
SET claimed_at = clock_timestamp() - interval '2 hours',
    lease_expires_at = clock_timestamp() - interval '30 minutes'
WHERE claim_id = $1
  AND released_at IS NULL`, claimID)
	if err != nil {
		t.Fatalf("age and expire source claim: %v", err)
	}
	if tag.RowsAffected() != 1 {
		t.Fatalf("age and expire source claim affected %d rows", tag.RowsAffected())
	}
}

func assertPostgresTerminalRecovery(t *testing.T, pool *pgxpool.Pool, frame outputapp.ConfigurationValidationFrame, seed postgresValidationSeed, request executionapp.ClaimRequest, decision executionapp.ClaimDecision) {
	t.Helper()
	if decision.Disposition != executionapp.ClaimRecoverTerminalACK {
		t.Fatalf("unexpected replacement claim disposition: %s", decision.Disposition)
	}
	if decision.Lease.Fence.ClaimAttempt != 2 || decision.Lease.Fence.LeaseEpoch != 2 || decision.Lease.Fence.WorkloadIdentity != request.WorkloadIdentity || decision.Lease.Fence.WorkloadSessionID != request.WorkloadSessionID || decision.Lease.Fence.ProducerID != request.ProducerID {
		t.Fatalf("replacement fence was not rebound to the new pod: %+v", decision.Lease.Fence)
	}
	if decision.SettlementRecovery == nil || decision.SettlementRecovery.Proposal == nil || decision.SettlementRecovery.Receipt != nil {
		t.Fatalf("missing terminal proposal recovery: %+v", decision.SettlementRecovery)
	}
	proposal := decision.SettlementRecovery.Proposal
	if proposal.Fence != decision.Lease.Fence || proposal.Outcome != executionapp.SettlementCancelled || proposal.ProposalID != frame.Fence.CommandID+":settlement" || proposal.TerminalLogicalOutputID != frame.LogicalOutputID || proposal.TerminalEventID != frame.EventID || proposal.TerminalSequence != frame.Sequence || proposal.IdempotencyKey != frame.Fence.CommandID+":prepare-settlement" {
		t.Fatalf("unexpected recovered terminal proposal: %+v", proposal)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var releasedReason string
	var releasedAt time.Time
	if err := pool.QueryRow(ctx, `
SELECT release_reason, released_at
FROM elitea_runtime.execution_claims
WHERE claim_id = $1`, seed.claimID).Scan(&releasedReason, &releasedAt); err != nil {
		t.Fatalf("load released predecessor claim: %v", err)
	}
	if releasedReason != "LEASE_EXPIRED" || releasedAt.IsZero() {
		t.Fatalf("predecessor claim was not durably expired: reason=%q released_at=%s", releasedReason, releasedAt)
	}
	assertPostgresCount(t, ctx, pool, 1, `
SELECT count(*) FROM elitea_runtime.execution_claims
WHERE execution_id = $1 AND generation = $2 AND released_at IS NULL`, frame.Fence.ExecutionID, int64(frame.Fence.Generation))
	assertPostgresCount(t, ctx, pool, 2, `
SELECT count(*) FROM elitea_runtime.execution_claims
WHERE execution_id = $1 AND generation = $2`, frame.Fence.ExecutionID, int64(frame.Fence.Generation))
}

func assertPostgresCount(t *testing.T, ctx context.Context, pool *pgxpool.Pool, want int64, query string, args ...any) {
	t.Helper()
	var got int64
	if err := pool.QueryRow(ctx, query, args...).Scan(&got); err != nil {
		t.Fatalf("query PostgreSQL integration count: %v", err)
	}
	if got != want {
		t.Fatalf("unexpected durable row count: got=%d want=%d", got, want)
	}
}

func waitForPostgresAuthorityLock(t *testing.T, ctx context.Context, pool *pgxpool.Pool, blockerPID int32) bool {
	t.Helper()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		var blocked bool
		err := pool.QueryRow(ctx, `
SELECT EXISTS (
    SELECT 1
    FROM pg_catalog.pg_stat_activity AS a
    WHERE a.datname = current_database()
      AND a.pid <> pg_backend_pid()
      AND a.state = 'active'
      AND a.wait_event_type = 'Lock'
      AND a.query LIKE '%SELECT c.claim_id%'
      AND a.query ~ 'FROM[[:space:]]+elitea_runtime[.]execution_claims[[:space:]]+(AS[[:space:]]+)?c'
      AND a.query ~ 'FOR[[:space:]]+UPDATE[[:space:]]+OF[[:space:]]+j[[:space:]]*,[[:space:]]*o[[:space:]]*,[[:space:]]*c'
      AND $1::integer = ANY(pg_catalog.pg_blocking_pids(a.pid))
)`, blockerPID).Scan(&blocked)
		if err != nil {
			t.Fatalf("observe PostgreSQL authority lock: %v", err)
		}
		if blocked {
			return true
		}
		select {
		case <-ctx.Done():
			return false
		case <-ticker.C:
		}
	}
}
