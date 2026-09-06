package policy

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// flakyDB fails its first `failures` queries and then behaves like fakeDB. It
// reproduces the fresh-install race: the gateway reads
// gateway.governance_config before elitea-migrate creates it.
type flakyDB struct {
	rows     [][]any
	failures int
	calls    int
}

var errNoRelation = errors.New(
	`relation "gateway.governance_config" does not exist (SQLSTATE 42P01)`)

func (f *flakyDB) Query(context.Context, string, ...any) (pgx.Rows, error) {
	f.calls++
	if f.calls <= f.failures {
		return nil, errNoRelation
	}
	return &fakeRows{rows: f.rows}, nil
}

// TestInitialLoadRetriesPastAMigrationRace is the fresh-install case.
//
// Before the retry schedule the first read failed, Start returned, and the
// store waited a whole DefaultRefreshInterval — 30 seconds — before it tried
// again. The gateway served requests throughout, enforcing no model allowlist,
// no MCP allowlist, no rate policy and no authored egress entry.
func TestInitialLoadRetriesPastAMigrationRace(t *testing.T) {
	t.Parallel()

	db := &flakyDB{
		failures: 2,
		rows: [][]any{
			dbRow("1", TypeRateLimit, "cap", `{"rate_limit":{"requests_per_min":10}}`),
		},
	}
	s := NewStore(Config{
		DB: db, Logger: quietLogger(),
		RefreshInterval:   time.Hour, // only the initial load can have run
		InitialRetryDelay: time.Microsecond,
		Now:               func() time.Time { return testNow },
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	s.Start(ctx)

	if _, ok := s.Current().RateLimit(Subject{ProjectID: 1}); !ok {
		t.Fatal("Start gave up on the first failure: the gateway enforces " +
			"nothing until the poll interval elapses")
	}
	if db.calls != 3 {
		t.Fatalf("database queried %d times, want 3 (two failures, one success)", db.calls)
	}
	if st := s.Status(); st.Error != "" || st.LastSuccess == "" {
		t.Fatalf("status after a recovered load = %+v", st)
	}
}

// TestInitialLoadStopsAfterTheBoundedSchedule holds the other half. A gateway
// whose database is genuinely unreachable must still finish booting: the
// schedule is bounded, and convergence then belongs to the poll loop.
func TestInitialLoadStopsAfterTheBoundedSchedule(t *testing.T) {
	t.Parallel()

	db := &flakyDB{failures: 1000}
	s := NewStore(Config{
		DB: db, Logger: quietLogger(),
		RefreshInterval:   time.Hour,
		InitialRetryDelay: time.Microsecond,
		Now:               func() time.Time { return testNow },
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	s.Start(ctx)

	if db.calls != initialLoadAttempts {
		t.Fatalf("database queried %d times, want %d", db.calls, initialLoadAttempts)
	}
	if s.Current() != Empty {
		t.Fatal("a store that never loaded is not on the Empty snapshot")
	}
	if st := s.Status(); st.Error == "" || st.LastSuccess != "" {
		t.Fatalf("status after an exhausted schedule = %+v", st)
	}
}

// TestInitialLoadDoesNotRetryWithoutADatabase keeps the schedule off the boot
// path of a gateway that has no pool. ErrNoDatabase is a composition fact, so
// retrying it would add the whole schedule to every such boot and change
// nothing. The DEFAULT delay is used deliberately: a regression that retried
// would take about ten seconds here.
func TestInitialLoadDoesNotRetryWithoutADatabase(t *testing.T) {
	t.Parallel()

	s := NewStore(Config{Logger: quietLogger(), RefreshInterval: time.Hour})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	start := time.Now()
	s.Start(ctx)
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("Start took %s without a database: the retry schedule must "+
			"not run on ErrNoDatabase", elapsed)
	}
	if s.Status().HasDatabase {
		t.Fatal("a store built without a pool reported a database")
	}
}

// TestInitialLoadStopsOnACancelledContext proves a shutdown during boot is not
// held up by the retry schedule.
func TestInitialLoadStopsOnACancelledContext(t *testing.T) {
	t.Parallel()

	db := &flakyDB{failures: 1000}
	s := NewStore(Config{
		DB: db, Logger: quietLogger(),
		RefreshInterval:   time.Hour,
		InitialRetryDelay: time.Hour, // only cancellation can end the wait
		Now:               func() time.Time { return testNow },
	})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	start := time.Now()
	s.Start(ctx)
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("Start took %s on a cancelled context", elapsed)
	}
	if db.calls != 1 {
		t.Fatalf("database queried %d times on a cancelled context, want 1", db.calls)
	}
}
