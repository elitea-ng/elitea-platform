package governance

import (
	"context"
	"testing"

	"github.com/elitea-ng/elitea-platform/services/elitea-llm-gateway/internal/failmode"
)

// TestCheckBudget_ReportsTheFiguresAnEstimateAdmissionNeeds pins the two
// Decision fields the image admission reads (issue 1969): the authoritative
// counter and the ceiling, on the healthy path only. The estimate is NOT fed to
// the FSM there: the soft-alert re-read passes the cost it just billed, and
// counting it again would refuse a request that fits.
func TestCheckBudget_ReportsTheFiguresAnEstimateAdmissionNeeds(t *testing.T) {
	nc := newFakeNATS()
	nc.totals[makeSubject()] = 10 * failmode.NanoUSD
	db := &fakeDB{row: failmode.Snapshot{HardLimitNano: limitNano, AccumulatedNano: 10 * failmode.NanoUSD, SoftAlertPct: 80, Found: true}}
	gs := newStore(nc, db)

	// An estimate far above the room left: the FSM still allows (it never
	// reads reqCostNano on the healthy path), and the figures say so.
	dec, err := gs.CheckBudget(context.Background(), testProject, testScope, testScopeID, testPeriod, 10*limitNano)
	if err != nil {
		t.Fatal(err)
	}
	if dec.Verdict != failmode.Allow {
		t.Fatalf("verdict = %v: the healthy-path FSM must not read the estimate", dec.Verdict)
	}
	if dec.SpentNano != 10*failmode.NanoUSD || dec.HardLimitNano != limitNano {
		t.Fatalf("figures = spent %d, limit %d; want %d and %d", dec.SpentNano, dec.HardLimitNano, 10*failmode.NanoUSD, limitNano)
	}
}

func TestCheckBudget_NoFiguresForAnUnlimitedScopeOrADownNATS(t *testing.T) {
	nc := newFakeNATS()
	nc.totals[makeSubject()] = 5
	gs := newStore(nc, &fakeDB{rowErr: failmode.ErrNoBudgetRow})
	dec, err := gs.CheckBudget(context.Background(), testProject, testScope, testScopeID, testPeriod, 100)
	if err != nil {
		t.Fatal(err)
	}
	if dec.SpentNano != 0 || dec.HardLimitNano != 0 {
		t.Fatalf("an unlimited scope reported figures: %+v", dec)
	}
}
