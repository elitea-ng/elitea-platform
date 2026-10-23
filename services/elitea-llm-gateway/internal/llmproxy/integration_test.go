// integration_test.go — BF0.9d behavioral integration test for the /llm budget path.
//
// Unlike budget_gate_test.go (which stubs BudgetChecker with a fake), this file
// wires the REAL governance.GovernanceStore — built from a fake NATS client and
// fake failmode.DB — directly into a REAL Handler via WithBudgetGate.  The entire
// chain is exercised:
//
//	HTTP request → Handler.Chat → checkBudget → GovernanceStore.CheckBudget
//	  → fakeNATS.ReadBudget (NATS counter) + integDB.QueryRow (PG snapshot)
//	  → failmode.Decide (FSM) → verdict → HTTP response
//
// and for the billed-completion path:
//
//	Handler.Chat → updateUsage → GovernanceStore.UpdateUsage
//	  → fakeNATS.IncrBudgetIdempotent (counter increment)
//	  → fakeNATS.PublishDelta (write-behind)
//
// Fakes are used ONLY for the leaf infrastructure (NATS transport, PG DB).  The
// GovernanceStore, failmode.Store, DegradedCounters, Reconciler, cost.Calculator,
// and the Handler itself are all real.
package llmproxy

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sony/gobreaker/v2"

	"github.com/EliteaAI/elitea-platform/services/elitea-llm-gateway/internal/cost"
	"github.com/EliteaAI/elitea-platform/services/elitea-llm-gateway/internal/failmode"
	"github.com/EliteaAI/elitea-platform/services/elitea-llm-gateway/internal/governance"
	natspkg "github.com/EliteaAI/elitea-platform/services/elitea-llm-gateway/internal/infra/nats"
	"github.com/maximhq/bifrost/core/schemas"
)

// ── fake NATS client (satisfies governance's unexported natsClient interface) ─

// integNATS is a thread-safe in-memory NATS fake.  It implements the full
// method set of governance.natsClient so it can be passed to
// governance.NewGovernanceStore without any live NATS server.
type integNATS struct {
	mu sync.Mutex

	// totals maps subject → current authoritative counter value (nano-USD).
	totals map[string]int64
	// applied tracks event_ids already applied (idempotency guard).
	applied map[string]bool
	// deltas records every payload sent via PublishDelta.
	deltas [][]byte

	// error overrides (nil = success).
	readErr error
	incrErr error
	pubErr  error

	breakerState  gobreaker.State
	stateChangeFn func(from, to gobreaker.State)
}

func newIntegNATS() *integNATS {
	return &integNATS{
		totals:  make(map[string]int64),
		applied: make(map[string]bool),
	}
}

func (n *integNATS) ReadBudget(_ context.Context, subject string) (int64, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.readErr != nil {
		return 0, n.readErr
	}
	return n.totals[subject], nil
}

func (n *integNATS) IncrBudget(_ context.Context, subject string, delta int64) (int64, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.incrErr != nil {
		return 0, n.incrErr
	}
	n.totals[subject] += delta
	return n.totals[subject], nil
}

func (n *integNATS) IncrBudgetIdempotent(_ context.Context, subject, eventID string, delta int64) (int64, bool, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.incrErr != nil {
		return 0, false, n.incrErr
	}
	if n.applied[eventID] {
		return n.totals[subject], false, nil // duplicate suppressed
	}
	n.applied[eventID] = true
	n.totals[subject] += delta
	return n.totals[subject], true, nil
}

func (n *integNATS) PublishDelta(_ context.Context, _ string, payload []byte) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.pubErr != nil {
		return n.pubErr
	}
	cp := make([]byte, len(payload))
	copy(cp, payload)
	n.deltas = append(n.deltas, cp)
	return nil
}

func (n *integNATS) TryAlertCooldown(_ context.Context, _ string) (bool, error) {
	return true, nil
}

func (n *integNATS) OnBreakerStateChange(fn func(from, to gobreaker.State)) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.stateChangeFn = fn
}

func (n *integNATS) BreakerState() gobreaker.State {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.breakerState
}

// getTotal safely reads the counter total for subject.
func (n *integNATS) getTotal(subject string) int64 {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.totals[subject]
}

// deltaCount safely returns the number of write-behind deltas published so far.
func (n *integNATS) deltaCount() int {
	n.mu.Lock()
	defer n.mu.Unlock()
	return len(n.deltas)
}

// ── fake failmode.DB (satisfies failmode.DB) ──────────────────────────────────

// integDB implements failmode.DB for integration tests.  QueryRow returns a
// scripted single-row result that failmode.Store.ReadSnapshot can scan.
type integDB struct {
	snap    failmode.Snapshot
	snapErr error
}

// intRow is a scripted failmode.Row.
type intRow struct {
	vals []any
	err  error
}

func (r intRow) Scan(dest ...any) error {
	if r.err != nil {
		return r.err
	}
	if len(dest) != len(r.vals) {
		return errors.New("intRow: arity mismatch")
	}
	for i, v := range r.vals {
		if err := intAssign(dest[i], v); err != nil {
			return err
		}
	}
	return nil
}

// intAssign copies v into dest, handling the types that ReadSnapshot scans.
func intAssign(dest, v any) error {
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
		// v is nil (untyped) for SQL NULL.
		if v == nil {
			*p = nil
		} else {
			s := v.(string)
			*p = &s
		}
	default:
		return errors.New("intRow: unsupported dest type")
	}
	return nil
}

// QueryRow satisfies failmode.DB.  It returns a scripted row encoding the
// preset Snapshot.  The SQL text and arguments are ignored — we always return
// the same scripted row, which is fine because each test creates its own integDB.
func (d *integDB) QueryRow(_ context.Context, _ string, _ ...any) failmode.Row {
	if d.snapErr != nil {
		return intRow{err: d.snapErr}
	}
	snap := d.snap
	// Encode in the column order failmode.Store.ReadSnapshot scans:
	//   is_unlimited, hard_limit_nano, accumulated_nano, soft_alert_pct,
	//   nats_fail_mode (*string / NULL), acc_found, age_seconds,
	//   soft_alerts_disabled
	var natsFM any // nil untyped interface → SQL NULL
	return intRow{vals: []any{
		snap.IsUnlimited,
		snap.HardLimitNano,
		snap.AccumulatedNano,
		snap.SoftAlertPct,
		natsFM,
		snap.Found,
		snap.Age.Seconds(),
		snap.SoftAlertsDisabled,
	}}
}

// Begin satisfies failmode.DB for the Reconciler's recovery path.  It returns a
// no-op transaction that yields no outage rows, so recovery passes complete
// trivially without touching the NATS fake.
func (d *integDB) Begin(_ context.Context) (failmode.Tx, error) {
	return &intNopTx{}, nil
}

// intNopTx is a no-op failmode.Tx for the reconciler in integration tests.
type intNopTx struct{}

func (t *intNopTx) QueryRow(_ context.Context, sql string, args ...any) failmode.Row {
	// Issue #515: the outage-window write claims its event id first.
	if strings.Contains(sql, "processed_event_ids") {
		id, _ := args[0].(string)
		return intRow{vals: []any{id}}
	}
	return intRow{err: errors.New("intNopTx: not used")}
}
func (t *intNopTx) Query(_ context.Context, _ string, _ ...any) (failmode.Rows, error) {
	return &intEmptyRows{}, nil // no outage rows
}
func (t *intNopTx) ExecAffected(_ context.Context, _ string, _ ...any) (int64, error) {
	return 1, nil
}
func (t *intNopTx) Commit(_ context.Context) error   { return nil }
func (t *intNopTx) Rollback(_ context.Context) error { return nil }

// intEmptyRows is a failmode.Rows that yields no rows.
type intEmptyRows struct{}

func (r *intEmptyRows) Next() bool          { return false }
func (r *intEmptyRows) Scan(_ ...any) error { return errors.New("intEmptyRows: no rows") }
func (r *intEmptyRows) Err() error          { return nil }
func (r *intEmptyRows) Close()              {}

// ── nc2integCounter adapts integNATS to failmode.Counter for the Reconciler ──

type nc2integCounter struct{ nc *integNATS }

func (a *nc2integCounter) ReadBudget(ctx context.Context, subject string) (int64, error) {
	return a.nc.ReadBudget(ctx, subject)
}

func (a *nc2integCounter) IncrBudgetIdempotent(ctx context.Context, subject, eventID string, delta int64) (int64, bool, error) {
	return a.nc.IncrBudgetIdempotent(ctx, subject, eventID, delta)
}

func (a *nc2integCounter) BudgetSubject(scope, scopeID string, periodStartUnix int64) string {
	return natspkg.BudgetSubject(scope, scopeID, periodStartUnix)
}

// ── buildIntStore assembles the real GovernanceStore over the two fakes ───────

func buildIntStore(nc *integNATS, db *integDB) *governance.GovernanceStore {
	fmStore := failmode.NewStore(db)
	degraded := failmode.NewDegradedCounters()
	rec := failmode.NewReconciler(db, &nc2integCounter{nc: nc}, degraded, nil)
	params := failmode.Params{
		Mode:             failmode.ModeTieredHybrid,
		PGFreshness:      5 * time.Minute,
		ExpectedReplicas: 1,
		DegradedCapNano:  0,
	}
	gs := governance.NewGovernanceStore(nc, fmStore, degraded, rec, params, nil)
	gs.Start(context.Background())
	return gs
}

// ── integration test constants ────────────────────────────────────────────────

const (
	integProjectIDStr = "42"
	// integLimitNano is the hard budget limit used in the over-budget subtests:
	// 100 USD expressed in nano-USD.
	integLimitNano = int64(100) * failmode.NanoUSD
)

// ── TestGatewayIntegration ────────────────────────────────────────────────────

// TestGatewayIntegration is the BF0.9d behavioral gate.  It exercises the full
// handler → GovernanceStore → fake NATS chain without stubbing the store itself.
func TestGatewayIntegration(t *testing.T) {
	// periodStart matches what the handler computes internally via
	// billingPeriodStart(time.Now()) on the same wall-clock.  Because the test
	// is fast (sub-second) and never straddles a month boundary, the value is
	// stable for the duration of the run.
	periodStart := billingPeriodStart(time.Now())
	subject := natspkg.BudgetSubject("project", integProjectIDStr, periodStart)

	// ── 1. over-budget → 402, provider NEVER invoked ─────────────────────────
	t.Run("over-budget → 402 before provider", func(t *testing.T) {
		nc := newIntegNATS()
		// NATS counter at exactly the hard limit: authoritativeNano >= hardLimitNano → Block402.
		nc.totals[subject] = integLimitNano

		db := &integDB{snap: failmode.Snapshot{
			IsUnlimited:     false,
			HardLimitNano:   integLimitNano,
			AccumulatedNano: integLimitNano,
			SoftAlertPct:    80,
			Found:           true,
			Age:             0,
		}}

		gs := buildIntStore(nc, db)
		calc := cost.New(cost.Config{}) // no catalog DB → default price table

		router := &trackingRouter{}
		router.chatResp = &schemas.BifrostChatResponse{ID: "should-never-reach"}
		h := NewHandler(router, nil, nil, WithBudgetGate(gs, calc))

		req := chatReqWithProject(t, integProjectIDStr, false)
		rec := httptest.NewRecorder()
		h.Chat(rec, req)

		// Must be 402 budget_exceeded.
		if rec.Code != http.StatusPaymentRequired {
			t.Fatalf("status = %d, want 402 (budget_exceeded); body=%s", rec.Code, rec.Body.String())
		}
		// Provider must have been blocked.
		if router.called.Load() {
			t.Error("provider was invoked despite over-budget verdict — gate did not enforce")
		}
		// Error body shape.
		var out openAIError
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("unmarshal error body: %v", err)
		}
		if out.Error.Type != "budget_exceeded" {
			t.Errorf("error.type = %q, want budget_exceeded", out.Error.Type)
		}
		if out.Error.Code != "insufficient_quota" {
			t.Errorf("error.code = %q, want insufficient_quota", out.Error.Code)
		}
	})

	// ── 2. under-budget → provider called + 200 response ─────────────────────
	t.Run("under-budget → provider called and 200 returned", func(t *testing.T) {
		nc := newIntegNATS()
		// 10 USD of 100 USD consumed — comfortably under limit.
		nc.totals[subject] = 10 * failmode.NanoUSD

		db := &integDB{snap: failmode.Snapshot{
			IsUnlimited:     false,
			HardLimitNano:   integLimitNano,
			AccumulatedNano: 10 * failmode.NanoUSD,
			SoftAlertPct:    80,
			Found:           true,
			Age:             0,
		}}

		gs := buildIntStore(nc, db)
		calc := cost.New(cost.Config{})

		router := &trackingRouter{}
		router.chatResp = &schemas.BifrostChatResponse{
			ID:    "cmpl-under-budget",
			Model: "gpt-4o",
			Usage: &schemas.BifrostLLMUsage{PromptTokens: 100, CompletionTokens: 50},
		}
		h := NewHandler(router, nil, nil, WithBudgetGate(gs, calc))

		req := chatReqWithProject(t, integProjectIDStr, false)
		rec := httptest.NewRecorder()
		h.Chat(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
		}
		if !router.called.Load() {
			t.Error("provider was NOT invoked despite Allow verdict")
		}
		// Response body should carry the provider's reply.
		var out map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("unmarshal response body: %v", err)
		}
		if out["id"] != "cmpl-under-budget" {
			t.Errorf("response id = %v, want cmpl-under-budget", out["id"])
		}
	})

	// ── 3. counter increments on billed completion ────────────────────────────
	//
	// After a successful unary chat completion with non-zero token usage, the
	// real cost.Calculator computes a positive cost and updateUsage calls
	// GovernanceStore.UpdateUsage → IncrBudgetIdempotent on the fake NATS client.
	// We assert:
	//   (a) the NATS counter for the project's subject is larger after the call, and
	//   (b) a write-behind delta was published.
	t.Run("counter increments on billed completion", func(t *testing.T) {
		nc := newIntegNATS()
		initialTotal := int64(5) * failmode.NanoUSD
		nc.totals[subject] = initialTotal

		db := &integDB{snap: failmode.Snapshot{
			IsUnlimited:     false,
			HardLimitNano:   integLimitNano,
			AccumulatedNano: initialTotal,
			SoftAlertPct:    80,
			Found:           true,
			Age:             0,
		}}

		gs := buildIntStore(nc, db)
		// Real cost.Calculator with no catalog DB → uses default price table.
		// gpt-4o-mini pricing: 150_000_000 nanoUSD/1M input, 600_000_000 nanoUSD/1M output.
		// With 200 input + 100 output tokens the real math is:
		//   input:  round(200 * 150_000_000 / 1_000_000)  = 30_000 nanoUSD
		//   output: round(100 * 600_000_000 / 1_000_000)  = 60_000 nanoUSD
		//   total:  90_000 nanoUSD — non-zero, so UpdateUsage fires.
		calc := cost.New(cost.Config{})

		router := &trackingRouter{}
		// The model string passed to Cost() comes from req.Provider / req.Model
		// after Bifrost's ToBifrostChatRequest.  The response model field is what
		// the fake router echoes back; it is NOT used by updateUsage (which reads
		// provider/model from the REQUEST).  The chat request body sent by
		// chatReqWithProject uses "openai/gpt-4o"; Bifrost splits this into
		// Provider="openai", Model="gpt-4o".  defaultPrice("gpt-4o") matches the
		// "gpt-4o" prefix in the default table.  Either way, TotalNanoUSD > 0 for
		// any non-zero token counts, so the exact price path does not affect
		// whether UpdateUsage is called.
		router.chatResp = &schemas.BifrostChatResponse{
			ID:    "cmpl-billing",
			Model: "openai/gpt-4o",
			Usage: &schemas.BifrostLLMUsage{PromptTokens: 200, CompletionTokens: 100},
		}
		h := NewHandler(router, nil, nil, WithBudgetGate(gs, calc))

		req := chatReqWithProject(t, integProjectIDStr, false)
		rec := httptest.NewRecorder()
		h.Chat(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
		}
		if !router.called.Load() {
			t.Error("provider was NOT invoked — under-budget Allow expected")
		}

		// FIX: billing is now async (off the HTTP critical path), so we must
		// poll until the goroutine completes rather than reading immediately.
		deadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline) && nc.getTotal(subject) <= initialTotal {
			time.Sleep(5 * time.Millisecond)
		}

		// NATS counter must have increased beyond the seeded initialTotal.
		finalTotal := nc.getTotal(subject)
		if finalTotal <= initialTotal {
			t.Errorf("NATS counter not incremented: initial=%d final=%d (want final > initial)",
				initialTotal, finalTotal)
		}

		// Assert the EXACT billed amount using the same cost.Calculator the handler
		// uses.  chatReqWithProject sends "openai/gpt-4o"; the fake router response
		// carries PromptTokens=200, CompletionTokens=100.  With gpt-4o default
		// pricing (2_500_000_000 / 10_000_000_000 nanoUSD per 1M tokens):
		//   input:  round(200 * 2_500_000_000 / 1_000_000) = 500_000 nanoUSD
		//   output: round(100 * 10_000_000_000 / 1_000_000) = 1_000_000 nanoUSD
		//   total:  1_500_000 nanoUSD
		// We compute this via the real Calculator so the test stays correct if the
		// default price table is updated.
		expectedCost := cost.New(cost.Config{}).Cost(context.Background(), "openai", "gpt-4o", 200, 100).TotalNanoUSD
		if expectedCost <= 0 {
			t.Fatal("cost.Calculator returned zero cost for gpt-4o 200+100 tokens — check default price table")
		}
		if finalTotal != initialTotal+expectedCost {
			t.Errorf("NATS counter = %d, want %d (initial=%d + cost=%d); billed amount is wrong",
				finalTotal, initialTotal+expectedCost, initialTotal, expectedCost)
		}

		// A write-behind delta must have been published to the GATEWAY_BUDGET_DELTAS stream.
		if nc.deltaCount() == 0 {
			t.Error("no write-behind delta published after billed completion — UpdateUsage incomplete")
		}
	})
}
