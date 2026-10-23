package repos

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	executionapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/execution"
	runtimedomain "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/runtime"
	"github.com/jackc/pgx/v5/pgxpool"
)

// TestPostgresServiceBackedExecutionAdmissionCapacity is a real PostgreSQL 16
// service-integration gate. Separate repository instances model independent
// application pods contending on the same durable policy row. This is not a
// multiprocess transport E2E, load, or soak test.
func TestPostgresServiceBackedExecutionAdmissionCapacity(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	assertPostgresAdmissionCapacitySchema(t, pool)

	const (
		maxOutstanding = int64(3)
		attempts       = 12
	)
	policy := testDispatchPolicy()
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
	admissions := make([]executionapp.ValidationAdmission, attempts)
	for index := range attempts {
		admissions[index] = postgresCapacityAdmission(index)
		repository, err := NewExecutionJobsRepository(pool, policy)
		if err != nil {
			t.Fatal(err)
		}
		go func(index int, repository *ExecutionJobsRepository) {
			<-start
			outcome, err := repository.AdmitValidation(ctx, admissions[index])
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
			var capacityError *executionapp.AdmissionCapacityError
			if !errors.As(result.err, &capacityError) || !capacityError.Retryable() || capacityError.MaxOutstanding != maxOutstanding {
				t.Fatalf("admission %d lost typed retry contract: %v", result.index, result.err)
			}
			capacityRejected++
		default:
			t.Fatalf("admission %d failed unexpectedly: %v", result.index, result.err)
		}
	}
	if len(created) != int(maxOutstanding) || capacityRejected != attempts-int(maxOutstanding) {
		t.Fatalf("cross-repository high-water gate created=%d rejected=%d", len(created), capacityRejected)
	}
	assertPostgresAdmissionRowCounts(t, ctx, pool, maxOutstanding, maxOutstanding, maxOutstanding)

	repository, err := NewExecutionJobsRepository(pool, policy)
	if err != nil {
		t.Fatal(err)
	}
	winner := admissions[created[0]]
	replay, err := repository.AdmitValidation(ctx, winner)
	if err != nil {
		t.Fatalf("exact replay at full capacity: %v", err)
	}
	if replay.Created || replay.ExecutionID != winner.Record.Job.ID || replay.CommandID != winner.Record.Job.CommandID {
		t.Fatalf("full-capacity replay changed durable identity: %+v", replay)
	}
	conflict := winner
	conflict.Record.RequestDigest = runtimedomain.SHA256([]byte("different-capacity-request"))
	if _, err := repository.AdmitValidation(ctx, conflict); !errors.Is(err, executionapp.ErrIdempotencyConflict) {
		t.Fatalf("conflicting replay at full capacity returned %v", err)
	}

	mismatchedPolicy := policy
	mismatchedPolicy.MaxOutstanding++
	mismatchedRepository, err := NewExecutionJobsRepository(pool, mismatchedPolicy)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := mismatchedRepository.AdmitValidation(ctx, postgresCapacityAdmission(attempts)); !errors.Is(err, ErrAdmissionPolicyMismatch) {
		t.Fatalf("persisted policy mismatch did not fail closed: %v", err)
	}

	if _, err := pool.Exec(ctx, `
UPDATE elitea_runtime.execution_jobs
SET state = 'SUCCEEDED', settled_at = clock_timestamp()
WHERE execution_id = $1 AND generation = $2`, winner.Record.Job.ID, int64(winner.Record.Job.Generation)); err != nil {
		t.Fatalf("terminalize admitted execution: %v", err)
	}
	replacement := postgresCapacityAdmission(attempts + 1)
	replacementOutcome, err := repository.AdmitValidation(ctx, replacement)
	if err != nil || !replacementOutcome.Created {
		t.Fatalf("terminal transition did not release one slot: outcome=%+v err=%v", replacementOutcome, err)
	}
	if _, err := repository.AdmitValidation(ctx, postgresCapacityAdmission(attempts+2)); !errors.Is(err, executionapp.ErrAdmissionCapacityExhausted) {
		t.Fatalf("replacement did not restore high-water mark: %v", err)
	}
	assertPostgresAdmissionRowCounts(t, ctx, pool, maxOutstanding+1, maxOutstanding, maxOutstanding)
}

// TestPostgresServiceBackedExecutionAdmissionClockAuthority proves that host
// clock skew cannot create already-expired or overlong commands. PostgreSQL
// authors one timestamp for every admission row and derives the deadline from
// the bounded policy TTL; exact replay returns that durable timing unchanged.
func TestPostgresServiceBackedExecutionAdmissionClockAuthority(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)

	policy := testDispatchPolicy()
	policy.DeadlineTTL = 10 * time.Minute
	repository, err := NewExecutionJobsRepository(pool, policy)
	if err != nil {
		t.Fatal(err)
	}
	outbox, err := NewCommandOutboxRepository(pool, policy.StreamName)
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	vectors := []struct {
		name string
		skew time.Duration
	}{
		{name: "host clock 24 hours ahead", skew: 24 * time.Hour},
		{name: "host clock 24 hours behind", skew: -24 * time.Hour},
	}
	for index, vector := range vectors {
		t.Run(vector.name, func(t *testing.T) {
			seed := postgresCapacityAdmission(100 + index)
			request := executionapp.SubmitValidationRequest{
				Identity: executionapp.AdmissionIdentity{
					TenantID:            seed.Record.Job.TenantID,
					ResourceProjectID:   seed.Record.Job.ResourceProjectID,
					ProjectionProjectID: seed.Record.Job.ProjectionProjectID,
					ActorID:             seed.Record.Job.ActorID,
				},
				IdempotencyKey: seed.Record.IdempotencyKey,
				InputBundle:    seed.Record.InputBundle,
				Command:        seed.Command,
			}

			var databaseBefore time.Time
			if err := pool.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&databaseBefore); err != nil {
				t.Fatalf("read database clock before admission: %v", err)
			}
			hostNow := databaseBefore.Add(vector.skew)
			ids := []string{
				seed.Record.Job.ID,
				seed.Record.Job.CommandID,
				seed.Record.Outbox.ID,
				"replay-execution-" + seed.Record.Job.ID,
				"replay-command-" + seed.Record.Job.CommandID,
				"replay-outbox-" + seed.Record.Outbox.ID,
			}
			service, err := executionapp.NewSubmitJobService(repository, func() time.Time {
				return hostNow
			}, func() (string, error) {
				if len(ids) == 0 {
					return "", errors.New("unexpected admission identity allocation")
				}
				id := ids[0]
				ids = ids[1:]
				return id, nil
			})
			if err != nil {
				t.Fatal(err)
			}

			created, err := service.SubmitValidation(ctx, request)
			if err != nil || !created.Created {
				t.Fatalf("admit with skewed host clock: outcome=%+v err=%v", created, err)
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
				t.Fatalf("load durable admission timing: %v", err)
			}
			if !inputCreatedAt.Equal(admittedAt) || !outboxCreatedAt.Equal(admittedAt) {
				t.Fatalf("admission rows have different clocks: input=%s job=%s outbox=%s", inputCreatedAt, admittedAt, outboxCreatedAt)
			}
			if deadline.Sub(admittedAt) != policy.DeadlineTTL {
				t.Fatalf("database deadline TTL=%s, want %s", deadline.Sub(admittedAt), policy.DeadlineTTL)
			}
			if admittedAt.Before(databaseBefore.Add(-time.Millisecond)) || admittedAt.After(databaseAfter) {
				t.Fatalf("admitted_at is outside the database observation window: before=%s admitted=%s after=%s", databaseBefore, admittedAt, databaseAfter)
			}
			if distance := admittedAt.Sub(hostNow); distance > -23*time.Hour && distance < 23*time.Hour {
				t.Fatalf("admitted_at followed the skewed host clock: host=%s admitted=%s", hostNow, admittedAt)
			}
			if !created.AdmittedAt.Equal(admittedAt) || !created.Deadline.Equal(deadline) {
				t.Fatalf("created outcome did not return database timing: outcome=%+v admitted=%s deadline=%s", created, admittedAt, deadline)
			}

			pending, err := outbox.ListPendingValidationIDs(ctx, 10, executionapp.MinOutboxVisibilityTimeout)
			if err != nil {
				t.Fatalf("list immediately publishable admission: %v", err)
			}
			if !containsAdmissionID(pending, seed.Record.Outbox.ID) {
				t.Fatalf("skewed host clock made a new admission ineligible for publication: pending=%v", pending)
			}

			// Change the injected host clock in the opposite direction. Exact replay
			// must still return the original database identity and timing.
			hostNow = databaseBefore.Add(-vector.skew)
			replayed, err := service.SubmitValidation(ctx, request)
			if err != nil {
				t.Fatalf("replay skewed admission: %v", err)
			}
			if replayed.Created || replayed.ExecutionID != created.ExecutionID || replayed.CommandID != created.CommandID || !replayed.AdmittedAt.Equal(created.AdmittedAt) || !replayed.Deadline.Equal(created.Deadline) {
				t.Fatalf("exact replay changed durable timing or identity: created=%+v replayed=%+v", created, replayed)
			}
		})
	}
}

func containsAdmissionID(values []string, wanted string) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}

func postgresCapacityAdmission(index int) executionapp.ValidationAdmission {
	admission := testValidationAdmission()
	suffix := fmt.Sprintf("capacity-%02d", index)
	admission.Record.IdempotencyKey = "request-" + suffix
	admission.Record.RequestDigest = runtimedomain.SHA256([]byte("request:" + suffix))
	admission.Record.InputBundle.ID = "bundle-" + suffix
	admission.Record.InputBundle.Entries[0].ContentID = "content-" + suffix
	admission.Record.Job.ID = "execution-" + suffix
	admission.Record.Job.CommandID = "command-" + suffix
	admission.Record.Outbox.ID = "outbox-" + suffix
	admission.Record.Outbox.ExecutionID = admission.Record.Job.ID
	admission.Record.Outbox.CommandID = admission.Record.Job.CommandID
	admission.Command.ConfigurationRevisionID = "revision-" + suffix
	return admission
}

func assertPostgresAdmissionCapacitySchema(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var indexDefinition string
	if err := pool.QueryRow(ctx, `
SELECT indexdef
FROM pg_indexes
WHERE schemaname = 'elitea_runtime'
  AND tablename = 'execution_jobs'
  AND indexname = 'execution_jobs_active_capability_idx'`).Scan(&indexDefinition); err != nil {
		t.Fatalf("load active-capability index: %v", err)
	}
	for _, state := range []string{"PENDING", "DISPATCHED", "CLAIMED", "RUNNING", "SETTLING"} {
		if !strings.Contains(indexDefinition, state) {
			t.Fatalf("active-capability index omits %s: %s", state, indexDefinition)
		}
	}
}

func assertPostgresAdmissionRowCounts(t *testing.T, ctx context.Context, pool *pgxpool.Pool, total, active, configuredMax int64) {
	t.Helper()
	assertPostgresCount(t, ctx, pool, total, `SELECT count(*) FROM elitea_runtime.input_bundles`)
	assertPostgresCount(t, ctx, pool, total, `SELECT count(*) FROM elitea_runtime.input_bundle_entries`)
	assertPostgresCount(t, ctx, pool, total, `SELECT count(*) FROM elitea_runtime.execution_jobs`)
	assertPostgresCount(t, ctx, pool, total, `SELECT count(*) FROM elitea_runtime.command_outbox`)
	assertPostgresCount(t, ctx, pool, active, `
SELECT count(*)
FROM elitea_runtime.execution_jobs
WHERE capability_id = 'configuration.validate.v1'
  AND state IN ('PENDING', 'DISPATCHED', 'CLAIMED', 'RUNNING', 'SETTLING')`)
	assertPostgresCount(t, ctx, pool, 1, `
SELECT count(*)
FROM elitea_runtime.execution_admission_policies
WHERE capability_id = 'configuration.validate.v1'
  AND max_outstanding = $1`, configuredMax)
}
