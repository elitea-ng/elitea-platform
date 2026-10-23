package failmode

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
)

// TestTryReserve_ConcurrentAdmissionsAreBounded is the defect the reservation
// exists for: 200 concurrent admissions on a zero spend. Without an atomic
// reserve every one of them passes. With it, at most share/reservation do.
func TestTryReserve_ConcurrentAdmissionsAreBounded(t *testing.T) {
	d := NewDegradedCounters()
	const (
		share = 10 * NanoUSD
		r     = NanoUSD / 10 // 0.10 USD
		n     = 200
	)
	var admitted atomic.Int64
	holds := make(chan *Hold, n)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			if h, ok := d.TryReserve("k", r, share); ok {
				admitted.Add(1)
				holds <- h
			}
		}()
	}
	close(start)
	wg.Wait()
	close(holds)
	if got, want := admitted.Load(), int64(share/r); got != want {
		t.Fatalf("admitted %d of %d concurrent requests, want exactly %d (share/reservation)", got, n, want)
	}
	if got := d.Get("k"); got != share {
		t.Fatalf("held = %d, want the whole share %d", got, int64(share))
	}
	// Each admitted request bills its real cost, then releases its hold. The
	// spend stays within the share while every request costs at most r.
	for h := range holds {
		d.Add("k", r/2)
		h.Release()
		h.Release() // idempotent
	}
	if got := d.Held("k"); got != 0 {
		t.Fatalf("held after every release = %d, want 0", got)
	}
	if got := d.Get("k"); got != share/2 {
		t.Fatalf("spent = %d, want the billed %d", got, int64(share/2))
	}
}

// TestTryReserve_FleetOfReplicasStaysWithinCap: N replicas, each with its own
// counter and its share of the cap, admit concurrently. The total reserved
// over the fleet never passes the cap.
func TestTryReserve_FleetOfReplicasStaysWithinCap(t *testing.T) {
	const capNano = 7 * NanoUSD
	for _, n := range []int{1, 2, 3, 7} {
		share := PerReplicaShare(capNano, n)
		r := min(NanoUSD/10, share)
		var fleet atomic.Int64
		var wg sync.WaitGroup
		for range n {
			d := NewDegradedCounters()
			for range 500 {
				wg.Add(1)
				go func() {
					defer wg.Done()
					if _, ok := d.TryReserve("k", r, share); ok {
						fleet.Add(r)
					}
				}()
			}
		}
		wg.Wait()
		if fleet.Load() > capNano {
			t.Fatalf("n=%d: the fleet reserved %d > cap %d", n, fleet.Load(), int64(capNano))
		}
	}
}

func TestTryReserve_Refusals(t *testing.T) {
	d := NewDegradedCounters()
	if _, ok := d.TryReserve("k", 0, 10); ok {
		t.Fatal("a zero reservation must not succeed")
	}
	if _, ok := d.TryReserve("k", 1, 0); ok {
		t.Fatal("a zero share must not admit")
	}
	d.Add("k", 8)
	if _, ok := d.TryReserve("k", 3, 10); ok {
		t.Fatal("billed 8 + 3 > share 10 must refuse")
	}
	h, ok := d.TryReserve("k", 2, 10)
	if !ok {
		t.Fatal("billed 8 + 2 = share 10 must admit")
	}
	if _, ok := d.TryReserve("k", 1, 10); ok {
		t.Fatal("a full share must refuse")
	}
	h.Release()
	if _, ok := d.TryReserve("k", 1, 10); !ok {
		t.Fatal("a released hold must free its room")
	}
}

// TestHold_ReleaseAfterResetIsANoOp: recovery clears billed and held; a hold
// taken before the reset must not subtract from the next window.
func TestHold_ReleaseAfterResetIsANoOp(t *testing.T) {
	for _, reset := range []func(*DegradedCounters){
		func(d *DegradedCounters) { d.Reset("k") },
		func(d *DegradedCounters) { d.ResetAll() },
	} {
		d := NewDegradedCounters()
		h, ok := d.TryReserve("k", 5, 10)
		if !ok {
			t.Fatal("reserve")
		}
		reset(d)
		h2, ok := d.TryReserve("k", 4, 10)
		if !ok {
			t.Fatal("reserve after reset")
		}
		h.Release() // from the old window: must not free h2's room
		if got := d.Held("k"); got != 4 {
			t.Fatalf("held = %d after a stale release, want 4", got)
		}
		h2.Release()
		if got := d.Held("k"); got != 0 {
			t.Fatalf("held = %d, want 0", got)
		}
	}
}

func TestHolds_TakeAdoptAndBackstop(t *testing.T) {
	d := NewDegradedCounters()
	reserve := func() *Hold {
		h, ok := d.TryReserve("k", 1, 100)
		if !ok {
			t.Fatal("reserve")
		}
		return h
	}

	// The backstop releases a request that billed nothing.
	s := NewHolds()
	s.Add(reserve())
	s.Add(reserve())
	if s.HeldFor("k") != 2 || s.HeldFor("other") != 0 || s.Len() != 2 {
		t.Fatalf("HeldFor/Len wrong: %d %d", s.HeldFor("k"), s.Len())
	}
	s.ReleaseUnlessAdopted()
	if d.Held("k") != 0 {
		t.Fatalf("backstop left %d held", d.Held("k"))
	}

	// Take hands the holds over; the backstop then releases nothing.
	s = NewHolds()
	s.Add(reserve())
	taken := s.Take()
	s.ReleaseUnlessAdopted()
	if d.Held("k") != 1 {
		t.Fatalf("held = %d, want 1 while the biller owns the hold", d.Held("k"))
	}
	ReleaseHolds(taken)
	if d.Held("k") != 0 {
		t.Fatal("ReleaseHolds did not release")
	}

	// An adopted set survives the backstop; ReleaseAll releases it.
	s = NewHolds()
	s.Add(reserve())
	s.Adopt()
	s.ReleaseUnlessAdopted()
	if d.Held("k") != 1 {
		t.Fatal("the backstop released an adopted hold")
	}
	s.ReleaseAll()
	if d.Held("k") != 0 {
		t.Fatal("ReleaseAll did not release")
	}

	// A hold added after the set closed is released at once.
	s.Add(reserve())
	if d.Held("k") != 0 {
		t.Fatal("a late Add outlived its request")
	}

	// Nil receivers are safe.
	var nilSet *Holds
	nilSet.Add(reserve())
	nilSet.Adopt()
	nilSet.ReleaseAll()
	if nilSet.Take() != nil || nilSet.HeldFor("k") != 0 || nilSet.Len() != 0 {
		t.Fatal("nil set misbehaves")
	}
	if d.Held("k") != 0 {
		t.Fatal("Add on a nil set must release the hold")
	}
}

func TestHoldsContext(t *testing.T) {
	ctx := context.Background()
	if HoldsFrom(ctx) != nil || IsCheckOnly(ctx) {
		t.Fatal("empty context carries holds or check-only")
	}
	s := NewHolds()
	ctx = WithHolds(ctx, s)
	if HoldsFrom(ctx) != s {
		t.Fatal("WithHolds/HoldsFrom round trip")
	}
	if ctx2 := CheckOnly(ctx); !IsCheckOnly(ctx2) || HoldsFrom(ctx2) != s {
		t.Fatal("CheckOnly must keep the holds and mark the context")
	}
	if HoldsFrom(nil) != nil || IsCheckOnly(nil) { //nolint:staticcheck // nil context on purpose
		t.Fatal("nil context")
	}
}

// TestDecide_JoinedDuringOutageGetsNoShare: a replica that never saw NATS
// healthy answers 503 in the capped states, whatever its spend.
func TestDecide_JoinedDuringOutageGetsNoShare(t *testing.T) {
	p := tieredParams()
	p.JoinedDuringOutage = true
	for _, acc := range []int64{0, 85 * NanoUSD} { // SAFE and NEAR
		snap := Snapshot{HardLimitNano: 100 * NanoUSD, AccumulatedNano: acc, SoftAlertPct: 80, Found: true}
		d := Decide(false, 0, 0, snap, 0, p)
		if d.Verdict != Block503 || !d.Degraded || !d.Capped || d.ReplicaShareNano != 0 {
			t.Fatalf("accumulated=%d: want 503 with no share, got %+v", acc, d)
		}
	}
	// It does not affect an unlimited project or an explicit fail_open.
	if d := Decide(false, 0, 0, Snapshot{IsUnlimited: true}, 0, p); d.Verdict != Allow {
		t.Fatalf("unlimited must still allow: %+v", d)
	}
	// And NATS up is authoritative.
	if d := Decide(true, 0, 0, Snapshot{HardLimitNano: 100 * NanoUSD}, 0, p); d.Verdict != Allow {
		t.Fatalf("NATS up must decide on the counter: %+v", d)
	}
}

// TestDecide_UnknownCostAtShareRefuses: with reqCostNano=0 (every admission)
// a replica exactly AT its share refuses; one nano below admits.
func TestDecide_UnknownCostAtShareRefuses(t *testing.T) {
	p := tieredParams()
	p.ExpectedReplicas = 2
	p.DegradedCapNano = 10 * NanoUSD // share 5 USD
	snap := Snapshot{HardLimitNano: 100 * NanoUSD, AccumulatedNano: 0, SoftAlertPct: 80, Found: true}
	if d := Decide(false, 0, 5*NanoUSD, snap, 0, p); d.Verdict != Block402 {
		t.Fatalf("at the share with an unknown cost must refuse: %+v", d)
	}
	if d := Decide(false, 0, 5*NanoUSD-1, snap, 0, p); d.Verdict != Allow || d.ReplicaShareNano != 5*NanoUSD {
		t.Fatalf("below the share must admit and report the share: %+v", d)
	}
	// A known cost that fits exactly is admitted.
	if d := Decide(false, 0, 4*NanoUSD, snap, NanoUSD, p); d.Verdict != Allow {
		t.Fatalf("a known cost that fits exactly must admit: %+v", d)
	}
}
