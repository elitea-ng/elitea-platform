package repos

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	runtimev1 "github.com/EliteaAI/elitea-platform/libs/proto/gen/go/elitea/runtime/v1"
	agentexecutionapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/agentexecution"
	executionapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/execution"
	executiondomain "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/execution"
	runtimedomain "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/runtime"
	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/protobuf/proto"
)

// TestPostgresAgentUnmaterializedReservationCountsTowardCap pins the issue 965
// invariant that a reserved-but-not-yet-materialized agent start consumes a
// capacity slot. A start that reserved its durable slot but has not written its
// execution job yet still blocks new starts from exceeding the live cap, so a
// crash between the reserve and the materialize cannot over-admit.
func TestPostgresAgentUnmaterializedReservationCountsTowardCap(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	const maxOutstanding = int64(3)
	policy := testAgentDispatchPolicy()
	policy.MaxOutstanding = maxOutstanding
	repository, err := NewAgentExecutionJobsRepository(pool, policy, 1)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Materialize one durable execution: one active job, one materialized
	// reservation.
	if _, err := repository.AdmitAgentExecution(ctx, postgresAgentCapacityAdmission(0)); err != nil {
		t.Fatalf("admit materialized agent execution: %v", err)
	}

	// Reserve two more slots without materializing them (in-flight starts that
	// reserved their durable slot but have not written the execution job).
	for index := 1; index < 3; index++ {
		if err := repository.reserveAgentAdmission(ctx, postgresAgentCapacityAdmission(index)); err != nil {
			t.Fatalf("reserve unmaterialized agent admission %d: %v", index, err)
		}
	}
	assertPostgresAgentAdmissionState(t, ctx, pool, 1, 1, 2, 1)

	// active(1) + unmaterialized(2) = 3 = cap. A new start must be rejected with
	// the typed capacity error even though only one durable job is active.
	_, err = repository.AdmitAgentExecution(ctx, postgresAgentCapacityAdmission(3))
	assertPostgresAgentCapacityError(t, err, maxOutstanding)
}

// TestPostgresServiceBackedAgentAdmissionCapacity is a real PostgreSQL
// service-integration gate for the agent start path. Separate repository
// instances model independent application pods contending on the same durable
// policy row. It pins the high-water mark (never more than the cap admitted),
// the typed retry contract on rejection, exact replay at full cap, the
// fail-closed policy mismatch, and slot release on a terminal transition.
func TestPostgresServiceBackedAgentAdmissionCapacity(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	const (
		maxOutstanding = int64(3)
		attempts       = 12
	)
	policy := testAgentDispatchPolicy()
	policy.MaxOutstanding = maxOutstanding

	type result struct {
		index   int
		outcome executionapp.AdmissionOutcome
		err     error
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	start := make(chan struct{})
	results := make(chan result, attempts)
	admissions := make([]agentexecutionapp.Admission, attempts)
	for index := range attempts {
		admissions[index] = postgresAgentCapacityAdmission(index)
		repository, err := NewAgentExecutionJobsRepository(pool, policy, 1)
		if err != nil {
			t.Fatal(err)
		}
		go func(index int, repository *AgentExecutionJobsRepository) {
			<-start
			outcome, err := repository.AdmitAgentExecution(ctx, admissions[index])
			results <- result{index: index, outcome: outcome, err: err}
		}(index, repository)
	}
	close(start)

	created := make([]int, 0, maxOutstanding)
	capacityRejected := 0
	for range attempts {
		result := <-results
		switch {
		case result.err == nil:
			if !result.outcome.Created {
				t.Fatalf("new admission %d was reported as replay: %+v", result.index, result.outcome)
			}
			created = append(created, result.index)
		case errors.Is(result.err, executionapp.ErrAdmissionCapacityExhausted):
			assertPostgresAgentCapacityError(t, result.err, maxOutstanding)
			capacityRejected++
		default:
			t.Fatalf("admission %d failed unexpectedly: %v", result.index, result.err)
		}
	}
	if len(created) != int(maxOutstanding) || capacityRejected != attempts-int(maxOutstanding) {
		t.Fatalf("cross-repository high-water gate created=%d rejected=%d", len(created), capacityRejected)
	}
	assertPostgresAgentAdmissionState(t, ctx, pool, maxOutstanding, maxOutstanding, 0, maxOutstanding)

	repository, err := NewAgentExecutionJobsRepository(pool, policy, 1)
	if err != nil {
		t.Fatal(err)
	}
	winner := admissions[created[0]]
	replay, err := repository.AdmitAgentExecution(ctx, winner)
	if err != nil {
		t.Fatalf("exact replay at full capacity: %v", err)
	}
	if replay.Created || replay.ExecutionID != winner.Record.Job.ID || replay.CommandID != winner.Record.Job.CommandID {
		t.Fatalf("full-capacity replay changed durable identity: %+v", replay)
	}
	conflict := winner
	conflict.Record.RequestDigest = runtimedomain.SHA256([]byte("different-agent-capacity-request"))
	if _, err := repository.AdmitAgentExecution(ctx, conflict); !errors.Is(err, executionapp.ErrIdempotencyConflict) {
		t.Fatalf("conflicting replay at full capacity returned %v", err)
	}

	mismatchedPolicy := policy
	mismatchedPolicy.MaxOutstanding++
	mismatchedRepository, err := NewAgentExecutionJobsRepository(pool, mismatchedPolicy, 1)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := mismatchedRepository.AdmitAgentExecution(ctx, postgresAgentCapacityAdmission(attempts)); !errors.Is(err, ErrAdmissionPolicyMismatch) {
		t.Fatalf("persisted policy mismatch did not fail closed: %v", err)
	}

	if _, err := pool.Exec(ctx, `
UPDATE elitea_runtime.execution_jobs
SET state = 'SUCCEEDED', settled_at = clock_timestamp()
WHERE execution_id = $1 AND generation = $2`, winner.Record.Job.ID, int64(winner.Record.Job.Generation)); err != nil {
		t.Fatalf("terminalize admitted execution: %v", err)
	}
	replacement := postgresAgentCapacityAdmission(attempts + 1)
	replacementOutcome, err := repository.AdmitAgentExecution(ctx, replacement)
	if err != nil || !replacementOutcome.Created {
		t.Fatalf("terminal transition did not release one slot: outcome=%+v err=%v", replacementOutcome, err)
	}
	if _, err := repository.AdmitAgentExecution(ctx, postgresAgentCapacityAdmission(attempts+2)); !errors.Is(err, executionapp.ErrAdmissionCapacityExhausted) {
		t.Fatalf("replacement did not restore high-water mark: %v", err)
	}
	assertPostgresAgentAdmissionState(t, ctx, pool, maxOutstanding+1, maxOutstanding, 0, maxOutstanding+1)
}

// TestPostgresAgentAdmissionReservationReaper pins the two-row deletion the
// reaper performs: an unmaterialized reservation older than the stale window
// is reclaimed (the slot a dead start leaked), a materialized reservation
// older than the GC window is collected, and a fresh row on either side
// survives. This is the maintenance half of issue 965's two-phase admission:
// without it a crash between the reserve commit and the materialize commit
// would leak a capacity slot forever.
func TestPostgresAgentAdmissionReservationReaper(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	const maxOutstanding = int64(3)
	const capability = "agent.execute.adhoc.v1"
	policy := testAgentDispatchPolicy()
	policy.MaxOutstanding = maxOutstanding
	repository, err := NewAgentExecutionJobsRepository(pool, policy, 1)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Fill the cap with three unmaterialized reservations (phase 1 only).
	for index := 0; index < int(maxOutstanding); index++ {
		if err := repository.reserveAgentAdmission(ctx, postgresAgentCapacityAdmission(index)); err != nil {
			t.Fatalf("reserve unmaterialized agent admission %d: %v", index, err)
		}
	}

	// Age one unmaterialized slot past the 60s stale window.
	if _, err := pool.Exec(ctx, `
UPDATE elitea_runtime.agent_admission_reservations
SET reserved_at = clock_timestamp() - interval '5 minutes'
WHERE capability_id = $1 AND idempotency_key = $2`,
		capability, "agent-request-agent-capacity-00"); err != nil {
		t.Fatalf("age stale unmaterialized reservation: %v", err)
	}

	// Reap: exactly the stale unmaterialized row is removed.
	reaped, err := repository.ReapAgentAdmissionReservations(ctx, 60, 86400*7)
	if err != nil {
		t.Fatalf("reap stale unmaterialized: %v", err)
	}
	if reaped != 1 {
		t.Fatalf("expected 1 stale unmaterialized row reaped, got %d", reaped)
	}
	assertPostgresCount(t, ctx, pool, 2, `
SELECT count(*)
FROM elitea_runtime.agent_admission_reservations
WHERE capability_id = $1 AND materialized_at IS NULL`, capability)

	// Materialize one surviving slot, then age it past the 24h GC window.
	if _, err := pool.Exec(ctx, `
UPDATE elitea_runtime.agent_admission_reservations
SET materialized_at = clock_timestamp() - interval '2 days'
WHERE capability_id = $1 AND idempotency_key = $2`,
		capability, "agent-request-agent-capacity-01"); err != nil {
		t.Fatalf("age old materialized reservation: %v", err)
	}

	// Reap: the old materialized row is collected; the fresh unmaterialized
	// slot survives.
	reaped, err = repository.ReapAgentAdmissionReservations(ctx, 60, 86400)
	if err != nil {
		t.Fatalf("reap old materialized: %v", err)
	}
	if reaped != 1 {
		t.Fatalf("expected 1 old materialized row reaped, got %d", reaped)
	}
	assertPostgresCount(t, ctx, pool, 1, `
SELECT count(*)
FROM elitea_runtime.agent_admission_reservations
WHERE capability_id = $1`, capability)
	assertPostgresCount(t, ctx, pool, 1, `
SELECT count(*)
FROM elitea_runtime.agent_admission_reservations
WHERE idempotency_key = $1 AND materialized_at IS NULL`,
		"agent-request-agent-capacity-02")
}

func testAgentDispatchPolicy() AgentExecutionDispatchPolicy {
	return AgentExecutionDispatchPolicy{
		StreamName:        "elitea:runtime:agent:commands",
		CapabilityVersion: "1",
		ResourceClass:     "agent",
		IsolationClass:    "project",
		Priority:          1,
		DeadlineTTL:       time.Hour,
		LimitsRevision:    "agent-limits-v1",
		MaxOutstanding:    2,
	}
}

func agentCapacityInput(generation string) *runtimev1.AgentExecutionInputV1 {
	steps := int32(12)
	return &runtimev1.AgentExecutionInputV1{
		SchemaRevision:           "elitea.runtime.agent-execution-input.v1",
		Llm:                      []byte(`{"kwargs":{"model":"model-1"}}`),
		ChatHistory:              []byte(`[]`),
		UserInput:                []byte(`"hello"`),
		Tools:                    []byte(`[]`),
		Application:              []byte(`{"id":7,"version_id":9}`),
		InternalTools:            []byte(`[]`),
		StepsLimit:               &steps,
		McpTokens:                []byte(`{}`),
		IgnoredMcpServers:        []byte(`[]`),
		UserDeclinedMcpServers:   []byte(`[]`),
		HitlDecisions:            []byte(`[]`),
		ExecutionGeneration:      proto.String(generation),
		Meta:                     []byte(`{}`),
		Persona:                  "generic",
		ContextSettings:          []byte(`{}`),
		InvokedSkills:            []byte(`[]`),
		AppliedSkills:            []byte(`[]`),
		AttachedSkills:           []byte(`[]`),
		InputAttachments:         []byte(`[]`),
		ParallelReconcile:        []byte(`null`),
		ParallelTerminalErrors:   []byte(`[]`),
		ExceptionHandlingEnabled: proto.Bool(false),
		DebugMode:                proto.Bool(true),
	}
}

func postgresAgentCapacityAdmission(index int) agentexecutionapp.Admission {
	suffix := fmt.Sprintf("agent-capacity-%02d", index)
	ids := [5]string{
		"agent-bundle-" + suffix,
		"agent-content-" + suffix,
		"agent-execution-" + suffix,
		"agent-command-" + suffix,
		"agent-outbox-" + suffix,
	}
	idCursor := 0
	newID := func() (string, error) {
		value := ids[idCursor]
		idCursor++
		return value, nil
	}
	factory, err := agentexecutionapp.NewInputBundleFactory(
		agentexecutionapp.InputProfile{
			Classification:        "tenant-confidential",
			RequiredGrantAudience: "elitea.runtime.input.read.v1",
		},
		newID,
	)
	if err != nil {
		panic(err)
	}
	bundle, binding, err := factory.Build(
		context.Background(),
		agentCapacityInput("gen-"+suffix),
		"stream-"+suffix,
		"message-"+suffix,
		"chat_predict",
	)
	if err != nil {
		panic(err)
	}
	createdAt := time.Date(2026, time.July, 16, 12, 0, 0, 0, time.UTC)
	record := executiondomain.Admission{
		IdempotencyScope: "tenant-agent/1/7",
		IdempotencyKey:   "agent-request-" + suffix,
		RequestDigest:    runtimedomain.SHA256([]byte("agent-request:" + suffix)),
		InputBundle:      bundle,
		Job: executiondomain.Job{
			ID:                  ids[2],
			CommandID:           ids[3],
			TenantID:            "tenant-agent",
			ResourceProjectID:   "1",
			ProjectionProjectID: "1",
			ActorID:             "7",
			CapabilityID:        executiondomain.AgentAdhocCapability,
			Generation:          1,
			State:               executiondomain.JobPending,
			CreatedAt:           createdAt,
		},
		Outbox: executiondomain.OutboxRecord{
			ID:          ids[4],
			CommandID:   ids[3],
			ExecutionID: ids[2],
			Generation:  1,
			CreatedAt:   createdAt,
		},
	}
	if err := record.Validate(); err != nil {
		panic(err)
	}
	if err := binding.Validate(record.InputBundle); err != nil {
		panic(err)
	}
	return agentexecutionapp.Admission{Record: record, Binding: binding}
}

func assertPostgresAgentCapacityError(t *testing.T, err error, maxOutstanding int64) {
	t.Helper()
	if !errors.Is(err, executionapp.ErrAdmissionCapacityExhausted) {
		t.Fatalf("expected typed agent capacity error, got: %v", err)
	}
	var capacityError *executionapp.AdmissionCapacityError
	if !errors.As(err, &capacityError) || !capacityError.Retryable() || capacityError.MaxOutstanding != maxOutstanding {
		t.Fatalf("agent capacity error lost typed retry contract: %v", err)
	}
}

func assertPostgresAgentAdmissionState(
	t *testing.T,
	ctx context.Context,
	pool *pgxpool.Pool,
	total,
	active,
	unmaterialized,
	materialized int64,
) {
	t.Helper()
	const capability = "agent.execute.adhoc.v1"
	assertPostgresCount(t, ctx, pool, total, `SELECT count(*) FROM elitea_runtime.input_bundles`)
	assertPostgresCount(t, ctx, pool, total, `SELECT count(*) FROM elitea_runtime.input_bundle_entries`)
	assertPostgresCount(t, ctx, pool, total, `SELECT count(*) FROM elitea_runtime.execution_jobs`)
	assertPostgresCount(t, ctx, pool, total, `SELECT count(*) FROM elitea_runtime.command_outbox`)
	assertPostgresCount(t, ctx, pool, active, `
SELECT count(*)
FROM elitea_runtime.execution_jobs
WHERE capability_id = $1
  AND state IN ('PENDING', 'DISPATCHED', 'CLAIMED', 'RUNNING', 'SETTLING')`, capability)
	assertPostgresCount(t, ctx, pool, unmaterialized, `
SELECT count(*)
FROM elitea_runtime.agent_admission_reservations
WHERE capability_id = $1 AND materialized_at IS NULL`, capability)
	assertPostgresCount(t, ctx, pool, materialized, `
SELECT count(*)
FROM elitea_runtime.agent_admission_reservations
WHERE capability_id = $1 AND materialized_at IS NOT NULL`, capability)
}
