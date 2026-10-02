package repos

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type agentAdmissionClockTracer struct {
	reads atomic.Int64
}

func (t *agentAdmissionClockTracer) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	if strings.HasPrefix(data.SQL, "-- name: LoadRuntimeAdmissionTiming :one") {
		t.reads.Add(1)
	}
	return ctx
}

func (*agentAdmissionClockTracer) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

func TestPostgresAgentAdmissionPersistsOneDatabaseAuthoredClock(t *testing.T) {
	privatePool := newMigratedPostgresIntegrationPool(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	clockTracer := &agentAdmissionClockTracer{}
	config := privatePool.Config()
	config.MaxConns = 2
	config.ConnConfig.Tracer = clockTracer
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	policy := testAgentDispatchPolicy()
	policy.DeadlineTTL = 10 * time.Minute
	repository, err := NewAgentExecutionJobsRepository(pool, policy, 1)
	if err != nil {
		t.Fatal(err)
	}

	for index, vector := range []struct {
		name string
		skew time.Duration
	}{
		{name: "caller_clock_ahead", skew: 24 * time.Hour},
		{name: "caller_clock_behind", skew: -24 * time.Hour},
	} {
		t.Run(vector.name, func(t *testing.T) {
			var databaseBefore time.Time
			if err := pool.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&databaseBefore); err != nil {
				t.Fatalf("read database clock before admission: %v", err)
			}
			admission := postgresAgentCapacityAdmission(850 + index)
			callerNow := databaseBefore.Add(vector.skew)
			admission.Record.Job.CreatedAt = callerNow
			admission.Record.Outbox.CreatedAt = callerNow
			clockReadsBefore := clockTracer.reads.Load()
			created, err := repository.AdmitAgentExecution(ctx, admission)
			if err != nil || !created.Created {
				t.Fatalf("admit agent with skewed caller clock: outcome=%+v err=%v", created, err)
			}
			if clockTracer.reads.Load() != clockReadsBefore+1 {
				t.Fatal("agent materialization does not read the database admission clock")
			}

			var inputCreatedAt, admittedAt, outboxCreatedAt, deadline, databaseAfter time.Time
			if err := pool.QueryRow(ctx, `
SELECT b.created_at, j.admitted_at, o.created_at, o.deadline, clock_timestamp()
FROM elitea_runtime.execution_jobs AS j
JOIN elitea_runtime.input_bundles AS b
  ON b.input_bundle_id = j.input_bundle_id
JOIN elitea_runtime.command_outbox AS o
  ON o.execution_id = j.execution_id AND o.generation = j.generation
WHERE j.execution_id = $1 AND j.generation = 1`, created.ExecutionID).Scan(
				&inputCreatedAt,
				&admittedAt,
				&outboxCreatedAt,
				&deadline,
				&databaseAfter,
			); err != nil {
				t.Fatalf("load durable agent admission timing: %v", err)
			}
			if !inputCreatedAt.Equal(admittedAt) || !outboxCreatedAt.Equal(admittedAt) {
				t.Fatalf("agent admission rows have different clocks: input=%s job=%s outbox=%s", inputCreatedAt, admittedAt, outboxCreatedAt)
			}
			if deadline.Sub(admittedAt) != policy.DeadlineTTL {
				t.Fatalf("agent admission deadline TTL=%s, want %s", deadline.Sub(admittedAt), policy.DeadlineTTL)
			}
			if admittedAt.Before(databaseBefore.Add(-time.Millisecond)) || admittedAt.After(databaseAfter) {
				t.Fatalf("agent admitted_at is outside database bounds: before=%s admitted=%s after=%s", databaseBefore, admittedAt, databaseAfter)
			}
			if distance := admittedAt.Sub(callerNow); distance > -23*time.Hour && distance < 23*time.Hour {
				t.Fatalf("agent admitted_at followed the caller clock: caller=%s admitted=%s", callerNow, admittedAt)
			}
			if !created.AdmittedAt.Equal(admittedAt) || !created.Deadline.Equal(deadline) {
				t.Fatalf("agent outcome changed database timing: outcome=%+v admitted=%s deadline=%s", created, admittedAt, deadline)
			}
			pending, err := repository.ListPendingAgentExecutionIDs(ctx, 4, time.Minute)
			if err != nil {
				t.Fatalf("list publishable agent admission: %v", err)
			}
			if !containsAdmissionID(pending, admission.Record.Outbox.ID) {
				t.Fatalf("agent admission is not publishable under the database deadline: pending=%v", pending)
			}

			admission.Record.Job.CreatedAt = databaseBefore.Add(-vector.skew)
			admission.Record.Outbox.CreatedAt = admission.Record.Job.CreatedAt
			replayed, err := repository.AdmitAgentExecution(ctx, admission)
			if err != nil {
				t.Fatalf("replay agent admission with changed caller clock: %v", err)
			}
			if replayed.Created || replayed.ExecutionID != created.ExecutionID || replayed.CommandID != created.CommandID ||
				!replayed.AdmittedAt.Equal(created.AdmittedAt) || !replayed.Deadline.Equal(created.Deadline) {
				t.Fatalf("agent replay changed durable identity or timing: created=%+v replayed=%+v", created, replayed)
			}
			if clockTracer.reads.Load() != clockReadsBefore+1 {
				t.Fatal("agent replay reads a new admission clock instead of retaining the original timing")
			}
		})
	}
}
