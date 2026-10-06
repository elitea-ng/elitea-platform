package repos

import (
	"context"
	"errors"
	"fmt"
	"os"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	executionapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/execution"
	runtimedomain "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/runtime"
	"github.com/jackc/pgx/v5/pgxpool"
)

const postgresReliabilityOptIn = "ELITEA_RUNTIME_POSTGRES_RELIABILITY_TEST"

// TestPostgresServiceBackedCommandBusOutageBacklogReliability is an opt-in
// real PostgreSQL 16 service-integration reliability test with an injected
// command-bus adapter outage. It proves bounded concurrent admission, atomic input/job/
// outbox persistence, unpublished backlog retention, and exact-envelope reuse
// by replacement publisher instances. It does not stop a real NATS server
// and is not a cross-process E2E, failover, or soak claim.
func TestPostgresServiceBackedCommandBusOutageBacklogReliability(t *testing.T) {
	if os.Getenv(postgresReliabilityOptIn) != "1" {
		t.Skipf("set %s=1 with %s to run the PostgreSQL backlog reliability test", postgresReliabilityOptIn, postgresIntegrationDatabaseURL)
	}
	pool := newMigratedPostgresIntegrationPool(t)

	const (
		maxOutstanding    = int64(64)
		admissionAttempts = 128
		admissionWorkers  = 16
		publisherWorkers  = 8
	)
	policy := testDispatchPolicy()
	policy.MaxOutstanding = maxOutstanding
	policy.DeadlineTTL = 10 * time.Minute

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	type admissionResult struct {
		outcome executionapp.AdmissionOutcome
		err     error
	}
	jobs := make(chan int)
	results := make(chan admissionResult, admissionAttempts)
	var admissionGroup sync.WaitGroup
	admissionGroup.Add(admissionWorkers)
	for range admissionWorkers {
		repository, err := NewExecutionJobsRepository(pool, policy)
		if err != nil {
			t.Fatal(err)
		}
		go func(repository *ExecutionJobsRepository) {
			defer admissionGroup.Done()
			for index := range jobs {
				admission := postgresCapacityAdmission(index)
				now := time.Now().UTC()
				admission.Record.Job.CreatedAt = now
				admission.Record.Outbox.CreatedAt = now
				outcome, err := repository.AdmitValidation(ctx, admission)
				results <- admissionResult{outcome: outcome, err: err}
			}
		}(repository)
	}
	for index := range admissionAttempts {
		jobs <- index
	}
	close(jobs)
	admissionGroup.Wait()
	close(results)

	created := 0
	rejected := 0
	for result := range results {
		switch {
		case result.err == nil && result.outcome.Created:
			created++
		case errors.Is(result.err, executionapp.ErrAdmissionCapacityExhausted):
			rejected++
		default:
			t.Fatalf("unexpected concurrent admission outcome=%+v err=%v", result.outcome, result.err)
		}
	}
	if created != int(maxOutstanding) || rejected != admissionAttempts-int(maxOutstanding) {
		t.Fatalf("created=%d rejected=%d, want %d/%d", created, rejected, maxOutstanding, admissionAttempts-int(maxOutstanding))
	}
	assertPostgresAdmissionRowCounts(t, ctx, pool, maxOutstanding, maxOutstanding, maxOutstanding)

	outage := errors.New("injected command bus unavailable")
	first := &backlogReliabilityProducer{appendFailure: outage}
	runBacklogPublisherCycle(t, ctx, pool, policy.StreamName, first, publisherWorkers, outage)
	assertBacklogPublicationState(t, ctx, pool, maxOutstanding, 0, 0)
	if first.prepareCalls.Load() != maxOutstanding || first.appendCalls.Load() != maxOutstanding || first.maximum.Load() > publisherWorkers {
		t.Fatalf("first outage cycle prepares=%d appends=%d max_concurrent=%d", first.prepareCalls.Load(), first.appendCalls.Load(), first.maximum.Load())
	}
	firstBytes := first.appendedSnapshot()

	// A replacement publisher models a restarted process. It must load the
	// exact durable envelope instead of signing/selecting new bytes.
	second := &backlogReliabilityProducer{appendFailure: outage}
	runBacklogPublisherCycle(t, ctx, pool, policy.StreamName, second, publisherWorkers, outage)
	assertBacklogPublicationState(t, ctx, pool, maxOutstanding, 0, 0)
	if second.prepareCalls.Load() != 0 || second.appendCalls.Load() != maxOutstanding || second.maximum.Load() > publisherWorkers {
		t.Fatalf("replacement outage cycle prepares=%d appends=%d max_concurrent=%d", second.prepareCalls.Load(), second.appendCalls.Load(), second.maximum.Load())
	}
	assertExactBacklogBytes(t, firstBytes, second.appendedSnapshot())

	recovered := &backlogReliabilityProducer{}
	runBacklogPublisherCycle(t, ctx, pool, policy.StreamName, recovered, publisherWorkers, nil)
	assertBacklogPublicationState(t, ctx, pool, maxOutstanding, maxOutstanding, maxOutstanding)
	if recovered.prepareCalls.Load() != 0 || recovered.appendCalls.Load() != maxOutstanding || recovered.maximum.Load() > publisherWorkers {
		t.Fatalf("recovery cycle prepares=%d appends=%d max_concurrent=%d", recovered.prepareCalls.Load(), recovered.appendCalls.Load(), recovered.maximum.Load())
	}
	assertExactBacklogBytes(t, firstBytes, recovered.appendedSnapshot())
	if stats := pool.Stat(); stats.TotalConns() > 12 || stats.AcquiredConns() != 0 {
		t.Fatalf("PostgreSQL reliability pool total=%d acquired=%d, want <=12/0", stats.TotalConns(), stats.AcquiredConns())
	}
}

type backlogReliabilityProducer struct {
	appendFailure error
	prepareCalls  atomic.Int64
	appendCalls   atomic.Int64
	current       atomic.Int64
	maximum       atomic.Int64
	mu            sync.Mutex
	appended      map[string]int
}

func (p *backlogReliabilityProducer) PrepareValidation(_ context.Context, dispatch executionapp.ValidationDispatch) (executionapp.PreparedCommandEnvelope, error) {
	p.prepareCalls.Add(1)
	encoded := []byte("reliability-envelope:" + dispatch.OutboxID)
	return executionapp.PreparedCommandEnvelope{
		Bytes:            encoded,
		Digest:           runtimedomain.SHA256(encoded),
		SignatureProfile: 1,
		KeyID:            "reliability-key-v1",
	}, nil
}

func (p *backlogReliabilityProducer) AppendPrepared(_ context.Context, deliveryID string, prepared executionapp.PreparedCommandEnvelope) error {
	current := p.current.Add(1)
	defer p.current.Add(-1)
	p.appendCalls.Add(1)
	for {
		maximum := p.maximum.Load()
		if current <= maximum || p.maximum.CompareAndSwap(maximum, current) {
			break
		}
	}
	p.mu.Lock()
	if p.appended == nil {
		p.appended = make(map[string]int)
	}
	p.appended[deliveryID+"\x00"+string(prepared.Bytes)]++
	p.mu.Unlock()
	runtime.Gosched()
	return p.appendFailure
}

func (p *backlogReliabilityProducer) appendedSnapshot() map[string]int {
	p.mu.Lock()
	defer p.mu.Unlock()
	copy := make(map[string]int, len(p.appended))
	for encoded, count := range p.appended {
		copy[encoded] = count
	}
	return copy
}

func runBacklogPublisherCycle(t *testing.T, ctx context.Context, pool *pgxpool.Pool, stream string, producer executionapp.ReferenceCommandProducer, concurrency int, expectedError error) {
	t.Helper()
	outbox, err := NewCommandOutboxRepository(pool, stream)
	if err != nil {
		t.Fatal(err)
	}
	dispatcher, err := executionapp.NewValidationDispatcher(outbox, producer)
	if err != nil {
		t.Fatal(err)
	}
	publisher, err := executionapp.NewOutboxPublisher(outbox, dispatcher, executionapp.OutboxPublisherConfig{
		PollInterval:      time.Second,
		VisibilityTimeout: time.Minute,
		BatchSize:         64,
		MaxConcurrent:     concurrency,
		ReportFailure:     func(error) {},
	})
	if err != nil {
		t.Fatal(err)
	}
	err = publisher.RunOnce(ctx)
	if expectedError == nil && err != nil {
		t.Fatalf("recovery publisher cycle: %v", err)
	}
	if expectedError != nil && !errors.Is(err, expectedError) {
		t.Fatalf("outage publisher failure identity=%v, want %v", err, expectedError)
	}
}

func assertBacklogPublicationState(t *testing.T, ctx context.Context, pool *pgxpool.Pool, prepared, published, dispatched int64) {
	t.Helper()
	var preparedCount, publishedCount, dispatchedCount, publishAttempts int64
	if err := pool.QueryRow(ctx, `
SELECT count(*) FILTER (WHERE prepared_signed_envelope_bytes IS NOT NULL),
       count(*) FILTER (WHERE published_at IS NOT NULL),
       COALESCE(sum(publish_attempts), 0)
FROM elitea_runtime.command_outbox`).Scan(&preparedCount, &publishedCount, &publishAttempts); err != nil {
		t.Fatalf("read durable outage backlog: %v", err)
	}
	if err := pool.QueryRow(ctx, `
SELECT count(*)
FROM elitea_runtime.execution_jobs
WHERE state = 'DISPATCHED'`).Scan(&dispatchedCount); err != nil {
		t.Fatalf("read dispatched reliability jobs: %v", err)
	}
	if preparedCount != prepared || publishedCount != published || dispatchedCount != dispatched || publishAttempts != published {
		t.Fatalf("backlog prepared=%d published=%d dispatched=%d attempts=%d, want %d/%d/%d/%d", preparedCount, publishedCount, dispatchedCount, publishAttempts, prepared, published, dispatched, published)
	}
}

func assertExactBacklogBytes(t *testing.T, expected, actual map[string]int) {
	t.Helper()
	if len(actual) != len(expected) {
		t.Fatalf("replacement publisher envelope identities=%d, want %d", len(actual), len(expected))
	}
	for encoded, expectedCount := range expected {
		if actual[encoded] != expectedCount {
			t.Fatalf("replacement publisher changed/dropped durable envelope %q: count=%d want=%d", fmt.Sprintf("%.64s", encoded), actual[encoded], expectedCount)
		}
	}
}
