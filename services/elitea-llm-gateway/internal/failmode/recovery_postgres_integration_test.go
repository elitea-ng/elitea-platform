package failmode

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// recovery_postgres_integration_test.go runs the issue #515 wedge against a
// REAL PostgreSQL, with the SHIPPED DDL.
//
// The unit tests model the two writers. This one does not model anything: it
// executes the production statements — outageDedupSQL, outageUpsertSQL,
// selectOutageRowsSQL, finalizeRowSQL and countOutageRowsSQL — against the
// tables services/elitea-main/migrations/shared creates. Three of those
// statements were written for this fix and had never been parsed by Postgres;
// a model cannot report a syntax error, a wrong column name or a type the
// column will not take.
//
// It runs only when ELITEA_TEST_DATABASE_URL names a database:
//
//	podman run -d --name pg -e POSTGRES_PASSWORD=postgres -p 55515:5432 postgres:16-trixie
//	ELITEA_TEST_DATABASE_URL='postgres://postgres:postgres@127.0.0.1:55515/postgres?sslmode=disable' \
//	  GOWORK=off go test -run TestPostgres ./internal/failmode/

const databaseURLEnv = "ELITEA_TEST_DATABASE_URL"

// migrationFiles are the shipped files that create every table this test
// touches. They are READ from the repository rather than copied here, so a
// column this code needs and the migration does not create fails the test.
var migrationFiles = []string{
	"0067_gateway_budget_schema.sql",
	"0084_budget_usage_dimensions.sql",
}

// writeBackUpsertSQL is the write-back consumer's guarded UPSERT. The consumer
// lives in another Go module (services/elitea-scheduler,
// internal/budgetwriteback/store.go upsertSQL) which this module cannot import,
// so the statement is repeated here — as it already is in
// services/elitea-main/migrations/corpus_postgres_integration_test.go. What it
// proves is the half of the invariant this package cannot reach on its own:
// while the row is outage-owned the consumer's UPSERT matches NO row, and after
// the recovery pass releases it the same statement applies.
const writeBackUpsertSQL = `INSERT INTO gateway.llm_budget_accumulators AS acc
		(project_id, org_id, scope, scope_id, period_start, period_end,
		 accumulated_cost, outage_mode, reconciled, last_updated)
	VALUES ($1, $2, $3, $4, to_timestamp($5), to_timestamp($6),
		$7::numeric / 1000000000, false, false, now())
	ON CONFLICT (scope, scope_id, period_start) DO UPDATE SET
		accumulated_cost = acc.accumulated_cost + EXCLUDED.accumulated_cost,
		last_updated = now()
	WHERE NOT (acc.outage_mode AND NOT acc.reconciled)`

// openTestPool dials the test database, applies the shipped gateway DDL and
// returns a pool. It skips the test when no database is configured.
func openTestPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	url := os.Getenv(databaseURLEnv)
	if url == "" {
		t.Skipf("set %s to run the outage-recovery integration test", databaseURLEnv)
	}
	ctx := t.Context()
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)

	for _, name := range migrationFiles {
		path := filepath.Join("..", "..", "..", "elitea-main", "migrations", "shared", name)
		sql, err := os.ReadFile(path) //nolint:gosec // a fixed in-repo path
		if err != nil {
			t.Skipf("cannot read the shipped migration %s: %v", name, err)
		}
		if _, err := pool.Exec(ctx, string(sql)); err != nil {
			t.Fatalf("apply %s: %v", name, err)
		}
	}
	return pool
}

// pgFixture is one isolated project/scope for a single test.
type pgFixture struct {
	pool      *pgxpool.Pool
	store     *Store
	db        DB
	projectID int
	scopeID   string
	period    int64
	periodEnd int64
}

func newPGFixture(t *testing.T) *pgFixture {
	t.Helper()
	pool := openTestPool(t)
	ctx := t.Context()

	// A project id and a period unique to this test, so parallel runs and
	// left-over rows from an earlier run cannot interfere.
	projectID := 900_000 + int(time.Now().UnixNano()%90_000)
	scopeID := fmt.Sprint(projectID)
	period := time.Now().Unix() - int64(projectID)

	if _, err := pool.Exec(ctx,
		`INSERT INTO gateway.project_budget (project_id, hard_limit_usd, is_unlimited, soft_alert_pct)
		 VALUES ($1, 100, false, 80)`, projectID); err != nil {
		t.Fatalf("seed project_budget: %v", err)
	}
	t.Cleanup(func() {
		bg := context.Background()
		_, _ = pool.Exec(bg, `DELETE FROM gateway.llm_budget_accumulators WHERE project_id = $1`, projectID)
		_, _ = pool.Exec(bg, `DELETE FROM gateway.llm_usage_events WHERE project_id = $1`, projectID)
		_, _ = pool.Exec(bg, `DELETE FROM gateway.project_budget WHERE project_id = $1`, projectID)
	})

	db := NewPoolDB(pool)
	return &pgFixture{
		pool: pool, store: NewStore(db), db: db,
		projectID: projectID, scopeID: scopeID,
		period: period, periodEnd: period + 86_400,
	}
}

// outageDelta builds a billed outage-window delta for this fixture.
func (f *pgFixture) outageDelta(eventID string, nano int64) OutageDelta {
	return OutageDelta{
		ProjectID:    f.projectID,
		Scope:        ScopeProject,
		ScopeID:      f.scopeID,
		EventID:      eventID,
		PeriodStart:  f.period,
		PeriodEnd:    f.periodEnd,
		DeltaNanoUSD: nano,
	}
}

// state reads the accumulated nano-USD and the outage flags straight out of the
// table.
func (f *pgFixture) state(t *testing.T) (nano int64, outage, reconciled bool) {
	t.Helper()
	err := f.pool.QueryRow(t.Context(),
		`SELECT (accumulated_cost * $3)::bigint, outage_mode, reconciled
		   FROM gateway.llm_budget_accumulators
		  WHERE scope = $1 AND scope_id = $2`,
		ScopeProject, f.scopeID, NanoUSD).Scan(&nano, &outage, &reconciled)
	if err != nil {
		t.Fatalf("read accumulator: %v", err)
	}
	return nano, outage, reconciled
}

// writeBack runs the consumer's guarded UPSERT and reports whether it applied.
func (f *pgFixture) writeBack(t *testing.T, nano int64) bool {
	t.Helper()
	tag, err := f.pool.Exec(t.Context(), writeBackUpsertSQL,
		f.projectID, nil, ScopeProject, f.scopeID, f.period, f.periodEnd, nano)
	if err != nil {
		t.Fatalf("write-back upsert: %v", err)
	}
	return tag.RowsAffected() == 1
}

// reconciler builds a Reconciler over the real pool and the given counter.
func (f *pgFixture) reconciler(c Counter) *Reconciler {
	r := NewReconciler(f.db, c, NewDegradedCounters(), nil)
	r.sweepInterval = 0 // driven by SweepOnce, never by a ticker, in this test
	return r
}

// TestPostgres_OutageRowWedgesTheWriteBackAndTheSweepClearsIt is the whole
// defect and the whole fix, against real SQL.
func TestPostgres_OutageRowWedgesTheWriteBackAndTheSweepClearsIt(t *testing.T) {
	f := newPGFixture(t)
	ctx := t.Context()

	// One failed counter operation: the gateway writes the outage-window row.
	if err := f.store.PersistOutageDelta(ctx, f.outageDelta(newUUID(t), 3*NanoUSD)); err != nil {
		t.Fatalf("persist outage delta: %v", err)
	}
	nano, outage, reconciled := f.state(t)
	if nano != 3*NanoUSD || !outage || reconciled {
		t.Fatalf("after the outage write: nano=%d outage=%v reconciled=%v", nano, outage, reconciled)
	}

	// The write-back consumer is now barred from the row. This is the wedge:
	// every later delta for this scope matches no row and is deferred.
	if f.writeBack(t, 4*NanoUSD) {
		t.Fatal("the write-back UPSERT applied to an outage-owned row; the guard is not holding")
	}
	if f.writeBack(t, 5*NanoUSD) {
		t.Fatal("the second write-back UPSERT applied to an outage-owned row")
	}
	if nano, _, _ := f.state(t); nano != 3*NanoUSD {
		t.Fatalf("durable spend = %d, want it frozen at %d while the row is wedged", nano, 3*NanoUSD)
	}

	// The gauge read reports the held row.
	if got := countOutageRows(t, f.pool); got < 1 {
		t.Fatalf("countOutageRowsSQL returned %d, want at least the one held row", got)
	}

	// The sweep, with no breaker edge anywhere.
	counter := newFakeCounter()
	counter.totals[counter.BudgetSubject(ScopeProject, f.scopeID, f.period)] = 3 * NanoUSD
	f.reconciler(counter).SweepOnce(ctx)

	_, outage, reconciled = f.state(t)
	if outage || !reconciled {
		t.Fatalf("after the sweep: outage=%v reconciled=%v, want false/true", outage, reconciled)
	}

	// The consumer owns the row again and its deferred deltas apply.
	if !f.writeBack(t, 4*NanoUSD) || !f.writeBack(t, 5*NanoUSD) {
		t.Fatal("the write-back UPSERT still matches no row after recovery")
	}
	if nano, _, _ := f.state(t); nano != 12*NanoUSD {
		t.Fatalf("durable spend = %d, want all three amounts (%d)", nano, 12*NanoUSD)
	}
}

// TestPostgres_SweepKeepsTheRowWhileNATSIsDown is the negative control against
// real SQL: a scope that is genuinely in outage keeps its row, and the
// write-back consumer stays barred from it.
func TestPostgres_SweepKeepsTheRowWhileNATSIsDown(t *testing.T) {
	f := newPGFixture(t)
	ctx := t.Context()

	if err := f.store.PersistOutageDelta(ctx, f.outageDelta(newUUID(t), 2*NanoUSD)); err != nil {
		t.Fatalf("persist outage delta: %v", err)
	}

	counter := newFakeCounter()
	r := f.reconciler(counter)
	down := false
	r.SetHealthCheck(func() bool { return down })
	for range 5 {
		r.SweepOnce(ctx)
	}
	if _, outage, _ := f.state(t); !outage {
		t.Fatal("the sweep released a row while NATS was unreachable")
	}
	if f.writeBack(t, NanoUSD) {
		t.Fatal("the write-back UPSERT applied while the row was still held")
	}

	// NATS returns; the same sweep releases the row.
	down = true
	r.SweepOnce(ctx)
	if _, outage, _ := f.state(t); outage {
		t.Fatal("the sweep did not release the row after NATS returned")
	}
}

// TestPostgres_OutageWriteIsExactlyOnceAgainstTheConsumer proves the dedup gate
// on real SQL, in both orders. The delta of a request that entered the outage
// window on a single failed increment IS published, so the consumer sees it;
// only gateway.processed_event_ids stops the money being counted twice.
func TestPostgres_OutageWriteIsExactlyOnceAgainstTheConsumer(t *testing.T) {
	f := newPGFixture(t)
	ctx := t.Context()

	// Order A — the gateway first. A consumer that later claims the same id
	// finds it taken and contributes nothing.
	eventA := newUUID(t)
	if err := f.store.PersistOutageDelta(ctx, f.outageDelta(eventA, 3*NanoUSD)); err != nil {
		t.Fatalf("persist outage delta: %v", err)
	}
	if claimed := claimEvent(t, f.pool, eventA); claimed {
		t.Fatal("the consumer claimed an event id the outage write had already taken")
	}

	// Order B — the consumer first. The outage write must add nothing and must
	// NOT flag a row whose money is already durable.
	eventB := newUUID(t)
	if claimed := claimEvent(t, f.pool, eventB); !claimed {
		t.Fatal("a fresh event id was not claimable")
	}
	before, _, _ := f.state(t)
	if err := f.store.PersistOutageDelta(ctx, f.outageDelta(eventB, 9*NanoUSD)); err != nil {
		t.Fatalf("persist outage delta: %v", err)
	}
	after, _, _ := f.state(t)
	if after != before {
		t.Fatalf("durable spend moved from %d to %d on an already-applied event", before, after)
	}
}

// TestPostgres_SnapshotStillReadsTheDurableTier keeps the shipped tiered_hybrid
// mode working: acc_found and age_seconds must still come off the accumulator
// row, both while it is outage-owned and after the sweep releases it. They are
// the durable tier of enforcement, and a fix that broke them would trade a
// frozen figure for no figure at all.
func TestPostgres_SnapshotStillReadsTheDurableTier(t *testing.T) {
	f := newPGFixture(t)
	ctx := t.Context()

	// Before any spend: no accumulator row, so a fresh zero snapshot.
	snap, err := f.store.ReadSnapshot(ctx, f.projectID, ScopeProject, f.scopeID, f.period)
	if err != nil {
		t.Fatalf("read snapshot: %v", err)
	}
	if snap.Found || snap.AccumulatedNano != 0 || snap.HardLimitNano != 100*NanoUSD {
		t.Fatalf("empty-period snapshot = %+v", snap)
	}

	if err := f.store.PersistOutageDelta(ctx, f.outageDelta(newUUID(t), 90*NanoUSD)); err != nil {
		t.Fatalf("persist outage delta: %v", err)
	}

	// While the row is outage-owned the durable tier still reports it, and the
	// FSM still blocks a request that is over the ceiling.
	snap, err = f.store.ReadSnapshot(ctx, f.projectID, ScopeProject, f.scopeID, f.period)
	if err != nil {
		t.Fatalf("read snapshot: %v", err)
	}
	if !snap.Found {
		t.Fatal("acc_found is false while an outage row exists; the durable tier cannot see the spend")
	}
	if snap.AccumulatedNano != 90*NanoUSD {
		t.Fatalf("AccumulatedNano = %d, want %d", snap.AccumulatedNano, 90*NanoUSD)
	}
	if snap.Age > time.Minute {
		t.Fatalf("age_seconds = %v; a fresh write must not read as a stale snapshot", snap.Age)
	}
	params := Params{Mode: ModeTieredHybrid, PGFreshness: 5 * time.Minute, ExpectedReplicas: 1}
	if dec := Decide(false, 0, 0, snap, 20*NanoUSD, params); dec.Verdict != Block402 {
		t.Fatalf("degraded verdict = %v (state %v), want Block402 from the durable tier", dec.Verdict, dec.State)
	}

	// After the sweep, the same read answers from the same row.
	counter := newFakeCounter()
	counter.totals[counter.BudgetSubject(ScopeProject, f.scopeID, f.period)] = 90 * NanoUSD
	f.reconciler(counter).SweepOnce(ctx)

	snap, err = f.store.ReadSnapshot(ctx, f.projectID, ScopeProject, f.scopeID, f.period)
	if err != nil {
		t.Fatalf("read snapshot after recovery: %v", err)
	}
	if !snap.Found || snap.AccumulatedNano != 90*NanoUSD {
		t.Fatalf("post-recovery snapshot = %+v, want the same durable figure", snap)
	}
	if dec := Decide(false, 0, 0, snap, 20*NanoUSD, params); dec.Verdict != Block402 {
		t.Fatalf("post-recovery degraded verdict = %v, want Block402", dec.Verdict)
	}
}

// ─── helpers ─────────────────────────────────────────────────────────────────

// countOutageRows runs the production gauge query.
func countOutageRows(t *testing.T, pool *pgxpool.Pool) int64 {
	t.Helper()
	var n int64
	if err := pool.QueryRow(t.Context(), countOutageRowsSQL).Scan(&n); err != nil {
		t.Fatalf("countOutageRowsSQL: %v", err)
	}
	return n
}

// claimEvent runs the write-back consumer's dedup claim and reports whether the
// id was newly inserted.
func claimEvent(t *testing.T, pool *pgxpool.Pool, eventID string) bool {
	t.Helper()
	var got string
	err := pool.QueryRow(t.Context(),
		`INSERT INTO gateway.processed_event_ids (event_id) VALUES ($1)
		 ON CONFLICT DO NOTHING RETURNING event_id`, eventID).Scan(&got)
	if err != nil {
		return false
	}
	return got != ""
}

// newUUID returns a fresh event id. gateway.processed_event_ids.event_id and
// gateway.llm_usage_events.event_id are both UUID columns, so the id the
// gateway generates (uuid.NewString) is the shape that has to be written here.
func newUUID(t *testing.T) string {
	t.Helper()
	return uuid.NewString()
}
