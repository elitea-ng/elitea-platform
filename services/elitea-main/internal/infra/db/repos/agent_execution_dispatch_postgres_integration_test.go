package repos

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	agentexecutionapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/agentexecution"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/transport/redisdispatch"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestPostgresAgentDispatchRetainsExactEnvelopeAcrossRedisOutageAndACKLoss(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	policy := AgentExecutionDispatchPolicy{
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
	redisOutage := errors.New("test Redis unavailable")
	signer := &postgresIndexDispatchSigner{keyID: "agent-key-before-rotation"}
	appender := &postgresIndexDispatchAppender{err: redisOutage}
	producer := newPostgresAgentProducer(t, policy, signer, appender)
	dispatcher, err := agentexecutionapp.NewDispatcher(repository, producer)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if err := dispatcher.Dispatch(ctx, "outbox-agent"); !errors.Is(err, redisOutage) {
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

	// Simulate a process restart after Redis accepted the command but before a
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

func newPostgresAgentProducer(
	t *testing.T,
	policy AgentExecutionDispatchPolicy,
	signer redisdispatch.CommandSigner,
	appender redisdispatch.StreamAppender,
) *redisdispatch.AgentExecutionProducer {
	t.Helper()
	producer, err := redisdispatch.NewAgentExecutionProducer(
		redisdispatch.AgentExecutionProducerConfig{
			Stream:                       policy.StreamName,
			ConsumerGroup:                "elitea-agent-worker-v1",
			ValidationStream:             "elitea:runtime:validation:commands",
			IndexIngestStream:            "elitea:runtime:index:commands",
			ProtocolRevision:             "runtime-v1",
			EnvelopeSchemaRevision:       "signed-worker-command-v1",
			ApplicationCapabilityVersion: policy.CapabilityVersion,
			AdhocCapabilityVersion:       policy.CapabilityVersion,
			Limits: redisdispatch.Limits{
				Revision:               policy.LimitsRevision,
				MaxWorkerCommandBytes:  8 * 1024,
				MaxSignedEnvelopeBytes: 12 * 1024,
				MaxRedisFieldBytes:     12 * 1024,
				MaxRedisEntryBytes:     16 * 1024,
				MaxSignatureBytes:      128,
				MaxStringBytes:         512,
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
