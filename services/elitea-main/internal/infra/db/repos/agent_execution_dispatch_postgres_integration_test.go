package repos

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	agentexecutionapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/agentexecution"
	executionapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/execution"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/credentialgovernance/profile"
	executiondomain "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/execution"
	runtimedomain "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/runtime"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/transport/commandbus"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestPostgresAgentDispatchRetainsExactEnvelopeAcrossBusOutageAndACKLoss(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	policy := AgentExecutionDispatchPolicy{
		CredentialProfile: profile.LegacyAdmission(false),
		StreamName:        "elitea:runtime:agent:commands",
		CapabilityVersion: "1",
		ResourceClass:     "agent",
		IsolationClass:    "project",
		Priority:          1,
		DeadlineTTL:       time.Hour,
		LimitsRevision:    "agent-limits-v1",
		MaxOutstanding:    2,
	}
	repository, err := NewAgentExecutionJobsRepository(pool, policy, 1)
	if err != nil {
		t.Fatal(err)
	}
	admitted := admitPostgresAgentExecution(
		t,
		pool,
		"10000000-0000-4000-8000-000000000021",
		"20000000-0000-4000-8000-000000000021",
		"30000000-0000-4000-8000-000000000021",
	)
	busOutage := errors.New("test command bus unavailable")
	signer := &postgresIndexDispatchSigner{keyID: "agent-key-before-rotation"}
	appender := &postgresIndexDispatchAppender{err: busOutage}
	producer := newPostgresAgentProducer(t, policy, signer, appender)
	dispatcher, err := agentexecutionapp.NewDispatcher(repository, producer)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if err := dispatcher.Dispatch(ctx, "outbox-agent"); !errors.Is(err, busOutage) {
		t.Fatalf("injected agent append failure = %v", err)
	}
	prepared := assertPostgresAgentDispatchState(t, ctx, pool, admitted.ExecutionID, false, "PENDING", 0)
	if signer.callCount() != 1 || appender.callCount() != 1 ||
		!bytes.Equal(prepared, appender.callBytes(0)) {
		t.Fatal("failed agent append did not retain its exact prepared envelope")
	}

	signer.keyID = "agent-key-after-rotation"
	appender.setError(nil)
	if err := dispatcher.Dispatch(ctx, "outbox-agent"); err != nil {
		t.Fatal(err)
	}
	assertPostgresAgentDispatchState(t, ctx, pool, admitted.ExecutionID, true, "DISPATCHED", 1)
	if signer.callCount() != 1 || appender.callCount() != 2 ||
		!bytes.Equal(appender.callBytes(0), appender.callBytes(1)) {
		t.Fatal("agent dispatch retry re-signed or changed the durable envelope")
	}

	// Simulate a process restart after the command bus accepted the command but before a
	// worker claim/ACK became observable. The visibility timeout must select the
	// same outbox row and replay the exact prepared bytes.
	if _, err := pool.Exec(ctx, `
UPDATE elitea_runtime.command_outbox
SET last_visibility_at = clock_timestamp() - interval '2 minutes'
WHERE outbox_id = 'outbox-agent'`); err != nil {
		t.Fatal(err)
	}
	visible, err := repository.ListPendingAgentExecutionIDs(ctx, 4, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if len(visible) != 1 || visible[0] != "outbox-agent" {
		t.Fatalf("ACK-loss recovery candidates = %v", visible)
	}
	if err := dispatcher.Dispatch(ctx, visible[0]); err != nil {
		t.Fatal(err)
	}
	assertPostgresAgentDispatchState(t, ctx, pool, admitted.ExecutionID, true, "DISPATCHED", 2)
	if signer.callCount() != 1 || appender.callCount() != 3 ||
		!bytes.Equal(appender.callBytes(0), appender.callBytes(2)) {
		t.Fatal("ACK-loss recovery changed or re-signed the durable agent envelope")
	}
}

// TestPostgresAgentDispatchHeldPublisherStillAppendsDurableWinner pins the same
// competing-publisher window that lost one index publish attempt. One publisher
// is held at the signing barrier until a competitor stores, appends and
// publishes the command, which moves the job to DISPATCHED. No worker holds
// authority, so the held publisher must still receive the durable winner,
// append it, and count its publish attempt. A refusal there reaches the
// dispatcher as success and drops the attempt without an error.
func TestPostgresAgentDispatchHeldPublisherStillAppendsDurableWinner(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	policy := AgentExecutionDispatchPolicy{
		CredentialProfile: profile.LegacyAdmission(false),
		StreamName:        "elitea:runtime:agent:commands",
		CapabilityVersion: "1",
		ResourceClass:     "agent",
		IsolationClass:    "project",
		Priority:          1,
		DeadlineTTL:       time.Hour,
		LimitsRevision:    "agent-limits-v1",
		MaxOutstanding:    2,
	}
	repository, err := NewAgentExecutionJobsRepository(pool, policy, 1)
	if err != nil {
		t.Fatal(err)
	}
	admitted := admitPostgresAgentExecution(
		t,
		pool,
		"10000000-0000-4000-8000-000000000022",
		"20000000-0000-4000-8000-000000000022",
		"30000000-0000-4000-8000-000000000022",
	)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	appender := &postgresIndexDispatchAppender{}
	started := make(chan struct{}, 1)
	release := make(chan struct{})
	heldSigner := &postgresIndexDispatchSigner{
		keyID:   "agent-held-loser",
		started: started,
		release: release,
	}
	winnerSigner := &postgresIndexDispatchSigner{keyID: "agent-held-winner"}
	heldDispatcher, err := agentexecutionapp.NewDispatcher(
		repository,
		newPostgresAgentProducer(t, policy, heldSigner, appender),
	)
	if err != nil {
		t.Fatal(err)
	}
	winnerDispatcher, err := agentexecutionapp.NewDispatcher(
		repository,
		newPostgresAgentProducer(t, policy, winnerSigner, appender),
	)
	if err != nil {
		t.Fatal(err)
	}

	heldResult := make(chan error, 1)
	go func() {
		heldResult <- heldDispatcher.Dispatch(ctx, "outbox-agent")
	}()
	select {
	case <-started:
	case <-ctx.Done():
		t.Fatalf("held agent publisher did not reach the signing barrier: %v", ctx.Err())
	}

	if err := winnerDispatcher.Dispatch(ctx, "outbox-agent"); err != nil {
		t.Fatalf("winning agent publisher: %v", err)
	}
	assertPostgresAgentDispatchState(t, ctx, pool, admitted.ExecutionID, true, "DISPATCHED", 1)

	close(release)
	select {
	case err := <-heldResult:
		if err != nil {
			t.Fatalf("held agent publisher: %v", err)
		}
	case <-ctx.Done():
		t.Fatalf("held agent publisher did not return: %v", ctx.Err())
	}

	prepared := assertPostgresAgentDispatchState(t, ctx, pool, admitted.ExecutionID, true, "DISPATCHED", 2)
	if appender.callCount() != 2 ||
		!bytes.Equal(appender.callBytes(0), prepared) ||
		!bytes.Equal(appender.callBytes(1), prepared) {
		t.Fatalf("held agent publisher dropped its append of the durable winner: appends=%d", appender.callCount())
	}
	if winnerSigner.callCount() != 1 || heldSigner.callCount() != 1 {
		t.Fatal("held agent publisher race re-signed the durable envelope")
	}
}

// TestPostgresAgentDispatchNeverRepublishesAClaimedRunningExecutionAfterItsLeaseLapses
// pins FU-290: once a worker has been granted authority, an abandoned lease
// (no release) must never make the execution a republish candidate, or a second
// worker could claim and re-run it.
func TestPostgresAgentDispatchNeverRepublishesAClaimedRunningExecutionAfterItsLeaseLapses(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	policy := AgentExecutionDispatchPolicy{
		CredentialProfile: profile.LegacyAdmission(false),
		StreamName:        "elitea:runtime:agent:commands",
		CapabilityVersion: "1",
		ResourceClass:     "agent",
		IsolationClass:    "project",
		Priority:          1,
		DeadlineTTL:       time.Hour,
		LimitsRevision:    "agent-limits-v1",
		MaxOutstanding:    2,
	}
	repository, err := NewAgentExecutionJobsRepository(pool, policy, 1)
	if err != nil {
		t.Fatal(err)
	}
	admitted := admitPostgresAgentExecution(
		t,
		pool,
		"10000000-0000-4000-8000-000000000023",
		"20000000-0000-4000-8000-000000000023",
		"30000000-0000-4000-8000-000000000023",
	)
	dispatcher, err := agentexecutionapp.NewDispatcher(
		repository,
		newPostgresAgentProducer(
			t,
			policy,
			&postgresIndexDispatchSigner{keyID: "agent-key-lapsed-lease"},
			&postgresIndexDispatchAppender{},
		),
	)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if err := dispatcher.Dispatch(ctx, "outbox-agent"); err != nil {
		t.Fatal(err)
	}
	assertPostgresAgentDispatchState(t, ctx, pool, admitted.ExecutionID, true, "DISPATCHED", 1)

	// Control: published, DISPATCHED and past the visibility timeout, the row is
	// a republish candidate. Without this the later emptiness proves nothing.
	if _, err := pool.Exec(ctx, `
UPDATE elitea_runtime.command_outbox
SET last_visibility_at = clock_timestamp() - interval '2 minutes'
WHERE outbox_id = 'outbox-agent'`); err != nil {
		t.Fatal(err)
	}
	visible, err := repository.ListPendingAgentExecutionIDs(ctx, 4, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if len(visible) != 1 || visible[0] != "outbox-agent" {
		t.Fatalf("control: unclaimed visibility-elapsed candidates = %v", visible)
	}

	var commandID string
	var generation int64
	var digestBytes []byte
	if err := pool.QueryRow(ctx, `
SELECT j.command_id, j.generation, o.prepared_signed_envelope_digest
FROM elitea_runtime.execution_jobs AS j
JOIN elitea_runtime.command_outbox AS o
  ON o.execution_id = j.execution_id AND o.generation = j.generation
WHERE j.execution_id = $1`, admitted.ExecutionID).Scan(&commandID, &generation, &digestBytes); err != nil {
		t.Fatal(err)
	}
	var digest runtimedomain.Digest
	if copy(digest[:], digestBytes) != len(digest) {
		t.Fatal("stored agent envelope digest has the wrong length")
	}
	claims, err := NewClaimsRepository(pool)
	if err != nil {
		t.Fatal(err)
	}
	decision, err := claims.ClaimValidation(ctx, executionapp.ClaimRequest{
		CommandID:            commandID,
		OutboxID:             "outbox-agent",
		ExecutionID:          admitted.ExecutionID,
		Generation:           uint64(generation),
		CapabilityID:         executiondomain.AgentApplicationCapability,
		SignedEnvelopeDigest: digest,
		WorkloadIdentity:     "spiffe://elitea.test/workload/agent-lapsed-lease",
		WorkloadSessionID:    "agent-lapsed-lease-session",
		ProducerID:           "agent-lapsed-lease-producer",
	}, executionapp.MaxClaimLeaseTTLMillis)
	if err != nil || decision.Disposition != executionapp.ClaimAccepted {
		t.Fatalf("agent claim = %+v err=%v", decision, err)
	}
	if _, err := claims.BeginExecution(ctx, decision.Lease.Fence); err != nil {
		t.Fatal(err)
	}

	// The worker gives up: it stops renewing and never releases. Make the lease
	// and the visibility window both long past.
	if _, err := pool.Exec(ctx, `
UPDATE elitea_runtime.execution_claims
SET claimed_at = clock_timestamp() - interval '2 hours',
    lease_expires_at = clock_timestamp() - interval '1 hour'
WHERE claim_id = $1`, decision.Lease.ClaimID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
UPDATE elitea_runtime.command_outbox
SET published_at = clock_timestamp() - interval '2 hours',
    last_visibility_at = clock_timestamp() - interval '2 hours'
WHERE outbox_id = 'outbox-agent'`); err != nil {
		t.Fatal(err)
	}
	var visibilityBefore time.Time
	var attemptsBefore int32
	if err := pool.QueryRow(ctx, `
SELECT last_visibility_at, publish_attempts
FROM elitea_runtime.command_outbox WHERE outbox_id = 'outbox-agent'`).Scan(&visibilityBefore, &attemptsBefore); err != nil {
		t.Fatal(err)
	}

	visible, err = repository.ListPendingAgentExecutionIDs(ctx, 4, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if len(visible) != 0 {
		t.Fatalf("claimed execution with a lapsed lease became a republish candidate: %v", visible)
	}
	if err := repository.MarkAgentExecutionPublished(ctx, "outbox-agent", digest); err != nil {
		t.Fatalf("mark published for a claimed execution: %v", err)
	}

	var visibilityAfter time.Time
	var attemptsAfter int32
	var jobState string
	var authorityGranted bool
	var releasedAt *time.Time
	if err := pool.QueryRow(ctx, `
SELECT o.last_visibility_at, o.publish_attempts, j.state,
       o.authority_granted_at IS NOT NULL, c.released_at
FROM elitea_runtime.command_outbox AS o
JOIN elitea_runtime.execution_jobs AS j
  ON j.execution_id = o.execution_id AND j.generation = o.generation
JOIN elitea_runtime.execution_claims AS c ON c.claim_id = $1
WHERE o.outbox_id = 'outbox-agent'`, decision.Lease.ClaimID).Scan(
		&visibilityAfter, &attemptsAfter, &jobState, &authorityGranted, &releasedAt,
	); err != nil {
		t.Fatal(err)
	}
	if !visibilityAfter.Equal(visibilityBefore) || attemptsAfter != attemptsBefore {
		t.Fatalf("publication refreshed a claimed execution: visibility %s -> %s attempts %d -> %d",
			visibilityBefore, visibilityAfter, attemptsBefore, attemptsAfter)
	}
	if jobState != "RUNNING" || !authorityGranted || releasedAt != nil {
		t.Fatalf("abandoned claim changed: state=%q authority=%v released_at=%v", jobState, authorityGranted, releasedAt)
	}
}

func newPostgresAgentProducer(
	t *testing.T,
	policy AgentExecutionDispatchPolicy,
	signer commandbus.CommandSigner,
	appender commandbus.StreamAppender,
) *commandbus.AgentExecutionProducer {
	t.Helper()
	producer, err := commandbus.NewAgentExecutionProducer(
		commandbus.AgentExecutionProducerConfig{
			Stream:                       policy.StreamName,
			Consumer:                     "elitea-agent-worker-v1",
			ValidationStream:             "elitea:runtime:validation:commands",
			IndexIngestStream:            "elitea:runtime:index:commands",
			ProtocolRevision:             "runtime-v1",
			EnvelopeSchemaRevision:       "signed-worker-command-v1",
			ApplicationCapabilityVersion: policy.CapabilityVersion,
			AdhocCapabilityVersion:       policy.CapabilityVersion,
			ToolkitReadCapabilityVersion: policy.CapabilityVersion,
			Limits: commandbus.Limits{
				Revision:                 policy.LimitsRevision,
				MaxWorkerCommandBytes:    8 * 1024,
				MaxSignedEnvelopeBytes:   12 * 1024,
				MaxTransportPayloadBytes: 12 * 1024,
				MaxTransportMessageBytes: 16 * 1024,
				MaxSignatureBytes:        128,
				MaxStringBytes:           512,
			},
			AllowTestOnlyHMAC: true,
		},
		signer,
		appender,
	)
	if err != nil {
		t.Fatal(err)
	}
	return producer
}

func assertPostgresAgentDispatchState(
	t *testing.T,
	ctx context.Context,
	pool *pgxpool.Pool,
	executionID string,
	published bool,
	state string,
	attempts int32,
) []byte {
	t.Helper()
	var prepared []byte
	var storedPublished bool
	var storedState string
	var storedAttempts int32
	if err := pool.QueryRow(ctx, `
SELECT o.prepared_signed_envelope_bytes,
       o.published_at IS NOT NULL,
       j.state,
       o.publish_attempts
FROM elitea_runtime.execution_jobs AS j
JOIN elitea_runtime.command_outbox AS o
  ON o.execution_id = j.execution_id AND o.generation = j.generation
WHERE j.execution_id = $1
  AND j.capability_id IN (
      'agent.execute.application.v1',
      'agent.execute.adhoc.v1'
  )`, executionID).Scan(
		&prepared,
		&storedPublished,
		&storedState,
		&storedAttempts,
	); err != nil {
		t.Fatal(err)
	}
	if storedPublished != published || storedState != state || storedAttempts != attempts {
		t.Fatalf(
			"agent dispatch state changed: published=%v state=%q attempts=%d",
			storedPublished,
			storedState,
			storedAttempts,
		)
	}
	return prepared
}
