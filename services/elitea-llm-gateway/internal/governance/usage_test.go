package governance

import (
	"context"
	"errors"
	"testing"

	"github.com/elitea-ng/elitea-platform/services/elitea-llm-gateway/internal/failmode"
)

// stubDefaults is a BudgetDefaults that answers with fixed values.
type stubDefaults struct {
	limitNano int64
	softPct   int
	failMode  string
	ok        bool
	calls     int
}

func (s *stubDefaults) DefaultBudgetNano(int) (int64, int, string, bool) {
	s.calls++
	return s.limitNano, s.softPct, s.failMode, s.ok
}

// --- BudgetFraction -------------------------------------------------------

func TestBudgetFractionUsesTheAuthoritativeCounter(t *testing.T) {
	nc := newFakeNATS()
	nc.totals[makeSubject()] = limitNano / 4 // 25 USD of a 100 USD ceiling
	db := &fakeDB{row: failmode.Snapshot{
		HardLimitNano:   limitNano,
		AccumulatedNano: 0, // the durable tier is behind; the counter is authoritative
		Found:           true,
	}}
	gs := newStore(nc, db)

	frac, ok := gs.BudgetFraction(context.Background(), testProject, testScope, testScopeID, testPeriod)
	if !ok {
		t.Fatal("BudgetFraction reported no ceiling for a budgeted project")
	}
	if frac < 0.249 || frac > 0.251 {
		t.Errorf("fraction = %v, want ~0.25", frac)
	}
}

// TestBudgetFractionFallsBackToTheSnapshot pins the UNDER-reporting direction.
// A rule that sends heavy spenders elsewhere must not fire on a number the
// gateway cannot stand behind.
func TestBudgetFractionFallsBackToTheSnapshot(t *testing.T) {
	nc := newFakeNATS()
	nc.readErr = errors.New("nats: unavailable")
	db := &fakeDB{row: failmode.Snapshot{
		HardLimitNano:   limitNano,
		AccumulatedNano: limitNano / 2,
		Found:           true,
	}}
	gs := newStore(nc, db)

	frac, ok := gs.BudgetFraction(context.Background(), testProject, testScope, testScopeID, testPeriod)
	if !ok {
		t.Fatal("BudgetFraction gave up when NATS was unavailable")
	}
	if frac < 0.499 || frac > 0.501 {
		t.Errorf("fraction = %v, want ~0.5 from the durable snapshot", frac)
	}
}

// TestBudgetFractionNeverFailsARequest covers every unreadable state. Each must
// resolve to "no ceiling", because this value only ever selects a route.
func TestBudgetFractionNeverFailsARequest(t *testing.T) {
	cases := []struct {
		name string
		db   *fakeDB
	}{
		{"unlimited project", &fakeDB{row: failmode.Snapshot{IsUnlimited: true}}},
		{"no ceiling", &fakeDB{row: failmode.Snapshot{HardLimitNano: 0}}},
		{"no budget row", &fakeDB{rowErr: failmode.ErrNoBudgetRow}},
		{"snapshot read failed", &fakeDB{rowErr: errors.New("connection refused")}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gs := newStore(newFakeNATS(), tc.db)
			frac, ok := gs.BudgetFraction(context.Background(), testProject, testScope, testScopeID, testPeriod)
			if ok || frac != 0 {
				t.Errorf("got (%v, %v), want (0, false)", frac, ok)
			}
		})
	}

	var nilStore *GovernanceStore
	if frac, ok := nilStore.BudgetFraction(context.Background(), 1, "project", "1", 0); ok || frac != 0 {
		t.Errorf("a nil store returned (%v, %v)", frac, ok)
	}
}

// TestBudgetFractionClampsANegativeTotal guards the correction-overshoot case:
// a negative counter must not produce a negative fraction that would make
// `budget_used < 0.5` true for a project that is over its limit.
func TestBudgetFractionClampsANegativeTotal(t *testing.T) {
	nc := newFakeNATS()
	nc.totals[makeSubject()] = -5
	db := &fakeDB{row: failmode.Snapshot{HardLimitNano: limitNano, Found: true}}
	gs := newStore(nc, db)

	frac, ok := gs.BudgetFraction(context.Background(), testProject, testScope, testScopeID, testPeriod)
	if !ok || frac != 0 {
		t.Errorf("got (%v, %v), want (0, true)", frac, ok)
	}
}

// --- authored budget defaults --------------------------------------------

// TestAuthoredDefaultAppliesWhenNoProjectRowExists is the wiring that makes an
// authored budget row do anything at all.
func TestAuthoredDefaultAppliesWhenNoProjectRowExists(t *testing.T) {
	nc := newFakeNATS()
	// The project has already spent past the authored 100 USD ceiling.
	nc.totals[makeSubject()] = limitNano * 2
	db := &fakeDB{rowErr: failmode.ErrNoBudgetRow}
	gs := newStore(nc, db)
	gs.SetBudgetDefaults(&stubDefaults{limitNano: limitNano, softPct: 80, ok: true})

	dec, err := gs.CheckBudget(context.Background(), testProject, testScope, testScopeID, testPeriod, 0)
	if err != nil {
		t.Fatalf("CheckBudget: %v", err)
	}
	if dec.Verdict != failmode.Block402 {
		t.Errorf("verdict = %v, want Block402 — the authored ceiling did not bite", dec.Verdict)
	}
}

// TestNoAuthoredDefaultKeepsTheUnlimitedBehaviour is the control. Without it, a
// change that made every project blocked would pass the test above.
func TestNoAuthoredDefaultKeepsTheUnlimitedBehaviour(t *testing.T) {
	nc := newFakeNATS()
	nc.totals[makeSubject()] = limitNano * 2
	db := &fakeDB{rowErr: failmode.ErrNoBudgetRow}

	// No defaults wired at all.
	gs := newStore(nc, db)
	dec, err := gs.CheckBudget(context.Background(), testProject, testScope, testScopeID, testPeriod, 0)
	if err != nil {
		t.Fatalf("CheckBudget: %v", err)
	}
	if dec.Verdict != failmode.Allow {
		t.Errorf("verdict = %v with no authored default, want Allow", dec.Verdict)
	}

	// Defaults wired but answering "no authored budget".
	gs2 := newStore(nc, db)
	stub := &stubDefaults{ok: false}
	gs2.SetBudgetDefaults(stub)
	dec2, err := gs2.CheckBudget(context.Background(), testProject, testScope, testScopeID, testPeriod, 0)
	if err != nil {
		t.Fatalf("CheckBudget: %v", err)
	}
	if dec2.Verdict != failmode.Allow {
		t.Errorf("verdict = %v when the default declined, want Allow", dec2.Verdict)
	}
	if stub.calls == 0 {
		t.Error("the defaults port was never consulted")
	}
}

// TestAuthoredDefaultDoesNotOverrideAProjectRow is the containment guarantee:
// the fallback is reachable only when there is no per-project row.
func TestAuthoredDefaultDoesNotOverrideAProjectRow(t *testing.T) {
	nc := newFakeNATS()
	nc.totals[makeSubject()] = limitNano * 2
	// The project has its own row, and it is unlimited.
	db := &fakeDB{row: failmode.Snapshot{IsUnlimited: true, Found: true}}
	gs := newStore(nc, db)
	stub := &stubDefaults{limitNano: 1, softPct: 80, ok: true}
	gs.SetBudgetDefaults(stub)

	dec, err := gs.CheckBudget(context.Background(), testProject, testScope, testScopeID, testPeriod, 0)
	if err != nil {
		t.Fatalf("CheckBudget: %v", err)
	}
	if dec.Verdict != failmode.Allow {
		t.Errorf("verdict = %v; the project's own unlimited row must win", dec.Verdict)
	}
	if stub.calls != 0 {
		t.Error("the authored default was consulted for a project that has its own row")
	}
}

func TestSetBudgetDefaultsIsNilSafe(t *testing.T) {
	var gs *GovernanceStore
	gs.SetBudgetDefaults(&stubDefaults{})
}
