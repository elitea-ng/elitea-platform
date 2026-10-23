package governance

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/EliteaAI/elitea-platform/services/elitea-llm-gateway/internal/failmode"
	"github.com/EliteaAI/elitea-platform/services/elitea-llm-gateway/internal/infra/nats"
)

// degradedRig is a store with NATS down and a fresh snapshot well below the
// soft threshold (FRESH_SAFE). The default cap is 10% of the 100 USD limit,
// 10 USD; with expectedReplicas the share is 10/N USD.
func degradedRig(t *testing.T, expectedReplicas int, reservationNano int64, markHealthy bool) (*GovernanceStore, *fakeNATS, *failmode.DegradedCounters) {
	t.Helper()
	nc := newFakeNATS()
	nc.readErr = nats.ErrUnavailable
	nc.incrErr = nats.ErrUnavailable
	db := &fakeDB{row: failmode.Snapshot{
		HardLimitNano:   limitNano,
		AccumulatedNano: 10 * failmode.NanoUSD,
		SoftAlertPct:    80,
		Found:           true,
		Age:             time.Minute,
	}}
	degraded := failmode.NewDegradedCounters()
	rec := failmode.NewReconciler(db, nc2counter(nc), degraded, nil)
	gs := NewGovernanceStore(nc, failmode.NewStore(db), degraded, rec, failmode.Params{
		Mode:                    failmode.ModeTieredHybrid,
		PGFreshness:             5 * time.Minute,
		ExpectedReplicas:        expectedReplicas,
		DegradedReservationNano: reservationNano,
	}, nil)
	if markHealthy {
		gs.MarkNATSHealthy()
	}
	gs.Start(context.Background())
	t.Cleanup(gs.Drain)
	return gs, nc, degraded
}

func subjectKey() string { return nats.BudgetSubject(testScope, testScopeID, testPeriod) }

// TestCheckBudget_ConcurrentDegradedAdmissionsAreBounded is the review's
// HIGH finding: 200 concurrent requests on a zero degraded spend all passed,
// because none of them was billed yet. Each admission now reserves against the
// share, atomically, so at most share/reservation are admitted at once.
func TestCheckBudget_ConcurrentDegradedAdmissionsAreBounded(t *testing.T) {
	const reservation = failmode.NanoUSD // 1 USD
	gs, _, degraded := degradedRig(t, 2, reservation, true)
	share := int64(5 * failmode.NanoUSD) // 10 USD cap / 2 replicas

	const n = 200
	var admitted atomic.Int64
	sets := make([]*failmode.Holds, n)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := range n {
		sets[i] = failmode.NewHolds()
		wg.Add(1)
		go func(s *failmode.Holds) {
			defer wg.Done()
			<-start
			ctx := failmode.WithHolds(context.Background(), s)
			dec, err := gs.CheckBudget(ctx, testProject, testScope, testScopeID, testPeriod, 0)
			if err != nil {
				t.Errorf("CheckBudget: %v", err)
				return
			}
			if dec.Verdict == failmode.Allow {
				admitted.Add(1)
			}
		}(sets[i])
	}
	close(start)
	wg.Wait()

	if got, want := admitted.Load(), share/reservation; got != want {
		t.Fatalf("admitted %d of %d concurrent degraded requests; want exactly %d (share %d / reservation %d)",
			got, n, want, share, int64(reservation))
	}
	if got := degraded.Held(subjectKey()); got != share {
		t.Fatalf("held = %d, want the whole share %d", got, share)
	}

	// Each admitted request is billed (NATS down: onto the degraded counter),
	// THEN releases its hold, as the billing goroutine does.
	for i, s := range sets {
		held := s.Take()
		if len(held) == 0 {
			continue
		}
		evt := "evt-" + string(rune('a'+i%26)) + time.Now().Format("150405.000000000")
		if err := gs.UpdateUsage(context.Background(), testProject, testScope, testScopeID, evt,
			reservation/2, testPeriod, testPeriodEnd, nil); err != nil {
			t.Fatalf("UpdateUsage: %v", err)
		}
		failmode.ReleaseHolds(held)
	}
	if got := degraded.Held(subjectKey()); got != 0 {
		t.Fatalf("held after billing = %d, want 0", got)
	}
	if got := degraded.Get(subjectKey()); got != share/2 {
		t.Fatalf("spent = %d, want the billed %d", got, share/2)
	}
}

// TestCheckBudget_NoHolderReservesNothing: a caller with no request holds (the
// soft-alert snapshot) only reads.
func TestCheckBudget_NoHolderReservesNothing(t *testing.T) {
	gs, _, degraded := degradedRig(t, 1, failmode.NanoUSD, true)
	for range 50 {
		dec, err := gs.CheckBudget(context.Background(), testProject, testScope, testScopeID, testPeriod, 0)
		if err != nil || dec.Verdict != failmode.Allow {
			t.Fatalf("got %+v, %v", dec, err)
		}
	}
	if got := degraded.Held(subjectKey()); got != 0 {
		t.Fatalf("a read-only check reserved %d", got)
	}
}

// TestCheckBudget_ReservationNeverExceedsShare: a reservation larger than the
// share is cut to the share, so a small share still admits ONE request.
func TestCheckBudget_ReservationNeverExceedsShare(t *testing.T) {
	gs, _, degraded := degradedRig(t, 4, 100*failmode.NanoUSD, true) // share 2.5 USD
	s := failmode.NewHolds()
	ctx := failmode.WithHolds(context.Background(), s)
	if dec, _ := gs.CheckBudget(ctx, testProject, testScope, testScopeID, testPeriod, 0); dec.Verdict != failmode.Allow {
		t.Fatalf("first request must be admitted: %+v", dec)
	}
	if got := degraded.Held(subjectKey()); got != 10*failmode.NanoUSD/4 {
		t.Fatalf("held = %d, want the share", got)
	}
	s2 := failmode.NewHolds()
	if dec, _ := gs.CheckBudget(failmode.WithHolds(context.Background(), s2), testProject, testScope, testScopeID, testPeriod, 0); dec.Verdict != failmode.Block402 {
		t.Fatalf("second request must wait for the first: %+v", dec)
	}
	s.ReleaseAll()
	if dec, _ := gs.CheckBudget(failmode.WithHolds(context.Background(), s2), testProject, testScope, testScopeID, testPeriod, 0); dec.Verdict != failmode.Allow {
		t.Fatalf("after the release the next request must be admitted: %+v", dec)
	}
}

// TestCheckBudget_CheckOnlyIgnoresOwnHold: the realtime re-check must not see
// the session's own reservation as other spend, and must not reserve again.
func TestCheckBudget_CheckOnlyIgnoresOwnHold(t *testing.T) {
	gs, _, degraded := degradedRig(t, 4, 100*failmode.NanoUSD, true) // the hold fills the share
	s := failmode.NewHolds()
	ctx := failmode.WithHolds(context.Background(), s)
	if dec, _ := gs.CheckBudget(ctx, testProject, testScope, testScopeID, testPeriod, 0); dec.Verdict != failmode.Allow {
		t.Fatalf("admission: %+v", dec)
	}
	for range 3 {
		dec, _ := gs.CheckBudget(failmode.CheckOnly(ctx), testProject, testScope, testScopeID, testPeriod, 0)
		if dec.Verdict != failmode.Allow {
			t.Fatalf("a re-check must not be refused by its own hold: %+v", dec)
		}
	}
	if s.Len() != 1 || degraded.Held(subjectKey()) != 10*failmode.NanoUSD/4 {
		t.Fatalf("a re-check reserved: holds=%d held=%d", s.Len(), degraded.Held(subjectKey()))
	}
	// Another request's re-check DOES see this hold.
	other := failmode.CheckOnly(failmode.WithHolds(context.Background(), failmode.NewHolds()))
	if dec, _ := gs.CheckBudget(other, testProject, testScope, testScopeID, testPeriod, 0); dec.Verdict != failmode.Block402 {
		t.Fatalf("another request's re-check must see the full share: %+v", dec)
	}
}

// TestCheckBudget_ReplicaThatJoinedDuringOutageGetsNoShare: a store that has
// never seen NATS healthy answers 503 while NATS is down, so a pod restarted
// during an outage adds no share to the fleet. One successful NATS read is
// enough to earn a share for the next outage.
func TestCheckBudget_ReplicaThatJoinedDuringOutageGetsNoShare(t *testing.T) {
	gs, nc, degraded := degradedRig(t, 1, failmode.NanoUSD, false)
	ctx := failmode.WithHolds(context.Background(), failmode.NewHolds())
	dec, err := gs.CheckBudget(ctx, testProject, testScope, testScopeID, testPeriod, 0)
	if err != nil {
		t.Fatalf("CheckBudget: %v", err)
	}
	if dec.Verdict != failmode.Block503 || !dec.Degraded {
		t.Fatalf("a replica that never saw NATS healthy must answer 503, got %+v", dec)
	}
	if got := degraded.Held(subjectKey()); got != 0 {
		t.Fatalf("a refused request reserved %d", got)
	}

	// NATS comes back for one read: the replica has now seen it healthy.
	nc.mu.Lock()
	nc.readErr = nil
	nc.mu.Unlock()
	if dec, _ := gs.CheckBudget(context.Background(), testProject, testScope, testScopeID, testPeriod, 0); dec.Verdict != failmode.Allow {
		t.Fatalf("NATS up: %+v", dec)
	}
	nc.mu.Lock()
	nc.readErr = nats.ErrUnavailable
	nc.mu.Unlock()
	if dec, _ := gs.CheckBudget(ctx, testProject, testScope, testScopeID, testPeriod, 0); dec.Verdict != failmode.Allow {
		t.Fatalf("after one healthy read the replica gets its share: %+v", dec)
	}
}

// TestUpdateUsage_SuccessfulIncrementMarksHealthy: a successful increment is
// also an observation of NATS healthy.
func TestUpdateUsage_SuccessfulIncrementMarksHealthy(t *testing.T) {
	gs, nc, _ := degradedRig(t, 1, failmode.NanoUSD, false)
	nc.mu.Lock()
	nc.incrErr = nil
	nc.mu.Unlock()
	if err := gs.UpdateUsage(context.Background(), testProject, testScope, testScopeID, "evt-healthy",
		1, testPeriod, testPeriodEnd, nil); err != nil {
		t.Fatalf("UpdateUsage: %v", err)
	}
	if dec, _ := gs.CheckBudget(context.Background(), testProject, testScope, testScopeID, testPeriod, 0); dec.Verdict != failmode.Allow {
		t.Fatalf("after a successful increment the replica gets its share: %+v", dec)
	}
}
