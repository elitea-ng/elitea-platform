package governance

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/elitea-ng/elitea-platform/services/elitea-llm-gateway/internal/failmode"
	"github.com/elitea-ng/elitea-platform/services/elitea-llm-gateway/internal/infra/nats"
)

// degradedRig is a store with NATS down and a fresh snapshot well below the
// soft threshold (FRESH_SAFE). The default cap is 10% of the 100 USD limit,
// 10 USD; with expectedReplicas the share is 10/N USD.
func degradedRig(t *testing.T, expectedReplicas int, reservationNano int64) (*GovernanceStore, *fakeNATS, *failmode.DegradedCounters) {
	t.Helper()
	return degradedRigLimit(t, expectedReplicas, reservationNano, limitNano)
}

func degradedRigLimit(t testing.TB, expectedReplicas int, reservationNano, limit int64) (*GovernanceStore, *fakeNATS, *failmode.DegradedCounters) {
	t.Helper()
	nc := newFakeNATS()
	nc.readErr = nats.ErrUnavailable
	nc.incrErr = nats.ErrUnavailable
	db := &fakeDB{row: failmode.Snapshot{
		HardLimitNano:   limit,
		AccumulatedNano: limit / 10,
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
	gs, _, degraded := degradedRig(t, 2, reservation)
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
	// THEN its request ends and the holds go, as in production.
	for i, s := range sets {
		if s.Len() == 0 {
			s.End()
			continue
		}
		if err := gs.UpdateUsage(context.Background(), testProject, testScope, testScopeID, fmt.Sprintf("evt-%d", i),
			reservation/2, testPeriod, testPeriodEnd, nil); err != nil {
			t.Fatalf("UpdateUsage: %v", err)
		}
		s.End()
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
	gs, _, degraded := degradedRig(t, 1, failmode.NanoUSD)
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

// TestCheckBudget_ShareSmallerThanReservationRefuses: the reservation is not
// cut to the share. A share smaller than the reservation admits nothing,
// because a cut reservation would let one request of any cost in.
func TestCheckBudget_ShareSmallerThanReservationRefuses(t *testing.T) {
	gs, _, degraded := degradedRig(t, 4, 3*failmode.NanoUSD) // share 2.5 USD < 3 USD
	s := failmode.NewHolds()
	dec, err := gs.CheckBudget(failmode.WithHolds(context.Background(), s), testProject, testScope, testScopeID, testPeriod, 0)
	if err != nil {
		t.Fatalf("CheckBudget: %v", err)
	}
	if dec.Verdict != failmode.Block402 || !dec.Capped || dec.ReplicaShareNano != 10*failmode.NanoUSD/4 {
		t.Fatalf("a share below the reservation must refuse with 402 and report the share: %+v", dec)
	}
	if s.Len() != 0 || degraded.Held(subjectKey()) != 0 {
		t.Fatalf("a refused request reserved: holds=%d held=%d", s.Len(), degraded.Held(subjectKey()))
	}
}

// TestCheckBudget_DefaultReservationFitsNormalBudgets states the condition the
// docs give: with the default cap (10% of the hard limit) and the default
// reservation (0.10 USD), a replica admits while limit / 10 / N >= 0.10 USD,
// that is while the hard limit is at least N USD.
func TestCheckBudget_DefaultReservationFitsNormalBudgets(t *testing.T) {
	for _, n := range []int{1, 2, 3, 7, 10, 20} {
		for _, limitUSD := range []int64{1, 5, 10, 20, 100, 1000} {
			gs, _, _ := degradedRigLimit(t, n, 0, limitUSD*failmode.NanoUSD) // 0 = the default reservation
			dec, err := gs.CheckBudget(failmode.WithHolds(context.Background(), failmode.NewHolds()),
				testProject, testScope, testScopeID, testPeriod, 0)
			if err != nil {
				t.Fatalf("CheckBudget: %v", err)
			}
			want := failmode.Block402
			if limitUSD >= int64(n) {
				want = failmode.Allow
			}
			if dec.Verdict != want {
				t.Fatalf("limit %d USD, %d replicas: verdict %v, want %v (admit iff limit >= N USD)",
					limitUSD, n, dec.Verdict, want)
			}
		}
	}
}

// TestCheckBudget_CheckOnlyIgnoresOwnHold: the realtime re-check must not see
// the session's own reservation as other spend, and must not reserve again.
func TestCheckBudget_CheckOnlyIgnoresOwnHold(t *testing.T) {
	gs, _, degraded := degradedRig(t, 4, 10*failmode.NanoUSD/4) // the hold fills the 2.5 USD share
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

// TestCheckBudget_CheckOnlyAcrossTwoOutages: a realtime session admitted in
// one outage and re-checked in the next. Recovery cleared its hold from the
// counter, so the re-check must not subtract it again: with the new outage's
// share filled by other spend, the re-check refuses.
func TestCheckBudget_CheckOnlyAcrossTwoOutages(t *testing.T) {
	share := int64(10 * failmode.NanoUSD / 4)
	for name, reset := range map[string]func(*failmode.DegradedCounters){
		"Reset":    func(d *failmode.DegradedCounters) { d.Reset(subjectKey()) },
		"ResetAll": func(d *failmode.DegradedCounters) { d.ResetAll() },
	} {
		gs, _, degraded := degradedRig(t, 4, share)
		s := failmode.NewHolds()
		ctx := failmode.WithHolds(context.Background(), s)
		if dec, _ := gs.CheckBudget(ctx, testProject, testScope, testScopeID, testPeriod, 0); dec.Verdict != failmode.Allow {
			t.Fatalf("%s: admission in outage 1: %+v", name, dec)
		}
		// NATS recovers; the reconciler resets the window. Outage 2 begins and
		// other requests bill the whole share.
		reset(degraded)
		degraded.Add(subjectKey(), share)
		dec, _ := gs.CheckBudget(failmode.CheckOnly(ctx), testProject, testScope, testScopeID, testPeriod, 0)
		if dec.Verdict != failmode.Block402 {
			t.Fatalf("%s: the re-check subtracted a hold the reset already cleared: %+v", name, dec)
		}
		s.End()
	}
}

// TestDumpTotal_ExcludesReservations: the dump reports spend, and a
// reservation of a request in flight is not spend.
func TestDumpTotal_ExcludesReservations(t *testing.T) {
	gs, _, degraded := degradedRig(t, 1, failmode.NanoUSD)
	s := failmode.NewHolds()
	if dec, _ := gs.CheckBudget(failmode.WithHolds(context.Background(), s), testProject, testScope, testScopeID, testPeriod, 0); dec.Verdict != failmode.Allow {
		t.Fatalf("admission: %+v", dec)
	}
	degraded.Add(subjectKey(), 7)
	if got := gs.DumpTotal(testScope, testScopeID, testPeriod); got != 7 {
		t.Fatalf("DumpTotal = %d, want the billed 7 (not the 1 USD hold)", got)
	}
	if got := degraded.Get(subjectKey()); got != 7+failmode.NanoUSD {
		t.Fatalf("the gate must still see billed + held, got %d", got)
	}
	s.End()
}

// BenchmarkCheckBudget_HealthyParallel: the healthy path does not read the
// degraded counter, so parallel healthy checks do not contend on its lock.
func BenchmarkCheckBudget_HealthyParallel(b *testing.B) {
	gs, nc, _ := degradedRigLimit(b, 1, 0, limitNano)
	nc.mu.Lock()
	nc.readErr = nil
	nc.mu.Unlock()
	b.RunParallel(func(pb *testing.PB) {
		ctx := failmode.WithHolds(context.Background(), failmode.NewHolds())
		for pb.Next() {
			if _, err := gs.CheckBudget(ctx, testProject, testScope, testScopeID, testPeriod, 0); err != nil {
				b.Fatal(err)
			}
		}
	})
}
