package runtimecomposition

// FU-299 C3 against a real PostgreSQL: a runner's per-cycle failures are
// DEBUG only while the readiness monitor reports the database lost, and an
// ERROR (once a minute, with the suppressed count) otherwise.

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/elitea-ng/elitea-platform/libs/go/faults"
	"github.com/elitea-ng/elitea-platform/libs/go/faults/logx"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/health"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/depfailure"
)

type cycleFixture struct {
	pool      *pgxpool.Pool
	proxy     *cuttableProxy
	records   *logRecords
	logger    *slog.Logger
	outages   *faults.Transitions
	reporters *cycleReporters
	monitor   *health.Monitor
}

func newCycleFixture(t *testing.T) *cycleFixture {
	t.Helper()
	direct := newArtifactRetentionPostgresPool(t)
	target := net.JoinHostPort(direct.Config().ConnConfig.Host, strconv.Itoa(int(direct.Config().ConnConfig.Port)))
	proxy := startCuttableProxy(t, target)
	config := direct.Config().Copy()
	config.ConnConfig.Host = "127.0.0.1"
	config.ConnConfig.Port = proxy.port(t)
	config.ConnConfig.ConnectTimeout = time.Second
	config.ConnConfig.Fallbacks = nil
	pool, err := pgxpool.NewWithConfig(context.Background(), config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	records := &logRecords{}
	logger := logx.New(records, logx.Options{Level: slog.LevelDebug})
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("captured log:\n%s", records.buf.String())
		}
	})
	outages := faults.NewTransitions()
	monitor, err := health.NewMonitor([]health.Check{{
		Name:     "db",
		Critical: true,
		Probe: func(ctx context.Context) (string, string) {
			if err := pool.Ping(ctx); err != nil {
				return health.StateUnavailable, depfailure.Describe(err)
			}
			return health.StateOK, ""
		},
		Lost:      faults.DependencyDatabaseUnavailable.Sentence(),
		Recovered: "Main reached its database again",
		Code:      faults.DependencyDatabaseUnavailable,
	}}, logger, 5*time.Second) // the production interval: the runner is faster
	if err != nil {
		t.Fatal(err)
	}
	monitor.WithOutages(outages)
	return &cycleFixture{
		pool: pool, proxy: proxy, records: records, logger: logger, outages: outages, monitor: monitor,
		reporters: newCycleReporters(Dependencies{Logger: logger, Outages: outages}),
	}
}

// runPasses runs a runner pass every 50 ms until ctx ends, reporting each
// failure through the composition's reporter set.
func (f *cycleFixture) runPasses(ctx context.Context, pass func(context.Context) error) *atomic.Int64 {
	failures := &atomic.Int64{}
	go func() {
		for ctx.Err() == nil {
			if err := pass(ctx); err != nil && ctx.Err() == nil {
				failures.Add(1)
				f.reporters.report("agent outbox publisher", err)
			}
			select {
			case <-ctx.Done():
			case <-time.After(50 * time.Millisecond):
			}
		}
	}()
	return failures
}

func TestADatabaseCutIsOneLossLineAndNoPerCycleErrors(t *testing.T) {
	f := newCycleFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go f.monitor.Run(ctx)
	failures := f.runPasses(ctx, func(ctx context.Context) error {
		_, err := f.pool.Exec(ctx, "SELECT 1")
		return err
	})
	time.Sleep(200 * time.Millisecond)

	f.proxy.Cut()
	eventually(t, "twenty failed runner passes during the cut", func() bool { return failures.Load() >= 20 })
	f.proxy.Restore()
	cancel()

	if n := countLines(t, f.records, "WARN", faults.DependencyDatabaseUnavailable.Sentence()); n != 1 {
		t.Fatalf("loss lines = %d, want exactly 1", n)
	}
	if n := countLines(t, f.records, "ERROR", faults.RunnerCycleFailed.Sentence()); n != 0 {
		t.Fatalf("per-cycle ERROR lines during the reported outage = %d, want 0", n)
	}
	if n := countLines(t, f.records, "DEBUG", faults.RunnerCycleFailed.Sentence()); n < 20 {
		t.Fatalf("DEBUG lines = %d, want every failure (at least 20)", n)
	}
}

func TestAQueryThatKeepsTimingOutOnAHealthyDatabaseIsNeverHidden(t *testing.T) {
	f := newCycleFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go f.monitor.Run(ctx)
	failures := f.runPasses(ctx, func(ctx context.Context) error {
		queryCtx, done := context.WithTimeout(ctx, 30*time.Millisecond)
		defer done()
		_, err := f.pool.Exec(queryCtx, "SELECT pg_sleep(1)")
		return err
	})
	eventually(t, "ten timed-out passes", func() bool { return failures.Load() >= 10 })
	cancel()

	if f.outages.IsLost("db") || countLines(t, f.records, "WARN", faults.DependencyDatabaseUnavailable.Sentence()) != 0 {
		t.Fatal("a healthy database was reported lost")
	}
	for _, entry := range f.records.entries(t) {
		if entry["level"] == "DEBUG" && entry["dependency"] != nil {
			t.Fatalf("a timing-out query on a healthy database was demoted as an outage: %v", entry)
		}
	}
	if n := countLines(t, f.records, "ERROR", faults.RunnerCycleFailed.Sentence()); n != 1 {
		t.Fatalf("ERROR lines = %d, want 1 (the rest of the minute is counted, not logged)", n)
	}
}

// Twelve runners failing together at the start of an outage all wait for the
// one confirming evaluation, so none of them logs an ERROR (FU-299 C3).
func TestManyRunnersFailingAtOnceStillLogNoPerCycleErrors(t *testing.T) {
	f := newCycleFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go f.monitor.Run(ctx)
	time.Sleep(200 * time.Millisecond)

	f.proxy.Cut()
	var wg sync.WaitGroup
	for runner := range 12 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 3 {
				if _, err := f.pool.Exec(ctx, "SELECT 1"); err != nil {
					f.reporters.report(fmt.Sprintf("runner %d", runner), err)
				}
			}
		}()
	}
	wg.Wait()
	f.proxy.Restore()
	cancel()

	if n := countLines(t, f.records, "ERROR", faults.RunnerCycleFailed.Sentence()); n != 0 {
		t.Fatalf("per-cycle ERROR lines from runners failing together = %d, want 0", n)
	}
	if n := countLines(t, f.records, "WARN", faults.DependencyDatabaseUnavailable.Sentence()); n != 1 {
		t.Fatalf("loss lines = %d, want exactly 1", n)
	}
}
