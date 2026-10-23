package governance

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/sony/gobreaker/v2"

	"github.com/EliteaAI/elitea-platform/services/elitea-llm-gateway/internal/failmode"
	"github.com/EliteaAI/elitea-platform/services/elitea-llm-gateway/internal/infra/nats"
)

// outage_wedge_test.go is the end-to-end reproduction of issue #515 and the
// proof that the fix clears it.
//
// The sequence it drives is the one the issue names:
//
//  1. ONE IncrBudgetIdempotent call fails while the breaker is CLOSED. The
//     gateway takes the outage branch and marks the accumulator row
//     outage_mode=true, reconciled=false.
//  2. PublishDelta runs next on the same healthy connection and succeeds.
//  3. Two more requests are billed with NATS fully healthy. Their write-behind
//     deltas reach the consumer, whose guard — NOT (outage_mode AND NOT
//     reconciled) — matches no row, so both are deferred.
//  4. No breaker edge ever occurs, so before this fix nothing cleared the flag
//     and the durable spend stayed at the value it had in step 1.
//
// The fakes below are the two Postgres writers, modelled from the SQL the real
// code sends. The gateway side is the REAL failmode.Store, the REAL
// failmode.Reconciler and the REAL GovernanceStore; only the leaf database and
// the NATS transport are fakes. The write-back consumer lives in another module
// (services/elitea-scheduler), so its Apply is modelled here from
// budgetwriteback/store.go — the same shape disjoint_recovery_test.go already
// models, and the model dispatches on the production statements rather than on
// invented ones.

const (
	wedgeProject     = 42
	wedgeScopeID     = "42"
	wedgePeriodStart = int64(1_000_000)
	wedgePeriodEnd   = int64(1_086_400)
)

// ─── the modelled Postgres ───────────────────────────────────────────────────

// modelRow is one gateway.llm_budget_accumulators row.
type modelRow struct {
	id              string
	scope, scopeID  string
	periodStartUnix int64
	accumulatedNano int64
	outageMode      bool
	reconciled      bool
	inProgress      bool
}

// modelPG holds the two tables both writers touch. Every mutation happens
// inside a transaction that holds the table lock, so the gateway's outage write
// (which runs in its own goroutine) and a recovery pass cannot interleave —
// a conservative stand-in for the row locks the real statements take.
type modelPG struct {
	mu     sync.Mutex
	rows   map[string]*modelRow
	events map[string]bool // gateway.processed_event_ids
	ledger map[string]bool // gateway.llm_usage_events
	nextID int
	// begins counts transactions opened against this model, so a test can
	// assert that a pass attempted nothing at all.
	begins int
	// snap is the gateway.project_budget config the snapshot read returns.
	snap failmode.Snapshot
}

func newModelPG(snap failmode.Snapshot) *modelPG {
	return &modelPG{
		rows:   map[string]*modelRow{},
		events: map[string]bool{},
		ledger: map[string]bool{},
		snap:   snap,
	}
}

func modelKey(scope, scopeID string, periodStartUnix int64) string {
	return scope + "|" + scopeID + "|" + fmt.Sprint(periodStartUnix)
}

// accumulated reports the durable spend for a scope, in nano-USD.
func (m *modelPG) accumulated(scope, scopeID string) int64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	if r := m.rows[modelKey(scope, scopeID, wedgePeriodStart)]; r != nil {
		return r.accumulatedNano
	}
	return 0
}

// outageOwned reports whether the recovery pass still owns the scope's row.
func (m *modelPG) outageOwned(scope, scopeID string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	r := m.rows[modelKey(scope, scopeID, wedgePeriodStart)]
	return r != nil && r.outageMode && !r.reconciled
}

// QueryRow serves the non-transactional reads: the hot-path snapshot and the
// outage-row count behind the gauge.
func (m *modelPG) QueryRow(_ context.Context, sql string, args ...any) failmode.Row {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.readLocked(sql, args)
}

func (m *modelPG) readLocked(sql string, args []any) failmode.Row {
	switch {
	case strings.Contains(sql, "count(*)"):
		var n int64
		for _, r := range m.rows {
			if r.outageMode && !r.reconciled {
				n++
			}
		}
		return scriptedRow{vals: []any{n}}

	case strings.Contains(sql, "processed_event_ids"):
		// The dedup claim, shared by the gateway's outage write and the
		// consumer: ON CONFLICT DO NOTHING RETURNING event_id.
		id, _ := args[0].(string)
		if m.events[id] {
			return scriptedRow{scanErr: pgx.ErrNoRows}
		}
		m.events[id] = true
		return scriptedRow{vals: []any{id}}

	case strings.Contains(sql, "FOR UPDATE"):
		// The recovery re-lock: WHERE id = $1 AND outage_mode AND NOT reconciled.
		id, _ := args[0].(string)
		for _, r := range m.rows {
			if r.id == id && r.outageMode && !r.reconciled {
				return scriptedRow{vals: []any{r.accumulatedNano}}
			}
		}
		return scriptedRow{scanErr: pgx.ErrNoRows}

	case strings.Contains(sql, "gateway.project_budget"):
		// The snapshot read, joined onto whatever the accumulator now holds.
		snap := m.snap
		var accum int64
		var found bool
		if r := m.rows[modelKey(failmode.ScopeProject, wedgeScopeID, wedgePeriodStart)]; r != nil {
			accum, found = r.accumulatedNano, true
		}
		var failMode any
		return scriptedRow{vals: []any{
			snap.IsUnlimited, snap.HardLimitNano, accum, snap.SoftAlertPct,
			failMode, found, 0.0, false,
		}}
	}
	return scriptedRow{scanErr: fmt.Errorf("modelPG: unmodelled read: %s", sql)}
}

func (m *modelPG) Begin(_ context.Context) (failmode.Tx, error) {
	m.mu.Lock()
	m.begins++
	return &modelTx{pg: m}, nil
}

// transactions reports how many transactions have been opened.
func (m *modelPG) transactions() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.begins
}

// modelTx applies each statement immediately and records an undo entry, so a
// Rollback discards everything the transaction staged — which is what makes the
// consumer's deferral (rollback, so the dedup rows are NOT persisted) faithful.
type modelTx struct {
	pg   *modelPG
	undo []func()
	done bool
}

func (t *modelTx) finish() {
	if !t.done {
		t.done = true
		t.pg.mu.Unlock()
	}
}

func (t *modelTx) QueryRow(_ context.Context, sql string, args ...any) failmode.Row {
	row := t.pg.readLocked(sql, args)
	if strings.Contains(sql, "processed_event_ids") {
		if id, ok := args[0].(string); ok {
			t.undo = append(t.undo, func() { delete(t.pg.events, id) })
		}
	}
	return row
}

func (t *modelTx) Query(_ context.Context, sql string, _ ...any) (failmode.Rows, error) {
	if !strings.Contains(sql, "SKIP LOCKED") {
		return nil, fmt.Errorf("modelPG: unmodelled query: %s", sql)
	}
	var out []*modelRow
	for _, r := range t.pg.rows {
		if r.outageMode && !r.reconciled {
			out = append(out, r)
		}
	}
	return &modelRows{rows: out}, nil
}

func (t *modelTx) ExecAffected(_ context.Context, sql string, args ...any) (int64, error) {
	pg := t.pg
	switch {
	case strings.Contains(sql, "gateway.llm_usage_events"):
		// ON CONFLICT (event_id) DO NOTHING: a second insert adds no row.
		id, _ := args[0].(string)
		if pg.ledger[id] {
			return 0, nil
		}
		pg.ledger[id] = true
		t.undo = append(t.undo, func() { delete(pg.ledger, id) })
		return 1, nil

	case strings.Contains(sql, "INSERT INTO gateway.llm_budget_accumulators"):
		// Both the gateway's outage UPSERT and the consumer's guarded UPSERT
		// arrive here; they are told apart by the outage_mode literal each one
		// writes, exactly as the two statements differ in the source.
		outage := strings.Contains(sql, "outage_mode = true")
		scope, _ := args[2].(string)
		scopeID, _ := args[3].(string)
		periodStart, _ := args[4].(int64)
		deltaNano, _ := args[6].(int64)
		key := modelKey(scope, scopeID, periodStart)

		if r := pg.rows[key]; r != nil {
			if !outage && r.outageMode && !r.reconciled {
				// The write-back guard: WHERE NOT (outage_mode AND NOT reconciled).
				return 0, nil
			}
			before := *r
			r.accumulatedNano += deltaNano
			if outage {
				r.outageMode, r.reconciled, r.inProgress = true, false, false
			}
			t.undo = append(t.undo, func() { *r = before })
			return 1, nil
		}
		pg.nextID++
		r := &modelRow{
			id:              fmt.Sprintf("row-%d", pg.nextID),
			scope:           scope,
			scopeID:         scopeID,
			periodStartUnix: periodStart,
			accumulatedNano: deltaNano,
			outageMode:      outage,
		}
		pg.rows[key] = r
		t.undo = append(t.undo, func() { delete(pg.rows, key) })
		return 1, nil

	case strings.Contains(sql, "reconciliation_in_progress = true"):
		id, _ := args[0].(string)
		for _, r := range pg.rows {
			if r.id == id {
				r.inProgress = true
				return 1, nil
			}
		}
		return 0, nil

	case strings.Contains(sql, "SET reconciled = true"):
		id, _ := args[0].(string)
		for _, r := range pg.rows {
			if r.id == id {
				before := *r
				r.reconciled, r.outageMode, r.inProgress = true, false, false
				t.undo = append(t.undo, func() { *r = before })
				return 1, nil
			}
		}
		return 0, nil
	}
	return 0, fmt.Errorf("modelPG: unmodelled statement: %s", sql)
}

func (t *modelTx) Commit(_ context.Context) error {
	t.finish()
	return nil
}

func (t *modelTx) Rollback(_ context.Context) error {
	if t.done {
		return nil
	}
	for i := len(t.undo) - 1; i >= 0; i-- {
		t.undo[i]()
	}
	t.finish()
	return nil
}

type modelRows struct {
	rows []*modelRow
	i    int
}

func (r *modelRows) Next() bool {
	if r.i >= len(r.rows) {
		return false
	}
	r.i++
	return true
}

func (r *modelRows) Scan(dest ...any) error {
	row := r.rows[r.i-1]
	*(dest[0].(*string)) = row.id
	*(dest[1].(*string)) = row.scope
	*(dest[2].(*string)) = row.scopeID
	*(dest[3].(*int64)) = row.periodStartUnix
	*(dest[4].(*int64)) = row.accumulatedNano
	return nil
}

func (r *modelRows) Err() error { return nil }
func (r *modelRows) Close()     {}

// ─── the modelled write-back consumer ────────────────────────────────────────

// applyDelta models budgetwriteback.Store.Apply for one delta: claim the event
// id, then run the guarded UPSERT, and roll the whole transaction back when the
// guard matches no row. It reports whether the delta was applied; false means
// deferred, which is what a NAK and a later redelivery look like.
func (m *modelPG) applyDelta(t *testing.T, payload []byte) bool {
	t.Helper()
	var d struct {
		EventID      string `json:"event_id"`
		Scope        string `json:"scope"`
		ScopeID      string `json:"scope_id"`
		ProjectID    int    `json:"project_id"`
		PeriodStart  int64  `json:"period_start"`
		PeriodEnd    int64  `json:"period_end"`
		DeltaNanoUSD int64  `json:"delta_nano_usd"`
		Usage        *struct {
			PromptTokens int64 `json:"prompt_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(payload, &d); err != nil {
		t.Fatalf("delta payload: %v", err)
	}
	tx, err := m.Begin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var claimed string
	switch err := tx.QueryRow(context.Background(),
		"INSERT INTO gateway.processed_event_ids ...", d.EventID).Scan(&claimed); {
	case errors.Is(err, pgx.ErrNoRows):
		// Already applied by the other writer: contributes nothing, and the
		// transaction commits as a no-op (the consumer ACKs).
		_ = tx.Commit(context.Background())
		return true
	case err != nil:
		t.Fatal(err)
	}
	// The usage-ledger row, inside the same dedup gate (Apply's usageEventSQL).
	if d.Usage != nil {
		if _, err := tx.ExecAffected(context.Background(),
			"INSERT INTO gateway.llm_usage_events ... ON CONFLICT (event_id) DO NOTHING", d.EventID); err != nil {
			t.Fatal(err)
		}
	}
	affected, err := tx.ExecAffected(context.Background(),
		"INSERT INTO gateway.llm_budget_accumulators ... ON CONFLICT DO UPDATE ... WHERE NOT (acc.outage_mode AND NOT acc.reconciled)",
		d.ProjectID, nil, d.Scope, d.ScopeID, d.PeriodStart, d.PeriodEnd, d.DeltaNanoUSD)
	if err != nil {
		t.Fatal(err)
	}
	if affected == 0 {
		_ = tx.Rollback(context.Background())
		return false
	}
	_ = tx.Commit(context.Background())
	return true
}

// ─── the scenario ────────────────────────────────────────────────────────────

type wedgeRig struct {
	nc  *fakeNATS
	pg  *modelPG
	gs  *GovernanceStore
	rec *failmode.Reconciler
}

func newWedgeRig(t *testing.T) *wedgeRig {
	t.Helper()
	nc := newFakeNATS()
	pg := newModelPG(failmode.Snapshot{
		HardLimitNano: 100 * failmode.NanoUSD,
		SoftAlertPct:  80,
	})
	degraded := failmode.NewDegradedCounters()
	rec := failmode.NewReconciler(pg, nc2counter(nc), degraded, nil)
	gs := NewGovernanceStore(nc, failmode.NewStore(pg), degraded, rec,
		failmode.Params{Mode: failmode.ModeTieredHybrid, PGFreshness: 5 * time.Minute, ExpectedReplicas: 1},
		nil)
	gs.MarkNATSHealthy() // a production store is built only after NATS bound its assets
	gs.Start(context.Background())
	return &wedgeRig{nc: nc, pg: pg, gs: gs, rec: rec}
}

// bill drives one billed request through the real UpdateUsage and waits for the
// outage-persist goroutine, so the durable state is settled before an assert.
func (r *wedgeRig) bill(t *testing.T, eventID string, costNano int64) {
	t.Helper()
	if err := r.gs.UpdateUsage(context.Background(), wedgeProject,
		failmode.ScopeProject, wedgeScopeID, eventID, costNano,
		wedgePeriodStart, wedgePeriodEnd, nil); err != nil {
		t.Fatalf("UpdateUsage(%s): %v", eventID, err)
	}
	r.gs.Drain()
}

// billWithUsage is bill with the request's reporting dimensions, so the usage
// ledger half of each write is exercised (issue #320).
func (r *wedgeRig) billWithUsage(t *testing.T, eventID string, costNano int64) {
	t.Helper()
	userID := 7
	dims := &failmode.UsageDimensions{
		UserID: &userID, Provider: "openai", Model: "gpt-4o",
		PromptTokens: 10, CompletionTokens: 20, OccurredAtUnix: wedgePeriodStart + 60,
	}
	if err := r.gs.UpdateUsage(context.Background(), wedgeProject,
		failmode.ScopeProject, wedgeScopeID, eventID, costNano,
		wedgePeriodStart, wedgePeriodEnd, dims); err != nil {
		t.Fatalf("UpdateUsage(%s): %v", eventID, err)
	}
	r.gs.Drain()
}

// ledgerRows reports how many usage-ledger rows exist for an event id (0 or 1:
// the model enforces the ON CONFLICT (event_id) the real table has).
func (m *modelPG) ledgerRows(eventID string) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.ledger[eventID] {
		return 1
	}
	return 0
}

// modelMaxDeliver is the budget-writeback consumer's MaxDeliver from
// deploy/helm/elitea/values.yaml natsBootstrap before the deferral backoff existed.
// The modelled consumer drops a delta after that many deliveries, as JetStream
// does, so a deferral storm shows up as lost money.
const modelMaxDeliver = 10

// deliverCapped models one JetStream consumer pass over a delta that is
// redelivered immediately on every deferral (a bare Nak), up to MaxDeliver.
// It reports whether the delta was applied before the cap dropped it.
func (m *modelPG) deliverCapped(t *testing.T, payload []byte) bool {
	t.Helper()
	for range modelMaxDeliver {
		if m.applyDelta(t, payload) {
			return true
		}
	}
	return false
}

// drainDeltas hands every published delta to the modelled consumer and returns
// how many were deferred by the outage guard.
func (r *wedgeRig) drainDeltas(t *testing.T) (deferred int) {
	t.Helper()
	r.nc.mu.Lock()
	pending := r.nc.deltas
	r.nc.deltas = nil
	r.nc.mu.Unlock()
	for _, payload := range pending {
		if !r.pg.applyDelta(t, payload) {
			deferred++
			// A deferred delta is redelivered, so put it back.
			r.nc.mu.Lock()
			r.nc.deltas = append(r.nc.deltas, payload)
			r.nc.mu.Unlock()
		}
	}
	return deferred
}

// TestOutageWedge_ClearsWithoutABreakerEdge is the issue's own acceptance test:
// fail one IncrBudgetIdempotent call with a closed breaker, drive two healthy
// billed requests, and assert that the accumulator holds all three amounts.
func TestOutageWedge_ClearsWithoutABreakerEdge(t *testing.T) {
	rig := newWedgeRig(t)

	const (
		cost1 = 3 * failmode.NanoUSD
		cost2 = 4 * failmode.NanoUSD
		cost3 = 5 * failmode.NanoUSD
		evt1  = "11111111-1111-1111-1111-111111111111"
		evt2  = "22222222-2222-2222-2222-222222222222"
		evt3  = "33333333-3333-3333-3333-333333333333"
	)

	// Step 1: ONE counter operation times out. The breaker stays closed — the
	// fake never fires a transition, which is exactly what a single failure
	// below the consecutive-failure threshold does.
	rig.nc.mu.Lock()
	rig.nc.incrFailuresLeft = 1
	rig.nc.mu.Unlock()
	rig.bill(t, evt1, cost1)

	if !rig.pg.outageOwned(failmode.ScopeProject, wedgeScopeID) {
		t.Fatal("one failed counter operation did not mark the row outage-owned; the reproduction is not set up")
	}
	if got := rig.pg.accumulated(failmode.ScopeProject, wedgeScopeID); got != cost1 {
		t.Fatalf("durable spend = %d, want the outage delta %d", got, cost1)
	}

	// Step 2 and 3: two fully healthy billed requests.
	rig.bill(t, evt2, cost2)
	rig.bill(t, evt3, cost3)

	// The consumer sees all three deltas. The first is already claimed by the
	// outage write, so it contributes nothing and is ACKed. The other two are
	// deferred by the guard for as long as the row stays outage-owned.
	if deferred := rig.drainDeltas(t); deferred != 2 {
		t.Fatalf("%d deltas deferred, want 2 (the two healthy requests)", deferred)
	}
	if got := rig.pg.accumulated(failmode.ScopeProject, wedgeScopeID); got != cost1 {
		t.Fatalf("durable spend = %d, want %d — the wedge is not reproduced", got, cost1)
	}

	// Redelivery alone never resolves it: no breaker edge occurs, so before this
	// fix nothing at all cleared the flag.
	for attempt := 1; attempt <= 5; attempt++ {
		if deferred := rig.drainDeltas(t); deferred != 2 {
			t.Fatalf("redelivery %d: %d deltas deferred, want 2", attempt, deferred)
		}
	}
	if got := rig.pg.accumulated(failmode.ScopeProject, wedgeScopeID); got != cost1 {
		t.Fatalf("durable spend advanced without recovery: %d", got)
	}

	// The fix: one sweep, driven by nothing but the clock.
	rig.rec.SweepOnce(context.Background())

	if rig.pg.outageOwned(failmode.ScopeProject, wedgeScopeID) {
		t.Fatal("the sweep did not release the row")
	}
	if deferred := rig.drainDeltas(t); deferred != 0 {
		t.Fatalf("%d deltas still deferred after recovery", deferred)
	}
	if got, want := rig.pg.accumulated(failmode.ScopeProject, wedgeScopeID), int64(cost1+cost2+cost3); got != want {
		t.Fatalf("durable spend = %d, want all three amounts %d", got, want)
	}
}

// TestOutageWedge_RecoveredRowDoesNotDoubleCount is the control the fix needs to
// keep. The delta of the request that entered the outage window WAS published:
// the connection was healthy, only the counter operation timed out. Handing the
// row back without a dedup gate would let the consumer apply that money a
// second time, which turns a wedged row into a wrong one.
func TestOutageWedge_RecoveredRowDoesNotDoubleCount(t *testing.T) {
	rig := newWedgeRig(t)
	const (
		cost = 7 * failmode.NanoUSD
		evt  = "44444444-4444-4444-4444-444444444444"
	)

	rig.nc.mu.Lock()
	rig.nc.incrFailuresLeft = 1
	rig.nc.mu.Unlock()
	rig.bill(t, evt, cost)

	rig.rec.SweepOnce(context.Background())

	// Now redeliver the published delta against the released row, several times.
	for range 3 {
		if deferred := rig.drainDeltas(t); deferred != 0 {
			t.Fatal("delta deferred after recovery")
		}
		rig.nc.mu.Lock()
		rig.nc.deltas = append(rig.nc.deltas, mustDelta(t, evt, cost))
		rig.nc.mu.Unlock()
	}
	rig.nc.mu.Lock()
	rig.nc.deltas = nil
	rig.nc.mu.Unlock()

	if got := rig.pg.accumulated(failmode.ScopeProject, wedgeScopeID); got != cost {
		t.Fatalf("durable spend = %d, want %d billed exactly once", got, cost)
	}
}

// TestOutageWedge_ConsumerFirstLeavesNoOutageRow is the other order of the same
// race: the delta publish succeeded and the consumer got there first. The
// outage write must then add nothing and must NOT flag the row, because there
// is no outage to record for money that is already durable.
func TestOutageWedge_ConsumerFirstLeavesNoOutageRow(t *testing.T) {
	rig := newWedgeRig(t)
	const (
		cost = 6 * failmode.NanoUSD
		evt  = "55555555-5555-5555-5555-555555555555"
	)

	// The consumer applies the delta before the gateway's outage goroutine runs.
	if !rig.pg.applyDelta(t, mustDelta(t, evt, cost)) {
		t.Fatal("the consumer could not apply a delta to a clean row")
	}

	rig.nc.mu.Lock()
	rig.nc.incrFailuresLeft = 1
	rig.nc.mu.Unlock()
	rig.bill(t, evt, cost)

	if rig.pg.outageOwned(failmode.ScopeProject, wedgeScopeID) {
		t.Fatal("the outage write flagged a row whose money the consumer had already applied")
	}
	if got := rig.pg.accumulated(failmode.ScopeProject, wedgeScopeID); got != cost {
		t.Fatalf("durable spend = %d, want %d billed exactly once", got, cost)
	}
}

// TestOutageWedge_GenuineOutageKeepsTheRow is the negative control. While the
// NATS breaker is open the sweep must attempt nothing: the outage spend cannot
// reach the authoritative counter, so the row must stay outage-owned and the
// write-back consumer must stay barred from it. Clearing the flag on a project
// that is genuinely in outage is worse than the wedge.
func TestOutageWedge_GenuineOutageKeepsTheRow(t *testing.T) {
	rig := newWedgeRig(t)
	const (
		cost = 9 * failmode.NanoUSD
		evt  = "66666666-6666-6666-6666-666666666666"
	)

	rig.nc.mu.Lock()
	rig.nc.incrErr = nats.ErrUnavailable
	rig.nc.readErr = nats.ErrUnavailable
	rig.nc.pubErr = nats.ErrUnavailable
	rig.nc.breakerState = gobreaker.StateOpen
	rig.nc.mu.Unlock()
	rig.bill(t, evt, cost)

	if !rig.pg.outageOwned(failmode.ScopeProject, wedgeScopeID) {
		t.Fatal("a genuine outage did not mark the row outage-owned")
	}

	// Many sweeps, all of them refusing to release the row.
	for range 10 {
		rig.rec.SweepOnce(context.Background())
	}
	if !rig.pg.outageOwned(failmode.ScopeProject, wedgeScopeID) {
		t.Fatal("the sweep released a row while NATS was unreachable; the outage spend would never reach the counter")
	}

	// NATS returns. The breaker closes without any edge being delivered to the
	// reconciler — a restarted replica sees exactly this — and the next sweep
	// releases the row and replays the outage spend.
	rig.nc.mu.Lock()
	rig.nc.incrErr, rig.nc.readErr, rig.nc.pubErr = nil, nil, nil
	rig.nc.breakerState = gobreaker.StateClosed
	rig.nc.mu.Unlock()

	rig.rec.SweepOnce(context.Background())

	if rig.pg.outageOwned(failmode.ScopeProject, wedgeScopeID) {
		t.Fatal("the sweep did not release the row after NATS returned")
	}
	subject := nats.BudgetSubject(failmode.ScopeProject, wedgeScopeID, wedgePeriodStart)
	rig.nc.mu.Lock()
	total := rig.nc.totals[subject]
	rig.nc.mu.Unlock()
	if total != cost {
		t.Fatalf("authoritative counter = %d, want the replayed outage spend %d", total, cost)
	}
}

// TestOutageWedge_SweepIsGatedOnTheBreakerState proves the wiring, not just the
// behaviour. NewGovernanceStore is the only place that hands the reconciler the
// NATS breaker state; without that one line the sweep would attempt a recovery
// pass on every tick of a real outage, write the crash marker on every held row
// and count a failure each time. The NATS fake here would answer every read, so
// the breaker state is the ONLY thing that can stop the pass.
func TestOutageWedge_SweepIsGatedOnTheBreakerState(t *testing.T) {
	rig := newWedgeRig(t)
	const (
		cost = 5 * failmode.NanoUSD
		evt  = "88888888-8888-8888-8888-888888888888"
	)

	rig.nc.mu.Lock()
	rig.nc.incrFailuresLeft = 1
	rig.nc.mu.Unlock()
	rig.bill(t, evt, cost)
	if !rig.pg.outageOwned(failmode.ScopeProject, wedgeScopeID) {
		t.Fatal("the row was not marked outage-owned")
	}

	// The breaker is open. Every NATS call this fake serves would still answer.
	rig.nc.mu.Lock()
	rig.nc.breakerState = gobreaker.StateOpen
	rig.nc.mu.Unlock()

	before := rig.pg.transactions()
	for range 5 {
		rig.rec.SweepOnce(context.Background())
	}
	if got := rig.pg.transactions() - before; got != 0 {
		t.Fatalf("the sweep opened %d transactions with the breaker open; the breaker state is not wired to it", got)
	}
	if !rig.pg.outageOwned(failmode.ScopeProject, wedgeScopeID) {
		t.Fatal("the sweep released the row with the breaker open")
	}

	// Close the breaker; the same sweep now releases the row.
	rig.nc.mu.Lock()
	rig.nc.breakerState = gobreaker.StateClosed
	rig.nc.mu.Unlock()
	rig.rec.SweepOnce(context.Background())
	if rig.pg.outageOwned(failmode.ScopeProject, wedgeScopeID) {
		t.Fatal("the sweep did not release the row with the breaker closed")
	}
}

// TestOutageWedge_MemberScopeWedgesAndClears covers the second budget scope
// issue #321 added. It is the reason #515 is worth correcting now: a request
// that carries a member id performs two counter increments, so the number of
// chances to enter the outage window per request is about double.
func TestOutageWedge_MemberScopeWedgesAndClears(t *testing.T) {
	rig := newWedgeRig(t)
	memberScopeID := failmode.UserScopeID(wedgeProject, 7)
	const (
		cost = 2 * failmode.NanoUSD
		evt  = "77777777-7777-7777-7777-777777777777"
	)

	rig.nc.mu.Lock()
	rig.nc.incrFailuresLeft = 1
	rig.nc.mu.Unlock()
	if err := rig.gs.UpdateUsage(context.Background(), wedgeProject,
		failmode.ScopeUser, memberScopeID, evt, cost,
		wedgePeriodStart, wedgePeriodEnd, nil); err != nil {
		t.Fatal(err)
	}
	rig.gs.Drain()

	if !rig.pg.outageOwned(failmode.ScopeUser, memberScopeID) {
		t.Fatal("the member row was not marked outage-owned")
	}
	rig.rec.SweepOnce(context.Background())
	if rig.pg.outageOwned(failmode.ScopeUser, memberScopeID) {
		t.Fatal("the sweep left the member row wedged")
	}
}

// mustDelta builds the write-behind payload the gateway publishes, so the
// modelled consumer is fed the same JSON shape the real one decodes.
func mustDelta(t *testing.T, eventID string, costNano int64) []byte {
	t.Helper()
	payload, err := json.Marshal(deltaPayload{
		EventID:      eventID,
		Scope:        failmode.ScopeProject,
		ScopeID:      wedgeScopeID,
		ProjectID:    wedgeProject,
		PeriodStart:  wedgePeriodStart,
		PeriodEnd:    wedgePeriodEnd,
		DeltaNanoUSD: costNano,
	})
	if err != nil {
		t.Fatal(err)
	}
	return payload
}

// TestOutageWedge_PublishFailureAfterAGoodIncrementIsNotLost is the live 4.45.1
// "PublishDelta failed … nats: unavailable" case: the counter increment
// SUCCEEDS and the write-behind publish fails right after it.
//
// Before #1296 nothing wrote that delta durably. The first fix wrote it as an
// OUTAGE row, and the gateway review found the cost of that: with NATS healthy
// the row was outage-owned until the next sweep, so the consumer deferred every
// other replica's healthy delta for the scope with a bare Nak, and a busy
// stream exhausted MaxDeliver and dropped them. [human decision] 2026-10-16,
// option A: the delta is written as a HEALTHY row. This test fails with the
// outage-path write: replica B's delta is deferred ten times and dropped.
func TestOutageWedge_PublishFailureAfterAGoodIncrementIsNotLost(t *testing.T) {
	rig := newWedgeRig(t)
	const (
		cost   = 8 * failmode.NanoUSD
		otherB = 3 * failmode.NanoUSD
		evt    = "99999999-9999-9999-9999-999999999999"
		evtB   = "bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb"
	)
	subject := nats.BudgetSubject(failmode.ScopeProject, wedgeScopeID, wedgePeriodStart)

	rig.nc.mu.Lock()
	rig.nc.pubErr = nats.ErrUnavailable
	rig.nc.mu.Unlock()
	rig.billWithUsage(t, evt, cost)

	rig.nc.mu.Lock()
	counter, published := rig.nc.totals[subject], len(rig.nc.deltas)
	rig.nc.pubErr = nil
	rig.nc.mu.Unlock()
	if counter != cost || published != 0 {
		t.Fatalf("setup: counter = %d (want %d), published = %d (want 0)", counter, cost, published)
	}
	if got := rig.pg.accumulated(failmode.ScopeProject, wedgeScopeID); got != cost {
		t.Fatalf("durable spend = %d, want %d: an unpublished delta after a good increment was lost", got, cost)
	}
	if rig.pg.outageOwned(failmode.ScopeProject, wedgeScopeID) {
		t.Fatal("a delta billed while NATS is healthy marked the row outage-owned")
	}
	if n := rig.pg.ledgerRows(evt); n != 1 {
		t.Fatalf("usage-ledger rows for the unpublished delta = %d, want 1", n)
	}

	// Another replica's healthy delta for the same scope, delivered with no
	// sweep in between, as a busy stream does. It must apply, not be deferred
	// until MaxDeliver drops it.
	if !rig.pg.deliverCapped(t, mustDelta(t, evtB, otherB)) {
		t.Fatal("another replica's healthy delta was deferred past MaxDeliver and lost")
	}

	// A lost ack: the "failed" publish did reach the stream after all. The
	// consumer applies it, and the claimed event id makes it add nothing:
	// neither money nor a second ledger row.
	if !rig.pg.applyDelta(t, mustDeltaWithUsage(t, evt, cost)) {
		t.Fatal("the redelivered delta was deferred")
	}
	if got, want := rig.pg.accumulated(failmode.ScopeProject, wedgeScopeID), int64(cost+otherB); got != want {
		t.Fatalf("durable spend = %d, want %d (each request billed exactly once)", got, want)
	}
	if n := rig.pg.ledgerRows(evt); n != 1 {
		t.Fatalf("usage-ledger rows after the redelivery = %d, want exactly 1", n)
	}
	rig.nc.mu.Lock()
	counter = rig.nc.totals[subject]
	rig.nc.mu.Unlock()
	if counter != cost {
		t.Fatalf("authoritative counter = %d, want %d: nothing may replay spend the counter already has", counter, cost)
	}
}

// TestOutageWedge_PublishFailureOntoAnOutageOwnedRowStaysOutageOwned covers
// the fallback of option A: a healthy write whose guard blocks because a real
// outage already owns the row adds its money to that row (outage UPSERT), so
// nothing is lost and the recovery pass stays its only owner.
func TestOutageWedge_PublishFailureOntoAnOutageOwnedRowStaysOutageOwned(t *testing.T) {
	rig := newWedgeRig(t)
	const (
		outage  = 5 * failmode.NanoUSD
		healthy = 2 * failmode.NanoUSD
		evt1    = "cccccccc-cccc-cccc-cccc-cccccccccccc"
		evt2    = "dddddddd-dddd-dddd-dddd-dddddddddddd"
	)
	rig.nc.mu.Lock()
	rig.nc.incrFailuresLeft = 1
	rig.nc.mu.Unlock()
	rig.bill(t, evt1, outage)
	if !rig.pg.outageOwned(failmode.ScopeProject, wedgeScopeID) {
		t.Fatal("setup: the failed increment did not mark the row outage-owned")
	}

	rig.nc.mu.Lock()
	rig.nc.pubErr = nats.ErrUnavailable
	rig.nc.mu.Unlock()
	rig.billWithUsage(t, evt2, healthy)

	if got, want := rig.pg.accumulated(failmode.ScopeProject, wedgeScopeID), int64(outage+healthy); got != want {
		t.Fatalf("durable spend = %d, want %d", got, want)
	}
	if !rig.pg.outageOwned(failmode.ScopeProject, wedgeScopeID) {
		t.Fatal("the fallback released a row a real outage still owns")
	}
	if n := rig.pg.ledgerRows(evt2); n != 1 {
		t.Fatalf("usage-ledger rows = %d, want 1", n)
	}
}

func mustDeltaWithUsage(t *testing.T, eventID string, costNano int64) []byte {
	t.Helper()
	userID := 7
	payload, err := json.Marshal(deltaPayload{
		EventID:      eventID,
		Scope:        failmode.ScopeProject,
		ScopeID:      wedgeScopeID,
		ProjectID:    wedgeProject,
		PeriodStart:  wedgePeriodStart,
		PeriodEnd:    wedgePeriodEnd,
		DeltaNanoUSD: costNano,
		Usage: &usageDimsPayload{
			UserID: &userID, Provider: "openai", Model: "gpt-4o",
			PromptTokens: 10, CompletionTokens: 20, OccurredAtUnix: wedgePeriodStart + 60,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return payload
}
