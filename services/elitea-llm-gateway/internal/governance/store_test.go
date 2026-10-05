package governance

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sony/gobreaker/v2"

	"github.com/EliteaAI/elitea-platform/services/elitea-llm-gateway/internal/failmode"
	"github.com/EliteaAI/elitea-platform/services/elitea-llm-gateway/internal/infra/nats"
)

// ─── fake NATS client ────────────────────────────────────────────────────────

type fakeNATS struct {
	mu sync.Mutex

	// totals maps subject → current running counter.
	totals  map[string]int64
	applied map[string]bool

	readErr  error
	incrErr  error
	pubErr   error
	alertErr error

	// incrFailuresLeft makes the next N IncrBudgetIdempotent calls fail with
	// ErrUnavailable and then heal. It models the single slow counter operation
	// of issue #515, which does not reach the breaker's failure threshold.
	incrFailuresLeft int

	// deltas published via PublishDelta.
	deltas  [][]byte
	readHit int

	// state change callback registered via OnBreakerStateChange.
	stateChangeFn func(from, to gobreaker.State)

	// breakerState controls what BreakerState() returns.
	breakerState gobreaker.State
}

func newFakeNATS() *fakeNATS {
	return &fakeNATS{
		totals:  map[string]int64{},
		applied: map[string]bool{},
	}
}

func (f *fakeNATS) ReadBudget(_ context.Context, subject string) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.readHit++
	if f.readErr != nil {
		return 0, f.readErr
	}
	return f.totals[subject], nil
}

func (f *fakeNATS) IncrBudget(_ context.Context, subject string, delta int64) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.incrErr != nil {
		return 0, f.incrErr
	}
	f.totals[subject] += delta
	return f.totals[subject], nil
}

func (f *fakeNATS) IncrBudgetIdempotent(_ context.Context, subject, eventID string, delta int64) (int64, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.incrErr != nil {
		return 0, false, f.incrErr
	}
	if f.incrFailuresLeft > 0 {
		f.incrFailuresLeft--
		return 0, false, nats.ErrUnavailable
	}
	if f.applied[eventID] {
		return f.totals[subject], false, nil
	}
	f.applied[eventID] = true
	f.totals[subject] += delta
	return f.totals[subject], true, nil
}

func (f *fakeNATS) PublishDelta(_ context.Context, _ string, payload []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.pubErr != nil {
		return f.pubErr
	}
	cp := make([]byte, len(payload))
	copy(cp, payload)
	f.deltas = append(f.deltas, cp)
	return nil
}

func (f *fakeNATS) TryAlertCooldown(_ context.Context, _ string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.alertErr != nil {
		return false, f.alertErr
	}
	return true, nil
}

func (f *fakeNATS) OnBreakerStateChange(fn func(from, to gobreaker.State)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.stateChangeFn = fn
}

func (f *fakeNATS) BreakerState() gobreaker.State {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.breakerState
}

// fireStateChange simulates a breaker transition.
func (f *fakeNATS) fireStateChange(from, to gobreaker.State) {
	f.mu.Lock()
	fn := f.stateChangeFn
	f.mu.Unlock()
	if fn != nil {
		fn(from, to)
	}
}

// ─── fake failmode.DB for in-memory snapshot reads ───────────────────────────

type fakeDB struct {
	row         failmode.Snapshot
	rowErr      error
	beginErr    error
	beginCalls  int // counts calls to Begin (proxy for PersistOutageDelta invocations)
	commitCalls int // counts successful Commits — asserts the txn was actually committed
	mu          sync.Mutex
}

func (d *fakeDB) QueryRow(_ context.Context, _ string, _ ...any) failmode.Row {
	if d.rowErr != nil {
		return scriptedRow{scanErr: d.rowErr}
	}
	// Encode the snapshot as the eight columns ReadSnapshot scans:
	// is_unlimited, hard_limit_nano, accumulated_nano, soft_alert_pct,
	// nats_fail_mode (*string), acc_found, age_seconds, soft_alerts_disabled
	//
	// nats_fail_mode must be a nil interface (not a nil *string wrapped in
	// interface{}) so that assignVal's nil check fires correctly for a NULL column.
	var natsFM any // nil interface ⇒ SQL NULL
	if d.row.NatsFailMode != "" {
		s := string(d.row.NatsFailMode)
		natsFM = s
	}
	ageSeconds := d.row.Age.Seconds()
	return scriptedRow{vals: []any{
		d.row.IsUnlimited,
		d.row.HardLimitNano,
		d.row.AccumulatedNano,
		d.row.SoftAlertPct,
		natsFM,
		d.row.Found,
		ageSeconds,
		d.row.SoftAlertsDisabled,
	}}
}

func (d *fakeDB) Begin(_ context.Context) (failmode.Tx, error) {
	d.mu.Lock()
	d.beginCalls++
	d.mu.Unlock()
	if d.beginErr != nil {
		return nil, d.beginErr
	}
	return &trackingNopTx{db: d}, nil
}

// scriptedRow is shared with the failmode package pattern; we redefine a local
// copy here so the governance test is self-contained (the failmode.scriptedRow
// is unexported).
type scriptedRow struct {
	vals    []any
	scanErr error
}

func (r scriptedRow) Scan(dest ...any) error {
	if r.scanErr != nil {
		return r.scanErr
	}
	if len(dest) != len(r.vals) {
		return errors.New("scriptedRow: arity mismatch")
	}
	for i, v := range r.vals {
		if err := assignVal(dest[i], v); err != nil {
			return err
		}
	}
	return nil
}

func assignVal(dest, v any) error {
	switch p := dest.(type) {
	case *bool:
		*p = v.(bool)
	case *int64:
		*p = v.(int64)
	case *int:
		*p = v.(int)
	case *float64:
		*p = v.(float64)
	case **string:
		// v may be an untyped nil (SQL NULL) or a string value. A typed-nil
		// *string wrapped in interface{} also compares == nil here because we
		// set natsFM as `any` (untyped nil) in the test helper.
		if v == nil {
			*p = nil
		} else {
			s := v.(string)
			*p = &s
		}
	case *string:
		*p = v.(string)
	default:
		return errors.New("scriptedRow: unsupported dest type")
	}
	return nil
}

// trackingNopTx is like nopTx but records each successful Commit in fakeDB so
// tests can assert the transaction was actually committed (not just opened).
type trackingNopTx struct {
	db *fakeDB
}

func (t *trackingNopTx) QueryRow(_ context.Context, sql string, args ...any) failmode.Row {
	// The outage-window write claims its event id first (issue #515). Model the
	// claim as always succeeding: this fake has no earlier write-back consumer.
	if strings.Contains(sql, "processed_event_ids") {
		id, _ := args[0].(string)
		return scriptedRow{vals: []any{id}}
	}
	return scriptedRow{scanErr: errors.New("nop")}
}
func (t *trackingNopTx) Query(_ context.Context, _ string, _ ...any) (failmode.Rows, error) {
	return nil, errors.New("nop")
}
func (t *trackingNopTx) ExecAffected(_ context.Context, _ string, _ ...any) (int64, error) {
	return 1, nil
}
func (t *trackingNopTx) Commit(_ context.Context) error {
	t.db.mu.Lock()
	t.db.commitCalls++
	t.db.mu.Unlock()
	return nil
}
func (t *trackingNopTx) Rollback(_ context.Context) error { return nil }

// ─── test helpers ─────────────────────────────────────────────────────────────

const (
	testProject   = 7
	testScope     = "project"
	testScopeID   = "42"
	testPeriod    = int64(1_000_000)
	testPeriodEnd = int64(1_086_400) // +1 day
)

// limitNano is 100 USD in nano-USD.
const limitNano = int64(100) * failmode.NanoUSD

func makeSubject() string {
	return nats.BudgetSubject(testScope, testScopeID, testPeriod)
}

// newStore builds a fully-wired GovernanceStore for testing over fakes.
func newStore(nc *fakeNATS, db *fakeDB) *GovernanceStore {
	fmStore := failmode.NewStore(db)
	degraded := failmode.NewDegradedCounters()
	rec := failmode.NewReconciler(db, nc2counter(nc), degraded, nil)
	params := failmode.Params{
		Mode:             failmode.ModeTieredHybrid,
		PGFreshness:      5 * time.Minute,
		ExpectedReplicas: 1,
	}
	gs := NewGovernanceStore(nc, fmStore, degraded, rec, params, nil)
	gs.Start(context.Background())
	return gs
}

// nc2counter adapts *fakeNATS to the failmode.Counter interface for the Reconciler.
type nc2counterAdapter struct{ nc *fakeNATS }

func nc2counter(nc *fakeNATS) failmode.Counter { return &nc2counterAdapter{nc: nc} }

func (a *nc2counterAdapter) ReadBudget(ctx context.Context, subject string) (int64, error) {
	return a.nc.ReadBudget(ctx, subject)
}
func (a *nc2counterAdapter) IncrBudgetIdempotent(ctx context.Context, subject, eventID string, delta int64) (int64, bool, error) {
	return a.nc.IncrBudgetIdempotent(ctx, subject, eventID, delta)
}
func (a *nc2counterAdapter) BudgetSubject(scope, scopeID string, periodStartUnix int64) string {
	return nats.BudgetSubject(scope, scopeID, periodStartUnix)
}

// ─── tests ────────────────────────────────────────────────────────────────────

// TestCheckBudget_UnderBudgetAllow: NATS up, counter well below limit → Allow.
func TestCheckBudget_UnderBudgetAllow(t *testing.T) {
	nc := newFakeNATS()
	nc.totals[makeSubject()] = 10 * failmode.NanoUSD // 10 of 100 USD spent

	db := &fakeDB{row: failmode.Snapshot{
		HardLimitNano:   limitNano,
		AccumulatedNano: 10 * failmode.NanoUSD,
		SoftAlertPct:    80,
		Found:           true,
		Age:             0,
	}}
	gs := newStore(nc, db)

	dec, err := gs.CheckBudget(context.Background(), testProject, testScope, testScopeID, testPeriod, failmode.NanoUSD)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if dec.Verdict != failmode.Allow {
		t.Fatalf("want Allow, got %v (state=%v)", dec.Verdict, dec.State)
	}
	if dec.State != failmode.StateNATSHealthy {
		t.Fatalf("want NATS_HEALTHY, got %v", dec.State)
	}
	if dec.Degraded {
		t.Fatal("should not be degraded when NATS is up")
	}
	// The audio routes refuse an unpriced model only for a budgeted project.
	if !dec.Budgeted {
		t.Fatal("a project with a hard limit must report Budgeted")
	}
}

// TestCheckBudget_OverBudgetBlock402: NATS up, counter ≥ limit → Block402.
func TestCheckBudget_OverBudgetBlock402(t *testing.T) {
	nc := newFakeNATS()
	nc.totals[makeSubject()] = limitNano // exactly at limit

	db := &fakeDB{row: failmode.Snapshot{
		HardLimitNano:   limitNano,
		AccumulatedNano: limitNano,
		Found:           true,
	}}
	gs := newStore(nc, db)

	dec, err := gs.CheckBudget(context.Background(), testProject, testScope, testScopeID, testPeriod, 0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if dec.Verdict != failmode.Block402 {
		t.Fatalf("want Block402, got %v (state=%v)", dec.Verdict, dec.State)
	}
	if dec.State != failmode.StateNATSHealthy {
		t.Fatalf("want NATS_HEALTHY, got %v", dec.State)
	}
}

// TestCheckBudget_NATSDownStale_Block503: NATS down + stale snapshot → Block503.
func TestCheckBudget_NATSDownStale_Block503(t *testing.T) {
	nc := newFakeNATS()
	nc.readErr = nats.ErrUnavailable // breaker open

	// Snapshot is stale (age > freshness).
	db := &fakeDB{row: failmode.Snapshot{
		HardLimitNano:   limitNano,
		AccumulatedNano: 5 * failmode.NanoUSD,
		Found:           true,
		Age:             10 * time.Minute, // > 5m freshness
	}}
	gs := newStore(nc, db)

	dec, err := gs.CheckBudget(context.Background(), testProject, testScope, testScopeID, testPeriod, 0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if dec.Verdict != failmode.Block503 {
		t.Fatalf("want Block503, got %v (state=%v)", dec.Verdict, dec.State)
	}
	if !dec.Degraded {
		t.Fatal("want Degraded=true on NATS-down path")
	}
}

// TestCheckBudget_NATSDownFresh_Allow: NATS down + fresh snapshot + under limit → Allow.
func TestCheckBudget_NATSDownFresh_Allow(t *testing.T) {
	nc := newFakeNATS()
	nc.readErr = nats.ErrUnavailable

	db := &fakeDB{row: failmode.Snapshot{
		HardLimitNano:   limitNano,
		AccumulatedNano: 10 * failmode.NanoUSD, // 10% spent, soft=80%
		SoftAlertPct:    80,
		Found:           true,
		Age:             1 * time.Minute, // fresh
	}}
	gs := newStore(nc, db)

	dec, err := gs.CheckBudget(context.Background(), testProject, testScope, testScopeID, testPeriod, failmode.NanoUSD)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if dec.Verdict != failmode.Allow {
		t.Fatalf("want Allow, got %v (state=%v)", dec.Verdict, dec.State)
	}
	if !dec.Degraded {
		t.Fatal("want Degraded=true on NATS-down path")
	}
}

// TestCheckBudget_NoBudgetRow_Unlimited: no project_budget row → unlimited → Allow.
func TestCheckBudget_NoBudgetRow_Unlimited(t *testing.T) {
	nc := newFakeNATS()
	nc.totals[makeSubject()] = 999 * failmode.NanoUSD // any counter

	db := &fakeDB{rowErr: failmode.ErrNoBudgetRow}
	gs := newStore(nc, db)

	// NATS up with non-zero counter; unlimited means no block.
	dec, err := gs.CheckBudget(context.Background(), testProject, testScope, testScopeID, testPeriod, 0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if dec.Verdict != failmode.Allow {
		t.Fatalf("unlimited project must be Allow, got %v", dec.Verdict)
	}
	if dec.Budgeted {
		t.Fatal("an unlimited project must not report Budgeted")
	}
}

// TestCheckBudget_NATSAndPGBothDown_Block503: NATS unavailable + PG error → Block503.
func TestCheckBudget_NATSAndPGBothDown_Block503(t *testing.T) {
	nc := newFakeNATS()
	nc.readErr = nats.ErrUnavailable

	db := &fakeDB{rowErr: errors.New("postgres down")}
	gs := newStore(nc, db)

	dec, err := gs.CheckBudget(context.Background(), testProject, testScope, testScopeID, testPeriod, 0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if dec.Verdict != failmode.Block503 {
		t.Fatalf("want Block503, got %v", dec.Verdict)
	}
}

// TestUpdateUsage_IncrementsAndPublishes: UpdateUsage calls IncrBudgetIdempotent
// and PublishDelta with the correct data.
func TestUpdateUsage_IncrementsAndPublishes(t *testing.T) {
	nc := newFakeNATS()
	db := &fakeDB{rowErr: failmode.ErrNoBudgetRow}
	gs := newStore(nc, db)

	const costNano = int64(2) * failmode.NanoUSD
	const eventID = "evt-001"
	err := gs.UpdateUsage(context.Background(), testProject, testScope, testScopeID, eventID, costNano, testPeriod, testPeriodEnd, nil)
	if err != nil {
		t.Fatalf("UpdateUsage: %v", err)
	}

	subject := makeSubject()
	nc.mu.Lock()
	total := nc.totals[subject]
	deltaCount := len(nc.deltas)
	nc.mu.Unlock()

	if total != costNano {
		t.Fatalf("counter total = %d, want %d", total, costNano)
	}
	if deltaCount != 1 {
		t.Fatalf("expected 1 delta published, got %d", deltaCount)
	}
}

// TestUpdateUsage_IdempotentOnRetry: a second call with the same eventID does
// not double-increment the counter.
func TestUpdateUsage_IdempotentOnRetry(t *testing.T) {
	nc := newFakeNATS()
	db := &fakeDB{rowErr: failmode.ErrNoBudgetRow}
	gs := newStore(nc, db)

	const costNano = int64(5) * failmode.NanoUSD
	const eventID = "evt-idem"

	// First call.
	if err := gs.UpdateUsage(context.Background(), testProject, testScope, testScopeID, eventID, costNano, testPeriod, testPeriodEnd, nil); err != nil {
		t.Fatalf("first UpdateUsage: %v", err)
	}
	// Second call with same eventID (retry simulation).
	if err := gs.UpdateUsage(context.Background(), testProject, testScope, testScopeID, eventID, costNano, testPeriod, testPeriodEnd, nil); err != nil {
		t.Fatalf("second UpdateUsage: %v", err)
	}

	subject := makeSubject()
	nc.mu.Lock()
	total := nc.totals[subject]
	nc.mu.Unlock()

	if total != costNano {
		t.Fatalf("idempotent: counter = %d, want %d (must not double-count)", total, costNano)
	}
}

// TestUpdateUsage_NATSDown_DegradedCounterUpdated: when NATS is unavailable,
// UpdateUsage must update the degraded counter so CheckBudget's FSM can gate it.
func TestUpdateUsage_NATSDown_DegradedCounterUpdated(t *testing.T) {
	nc := newFakeNATS()
	nc.incrErr = nats.ErrUnavailable
	// Also fail delta publish (NATS down) — should not error the caller.
	nc.pubErr = nats.ErrUnavailable

	db := &fakeDB{rowErr: failmode.ErrNoBudgetRow}
	gs := newStore(nc, db)

	const costNano = int64(3) * failmode.NanoUSD
	if err := gs.UpdateUsage(context.Background(), testProject, testScope, testScopeID, "evt-down", costNano, testPeriod, testPeriodEnd, nil); err != nil {
		t.Fatalf("UpdateUsage must not error on NATS-down: %v", err)
	}

	subject := makeSubject()
	got := gs.DumpTotal(testScope, testScopeID, testPeriod)
	if got != costNano {
		t.Fatalf("degraded counter = %d, want %d", got, costNano)
	}
	_ = subject
}

// TestDumpTotal_ZeroWhenNeverUpdated: DumpTotal returns 0 for an unknown scope.
func TestDumpTotal_ZeroWhenNeverUpdated(t *testing.T) {
	nc := newFakeNATS()
	db := &fakeDB{rowErr: failmode.ErrNoBudgetRow}
	gs := newStore(nc, db)
	if got := gs.DumpTotal("project", "9999", 0); got != 0 {
		t.Fatalf("expected 0, got %d", got)
	}
}

// TestResetExpired_NoOp: ResetExpired must return nil (no-op by design).
func TestResetExpired_NoOp(t *testing.T) {
	nc := newFakeNATS()
	db := &fakeDB{rowErr: failmode.ErrNoBudgetRow}
	gs := newStore(nc, db)
	if err := gs.ResetExpired(context.Background()); err != nil {
		t.Fatalf("ResetExpired returned error: %v", err)
	}
}

// TestNATSUnavailable_ReflectsState: NATSUnavailable returns false when
// BreakerState is Closed and true otherwise.
func TestNATSUnavailable_ReflectsState(t *testing.T) {
	nc := newFakeNATS()
	db := &fakeDB{rowErr: failmode.ErrNoBudgetRow}
	gs := newStore(nc, db)

	// Default state: closed.
	if gs.NATSUnavailable() {
		t.Fatal("expected NATS available (closed breaker)")
	}

	nc.mu.Lock()
	nc.breakerState = gobreaker.StateOpen
	nc.mu.Unlock()

	if !gs.NATSUnavailable() {
		t.Fatal("expected NATS unavailable (open breaker)")
	}
}

// TestTryAlertCooldown_Forwarded: TryAlertCooldown must call through to NATS.
func TestTryAlertCooldown_Forwarded(t *testing.T) {
	nc := newFakeNATS()
	db := &fakeDB{rowErr: failmode.ErrNoBudgetRow}
	gs := newStore(nc, db)

	ok, err := gs.TryAlertCooldown(context.Background(), testScope, testScopeID, testPeriod)
	if err != nil {
		t.Fatalf("TryAlertCooldown: %v", err)
	}
	if !ok {
		t.Fatal("expected alert to fire (first call)")
	}
}

// TestStart_BindsReconcilerContext: after Start, a breaker→closed transition
// fires the recovery reconciler and it runs with the bound context. We observe
// this by watching fakeDB.beginCalls: the reconciler's lockOutageRows always
// calls Begin once (even when no outage rows exist), so beginCalls > 0 proves
// the reconciler executed. The old test merely slept and checked for no panic.
func TestStart_BindsReconcilerContext(t *testing.T) {
	nc := newFakeNATS()
	db := &fakeDB{rowErr: failmode.ErrNoBudgetRow}
	gs := newStore(nc, db) // Start is called inside newStore

	// Simulate a breaker recovery edge (open → closed). The reconciler has no
	// outage rows so runPass completes trivially, but it must call db.Begin once
	// as part of lockOutageRows.
	nc.fireStateChange(gobreaker.StateOpen, gobreaker.StateClosed)

	// Poll for db.beginCalls > 0, confirming the reconciler actually ran.
	// Replace the bare sleep with deterministic polling (generous 2s timeout).
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		db.mu.Lock()
		began := db.beginCalls
		db.mu.Unlock()
		if began > 0 {
			break
		}
		time.Sleep(2 * time.Millisecond)
	}

	db.mu.Lock()
	began := db.beginCalls
	db.mu.Unlock()
	if began == 0 {
		t.Fatal("recovery reconciler did not run after breaker→closed (Begin never called); Start may not have bound the context")
	}
	_ = gs
}

// TestPing_Healthy: Ping returns nil when the breaker is closed.
func TestPing_Healthy(t *testing.T) {
	nc := newFakeNATS()
	db := &fakeDB{rowErr: failmode.ErrNoBudgetRow}
	gs := newStore(nc, db)

	if err := gs.Ping(context.Background()); err != nil {
		t.Fatalf("Ping should return nil when NATS is healthy, got: %v", err)
	}
}

// TestPing_Unhealthy: Ping returns ErrUnavailable when breaker is open.
func TestPing_Unhealthy(t *testing.T) {
	nc := newFakeNATS()
	nc.breakerState = gobreaker.StateOpen
	db := &fakeDB{rowErr: failmode.ErrNoBudgetRow}
	gs := newStore(nc, db)

	if err := gs.Ping(context.Background()); err == nil {
		t.Fatal("Ping should return error when NATS breaker is open")
	}
}

// TestPing_HalfOpen: StateHalfOpen must also be treated as unhealthy — a
// breaker mistakenly reported healthy during its probe window would let
// NATSUnavailable() callers make routing decisions on stale state.
func TestPing_HalfOpen(t *testing.T) {
	nc := newFakeNATS()
	nc.breakerState = gobreaker.StateHalfOpen
	db := &fakeDB{rowErr: failmode.ErrNoBudgetRow}
	gs := newStore(nc, db)

	err := gs.Ping(context.Background())
	if err == nil {
		t.Fatal("Ping should return error when NATS breaker is half-open")
	}
	if !errors.Is(err, nats.ErrUnavailable) {
		t.Fatalf("err = %v, want errors.Is(err, nats.ErrUnavailable)", err)
	}
}

// TestDefaultParams_Sane: DefaultParams must return a non-zero Params.
func TestDefaultParams_Sane(t *testing.T) {
	p := DefaultParams()
	if p.Mode != failmode.ModeTieredHybrid {
		t.Fatalf("mode = %v", p.Mode)
	}
	if p.PGFreshness <= 0 {
		t.Fatalf("freshness = %v", p.PGFreshness)
	}
	if p.ExpectedReplicas < 1 {
		t.Fatalf("replicas = %d", p.ExpectedReplicas)
	}
}

// TestUpdateUsage_DeltaPayloadRoundTrip: FIX 1 — marshals a deltaPayload and
// unmarshals it into a struct with the scheduler consumer's exact JSON keys,
// asserting all money/period/identity fields survive round-trip intact.
func TestUpdateUsage_DeltaPayloadRoundTrip(t *testing.T) {
	nc := newFakeNATS()
	db := &fakeDB{rowErr: failmode.ErrNoBudgetRow}
	gs := newStore(nc, db)

	const costNano = int64(7) * failmode.NanoUSD
	const eventID = "evt-payload-roundtrip"
	if err := gs.UpdateUsage(context.Background(), testProject, testScope, testScopeID, eventID, costNano, testPeriod, testPeriodEnd, nil); err != nil {
		t.Fatalf("UpdateUsage: %v", err)
	}

	nc.mu.Lock()
	if len(nc.deltas) == 0 {
		nc.mu.Unlock()
		t.Fatal("no delta published")
	}
	raw := nc.deltas[0]
	nc.mu.Unlock()

	// Consumer's expected struct (scheduler BudgetDelta JSON tags).
	var got struct {
		EventID      string `json:"event_id"`
		Scope        string `json:"scope"`
		ScopeID      string `json:"scope_id"`
		ProjectID    int    `json:"project_id"`
		OrgID        *int   `json:"org_id,omitempty"`
		PeriodStart  int64  `json:"period_start"`
		PeriodEnd    int64  `json:"period_end"`
		DeltaNanoUSD int64  `json:"delta_nano_usd"`
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.EventID != eventID {
		t.Errorf("event_id = %q, want %q", got.EventID, eventID)
	}
	if got.Scope != testScope {
		t.Errorf("scope = %q, want %q", got.Scope, testScope)
	}
	if got.ScopeID != testScopeID {
		t.Errorf("scope_id = %q, want %q", got.ScopeID, testScopeID)
	}
	if got.ProjectID != testProject {
		t.Errorf("project_id = %d, want %d", got.ProjectID, testProject)
	}
	if got.DeltaNanoUSD != costNano {
		t.Errorf("delta_nano_usd = %d, want %d", got.DeltaNanoUSD, costNano)
	}
	if got.PeriodStart != testPeriod {
		t.Errorf("period_start = %d, want %d", got.PeriodStart, testPeriod)
	}
	if got.PeriodEnd != testPeriodEnd {
		t.Errorf("period_end = %d, want %d", got.PeriodEnd, testPeriodEnd)
	}
}

// TestUpdateUsage_NATSDown_PersistOutageDelta_Called: FIX 2 — on the NATS-down
// path, PersistOutageDelta must be invoked (Begin called on the DB) so spend
// is durably written even before the breaker recovers.
func TestUpdateUsage_NATSDown_PersistOutageDelta_Called(t *testing.T) {
	nc := newFakeNATS()
	nc.incrErr = nats.ErrUnavailable
	nc.pubErr = nats.ErrUnavailable

	db := &fakeDB{rowErr: failmode.ErrNoBudgetRow}
	gs := newStore(nc, db)

	const costNano = int64(4) * failmode.NanoUSD
	if err := gs.UpdateUsage(context.Background(), testProject, testScope, testScopeID, "evt-outage", costNano, testPeriod, testPeriodEnd, nil); err != nil {
		t.Fatalf("UpdateUsage must not error: %v", err)
	}

	// PersistOutageDelta runs in a goroutine; poll until the commit completes
	// (not just Begin) so we assert the txn was actually COMMITTED, not just
	// started. A Begin-then-rollback bug would increment beginCalls but not
	// commitCalls and would be missed by the old assertion.
	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		db.mu.Lock()
		commits := db.commitCalls
		db.mu.Unlock()
		if commits > 0 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}

	db.mu.Lock()
	begins := db.beginCalls
	commits := db.commitCalls
	db.mu.Unlock()
	if begins == 0 {
		t.Fatal("PersistOutageDelta not called on NATS-down path (Begin never invoked)")
	}
	if commits == 0 {
		t.Fatal("PersistOutageDelta opened a transaction but never committed it (Begin without Commit)")
	}
}

// TestUpdateUsage_NATSHealthy_PersistOutageDelta_NotCalled: FIX 2 — on the
// healthy NATS path, PersistOutageDelta must NOT be called.
func TestUpdateUsage_NATSHealthy_PersistOutageDelta_NotCalled(t *testing.T) {
	nc := newFakeNATS() // NATS healthy: no incrErr
	db := &fakeDB{rowErr: failmode.ErrNoBudgetRow}
	gs := newStore(nc, db)

	const costNano = int64(2) * failmode.NanoUSD
	if err := gs.UpdateUsage(context.Background(), testProject, testScope, testScopeID, "evt-healthy", costNano, testPeriod, testPeriodEnd, nil); err != nil {
		t.Fatalf("UpdateUsage: %v", err)
	}

	// On the healthy NATS path no goroutine is spawned, so Drain() returns
	// immediately and the assertion is deterministic — no time.Sleep needed.
	gs.Drain()

	db.mu.Lock()
	calls := db.beginCalls
	db.mu.Unlock()
	if calls != 0 {
		t.Fatalf("PersistOutageDelta called on healthy path (Begin invoked %d times)", calls)
	}
}

// TestCheckBudget_PerProjectFailModeOverride: FIX 3 — a per-project fail_closed
// override must flip the decision to Block402 even when the platform baseline is
// tiered_hybrid and the snapshot is well within the budget.
func TestCheckBudget_PerProjectFailModeOverride(t *testing.T) {
	nc := newFakeNATS()
	nc.readErr = nats.ErrUnavailable // NATS down so fail-mode is consulted

	db := &fakeDB{row: failmode.Snapshot{
		HardLimitNano:   limitNano,
		AccumulatedNano: 10 * failmode.NanoUSD, // 10% spent — well under limit
		SoftAlertPct:    80,
		Found:           true,
		Age:             1 * time.Minute,         // fresh
		NatsFailMode:    failmode.ModeFailClosed, // per-project override
	}}
	gs := newStore(nc, db) // baseline is tiered_hybrid

	dec, err := gs.CheckBudget(context.Background(), testProject, testScope, testScopeID, testPeriod, failmode.NanoUSD)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// tiered_hybrid baseline + fresh snapshot would Allow; fail_closed override must Block402.
	if dec.Verdict != failmode.Block402 {
		t.Fatalf("per-project fail_closed override: want Block402, got %v (state=%v)", dec.Verdict, dec.State)
	}
	if !dec.Degraded {
		t.Fatal("want Degraded=true on NATS-down path")
	}
}

// TestCheckBudget_OutageExceededMax_ForcedClosed: FIX #8 — when the NATS
// breaker has been open longer than params.DegradedMaxDuration, CheckBudget
// must set OutageExceededMax=true so the FSM returns FORCED_CLOSED (Block503).
func TestCheckBudget_OutageExceededMax_ForcedClosed(t *testing.T) {
	nc := newFakeNATS()
	nc.readErr = nats.ErrUnavailable // NATS down

	// Fresh snapshot well under the limit — without the max-duration check the
	// FSM would allow (tiered_hybrid + fresh + under soft threshold).
	db := &fakeDB{row: failmode.Snapshot{
		HardLimitNano:   limitNano,
		AccumulatedNano: 10 * failmode.NanoUSD,
		SoftAlertPct:    80,
		Found:           true,
		Age:             1 * time.Minute,
	}}

	// Build store with a very short DegradedMaxDuration so the test does not
	// have to sleep.
	fmStore := failmode.NewStore(db)
	degraded := failmode.NewDegradedCounters()
	rec := failmode.NewReconciler(db, nc2counter(nc), degraded, nil)
	params := failmode.Params{
		Mode:                failmode.ModeTieredHybrid,
		PGFreshness:         5 * time.Minute,
		ExpectedReplicas:    1,
		DegradedMaxDuration: 10 * time.Millisecond, // very short for testing
	}
	gs := NewGovernanceStore(nc, fmStore, degraded, rec, params, nil)
	gs.Start(context.Background())

	// Simulate the breaker going open.
	nc.fireStateChange(gobreaker.StateClosed, gobreaker.StateOpen)

	// Sleep a little longer than DegradedMaxDuration so the clock advances.
	time.Sleep(50 * time.Millisecond)

	dec, err := gs.CheckBudget(context.Background(), testProject, testScope, testScopeID, testPeriod, 0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// The outage has exceeded max duration → must be FORCED_CLOSED (Block503).
	if dec.Verdict != failmode.Block503 {
		t.Fatalf("FIX #8: want Block503 (FORCED_CLOSED), got %v (state=%v)", dec.Verdict, dec.State)
	}
	if dec.State != failmode.StateForcedClosed {
		t.Fatalf("FIX #8: want StateForcedClosed, got %v", dec.State)
	}
	if !dec.Degraded {
		t.Fatal("FIX #8: want Degraded=true")
	}
}

// TestCheckBudget_NATSHealthyPGFails_FailClosed: Fix #1 — when NATS is healthy
// but the Postgres snapshot read fails (non-ErrNoBudgetRow transient error),
// CheckBudget must return a Block/error, NOT Allow. A zero Snapshot{} would
// give HardLimitNano=0 which disables enforcement silently.
func TestCheckBudget_NATSHealthyPGFails_FailClosed(t *testing.T) {
	nc := newFakeNATS()
	nc.totals[makeSubject()] = 10 * failmode.NanoUSD // NATS healthy and has a counter

	// Postgres returns a transient non-ErrNoBudgetRow error.
	db := &fakeDB{rowErr: errors.New("postgres connection timeout")}
	gs := newStore(nc, db)

	dec, err := gs.CheckBudget(context.Background(), testProject, testScope, testScopeID, testPeriod, failmode.NanoUSD)
	// Fix #1: must return an error and a blocking verdict, not Allow.
	if err == nil {
		t.Fatal("Fix #1: CheckBudget must return an error when NATS is healthy but PG read fails")
	}
	if dec.Verdict == failmode.Allow {
		t.Fatalf("Fix #1: want Block verdict when PG fails (fail-closed), got Allow (state=%v)", dec.State)
	}
}

// TestDrain_BlocksUntilInFlightPersistsComplete: Fix #2 — Drain() must block
// until all in-flight PersistOutageDelta goroutines have finished so that the
// server can call it before closing the database pool on graceful shutdown.
func TestDrain_BlocksUntilInFlightPersistsComplete(t *testing.T) {
	nc := newFakeNATS()
	nc.incrErr = nats.ErrUnavailable
	nc.pubErr = nats.ErrUnavailable

	// Use a blocking DB: the Begin() call waits until unblocked.
	blockCh := make(chan struct{})
	doneCh := make(chan struct{})
	var beginOnce sync.Once

	blockingDB := &blockingFakeDB{
		fakeDB: fakeDB{rowErr: failmode.ErrNoBudgetRow},
		onBegin: func() {
			beginOnce.Do(func() { close(doneCh) }) // signal that goroutine started
			<-blockCh                              // block until test unblocks
		},
	}

	fmStore := failmode.NewStore(blockingDB)
	degraded := failmode.NewDegradedCounters()
	rec := failmode.NewReconciler(blockingDB, nc2counter(nc), degraded, nil)
	params := failmode.Params{
		Mode:             failmode.ModeTieredHybrid,
		PGFreshness:      5 * time.Minute,
		ExpectedReplicas: 1,
	}
	gs := NewGovernanceStore(nc, fmStore, degraded, rec, params, nil)
	gs.Start(context.Background())

	const costNano = int64(3) * failmode.NanoUSD
	if err := gs.UpdateUsage(context.Background(), testProject, testScope, testScopeID, "evt-drain", costNano, testPeriod, testPeriodEnd, nil); err != nil {
		t.Fatalf("UpdateUsage: %v", err)
	}

	// Wait for the goroutine to start (it is blocking on blockCh).
	select {
	case <-doneCh:
	case <-time.After(2 * time.Second):
		t.Fatal("goroutine did not start within 2s")
	}

	// Drain() must not return while the goroutine is still blocked.
	drainDone := make(chan struct{})
	go func() {
		gs.Drain()
		close(drainDone)
	}()

	select {
	case <-drainDone:
		t.Fatal("Drain() returned before the in-flight persist goroutine completed")
	case <-time.After(50 * time.Millisecond):
		// Good: Drain is still blocked.
	}

	// Unblock the persist goroutine.
	close(blockCh)

	// Now Drain() should complete promptly.
	select {
	case <-drainDone:
		// Pass.
	case <-time.After(2 * time.Second):
		t.Fatal("Drain() did not return after the goroutine finished")
	}
}

// blockingFakeDB wraps fakeDB and calls onBegin() synchronously inside Begin()
// so the test can inject a blocking point into PersistOutageDelta's goroutine.
type blockingFakeDB struct {
	fakeDB
	onBegin func()
}

func (d *blockingFakeDB) Begin(ctx context.Context) (failmode.Tx, error) {
	if d.onBegin != nil {
		d.onBegin()
	}
	return d.fakeDB.Begin(ctx)
}

// TestCheckBudget_OutageExceededMax_ResetsOnClosed: FIX #8 — after breaker→CLOSED
// the outage clock must reset so a fresh outage does not immediately force-close.
func TestCheckBudget_OutageExceededMax_ResetsOnClosed(t *testing.T) {
	nc := newFakeNATS()
	nc.readErr = nats.ErrUnavailable

	db := &fakeDB{row: failmode.Snapshot{
		HardLimitNano:   limitNano,
		AccumulatedNano: 10 * failmode.NanoUSD,
		SoftAlertPct:    80,
		Found:           true,
		Age:             1 * time.Minute,
	}}

	fmStore := failmode.NewStore(db)
	degraded := failmode.NewDegradedCounters()
	rec := failmode.NewReconciler(db, nc2counter(nc), degraded, nil)
	params := failmode.Params{
		Mode:                failmode.ModeTieredHybrid,
		PGFreshness:         5 * time.Minute,
		ExpectedReplicas:    1,
		DegradedMaxDuration: 10 * time.Millisecond,
	}
	gs := NewGovernanceStore(nc, fmStore, degraded, rec, params, nil)
	gs.Start(context.Background())

	// Open the breaker, wait past the duration.
	nc.fireStateChange(gobreaker.StateClosed, gobreaker.StateOpen)
	time.Sleep(50 * time.Millisecond)

	// Now recover the breaker — this must reset the clock.
	nc.fireStateChange(gobreaker.StateOpen, gobreaker.StateClosed)
	// Small sleep to let the callback run.
	time.Sleep(5 * time.Millisecond)

	// NATS is still returning errors (readErr set), but the clock was reset,
	// so if we simulate another open right now (before the duration elapses)
	// the request should be allowed by the tiered-hybrid (fresh snapshot).
	nc.fireStateChange(gobreaker.StateClosed, gobreaker.StateOpen)

	dec, err := gs.CheckBudget(context.Background(), testProject, testScope, testScopeID, testPeriod, 0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// The new outage started just now (< 10ms ago) → must NOT be FORCED_CLOSED.
	if dec.Verdict == failmode.Block503 && dec.State == failmode.StateForcedClosed {
		t.Fatal("FIX #8: clock was not reset on breaker→CLOSED; got premature FORCED_CLOSED")
	}
	// Should be Allow or Block402 based on the fresh snapshot path.
	if dec.Verdict != failmode.Allow {
		t.Logf("FIX #8: got %v state=%v (not FORCED_CLOSED — reset confirmed)", dec.Verdict, dec.State)
	}
}

// TestUpdateUsage_PublishesUsageDimensions covers the branch of usageDimsFor
// that every other test in this file skips: they all pass nil dims, so only the
// "no dimensions" half was exercised.
//
// It is not a coverage errand. This is the ONLY assertion on the gateway side
// of the usage-ledger wire contract (issue #320). The consumer that reads these
// bytes lives in another Go module — services/elitea-scheduler/internal/
// budgetwriteback — so no compiler checks that the two structs agree. The JSON
// KEYS below are transcribed from that package's UsageDimensions. A rename on
// either side silently produces a delta whose usage object decodes to zeros,
// and the per-model table then reports every call as 0 tokens.
func TestUpdateUsage_PublishesUsageDimensions(t *testing.T) {
	nc := newFakeNATS()
	db := &fakeDB{rowErr: failmode.ErrNoBudgetRow}
	gs := newStore(nc, db)

	userID := 7
	dims := &failmode.UsageDimensions{
		UserID:           &userID,
		Provider:         "openai",
		Model:            "gpt-4o",
		PromptTokens:     11,
		CompletionTokens: 22,
		OccurredAtUnix:   1767225600,
	}

	const costNano = int64(3) * failmode.NanoUSD
	if err := gs.UpdateUsage(context.Background(), testProject, testScope, testScopeID,
		"evt-dims", costNano, testPeriod, testPeriodEnd, dims); err != nil {
		t.Fatalf("UpdateUsage: %v", err)
	}

	nc.mu.Lock()
	published := append([][]byte(nil), nc.deltas...)
	nc.mu.Unlock()
	if len(published) != 1 {
		t.Fatalf("published %d deltas, want 1", len(published))
	}

	// Decoded through a struct declared HERE with the scheduler's key names,
	// not through the producer's own type. Decoding with usageDimsPayload would
	// pass whatever that type happened to spell.
	var envelope struct {
		DeltaNanoUSD int64 `json:"delta_nano_usd"`
		Usage        *struct {
			UserID           *int   `json:"user_id"`
			Provider         string `json:"provider"`
			Model            string `json:"model"`
			PromptTokens     int64  `json:"prompt_tokens"`
			CompletionTokens int64  `json:"completion_tokens"`
			OccurredAtUnix   int64  `json:"occurred_at"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(published[0], &envelope); err != nil {
		t.Fatalf("decode delta %s: %v", published[0], err)
	}
	if envelope.Usage == nil {
		t.Fatalf("delta carries no usage object; the ledger row would never be written: %s", published[0])
	}
	if envelope.Usage.UserID == nil || *envelope.Usage.UserID != userID {
		t.Fatalf("usage.user_id = %v, want %d", envelope.Usage.UserID, userID)
	}
	if envelope.Usage.Provider != "openai" || envelope.Usage.Model != "gpt-4o" {
		t.Fatalf("usage provider/model = (%q, %q), want (openai, gpt-4o)",
			envelope.Usage.Provider, envelope.Usage.Model)
	}
	if envelope.Usage.PromptTokens != 11 || envelope.Usage.CompletionTokens != 22 {
		t.Fatalf("usage tokens = (%d, %d), want (11, 22)",
			envelope.Usage.PromptTokens, envelope.Usage.CompletionTokens)
	}
	if envelope.Usage.OccurredAtUnix != 1767225600 {
		t.Fatalf("usage.occurred_at = %d, want the gateway's billing instant 1767225600",
			envelope.Usage.OccurredAtUnix)
	}
	// The money is unchanged by the dimensions riding along.
	if envelope.DeltaNanoUSD != costNano {
		t.Fatalf("delta_nano_usd = %d, want %d", envelope.DeltaNanoUSD, costNano)
	}
}

// TestUpdateUsage_OmitsTheUsageObjectWhenThereAreNoDimensions is the other half
// of usageDimsFor, asserted rather than assumed.
//
// The member-scope delta of a request carries no dimensions, because the
// project-scope delta already recorded them. `omitempty` on a nil pointer is
// what keeps the key ABSENT rather than null, and absent is what the consumer
// tests for before it writes a ledger row. A second row per request would
// double every token and request count the per-model table reports.
func TestUpdateUsage_OmitsTheUsageObjectWhenThereAreNoDimensions(t *testing.T) {
	nc := newFakeNATS()
	db := &fakeDB{rowErr: failmode.ErrNoBudgetRow}
	gs := newStore(nc, db)

	if err := gs.UpdateUsage(context.Background(), testProject, testScope, testScopeID,
		"evt-no-dims", failmode.NanoUSD, testPeriod, testPeriodEnd, nil); err != nil {
		t.Fatalf("UpdateUsage: %v", err)
	}

	nc.mu.Lock()
	published := append([][]byte(nil), nc.deltas...)
	nc.mu.Unlock()

	var raw map[string]json.RawMessage
	if err := json.Unmarshal(published[0], &raw); err != nil {
		t.Fatalf("decode delta: %v", err)
	}
	if _, present := raw["usage"]; present {
		t.Fatalf("delta carries a usage key with no dimensions to report: %s", published[0])
	}
}
