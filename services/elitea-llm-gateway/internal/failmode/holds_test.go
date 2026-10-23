package failmode

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"
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
		r := int64(NanoUSD / 10)
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
	if _, ok := NewDegradedCounters().TryReserve("k", 11, 10); ok {
		t.Fatal("a reservation larger than the share must refuse, even on a zero spend")
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

// TestHolds_ReleaseOnTheLastUnpin: the holds go when the LAST pin drops,
// whatever the order. The request's own pin (End) and each biller's pin count
// the same.
func TestHolds_ReleaseOnTheLastUnpin(t *testing.T) {
	d := NewDegradedCounters()
	reserve := func() *Hold {
		h, ok := d.TryReserve("k", 1, 100)
		if !ok {
			t.Fatal("reserve")
		}
		return h
	}

	// A request that bills nothing: End releases.
	s := NewHolds()
	s.Add(reserve())
	s.Add(reserve())
	if s.HeldFor("k") != 2 || s.HeldFor("other") != 0 || s.Len() != 2 {
		t.Fatalf("HeldFor/Len wrong: %d %d", s.HeldFor("k"), s.Len())
	}
	s.End()
	s.End() // idempotent
	if d.Held("k") != 0 {
		t.Fatalf("End left %d held", d.Held("k"))
	}

	// A biller pinned the set: End keeps the holds until the bill lands.
	s = NewHolds()
	s.Add(reserve())
	unpin := s.Pin()
	s.End()
	if d.Held("k") != 1 {
		t.Fatalf("held = %d, want 1 while a bill is in flight", d.Held("k"))
	}
	unpin()
	unpin() // idempotent: must not drop another pin
	if d.Held("k") != 0 {
		t.Fatal("the last unpin did not release")
	}

	// The bill lands first; End releases.
	s = NewHolds()
	s.Add(reserve())
	unpin = s.Pin()
	unpin()
	if d.Held("k") != 1 {
		t.Fatal("an unpin released while the request still runs")
	}
	s.End()
	if d.Held("k") != 0 {
		t.Fatal("End after the bill did not release")
	}

	// After the set closed: a late Add releases at once, a late Pin is a no-op.
	s.Add(reserve())
	if d.Held("k") != 0 {
		t.Fatal("a late Add outlived its request")
	}
	s.Pin()()

	// Nil receivers are safe.
	var nilSet *Holds
	nilSet.Add(reserve())
	nilSet.Pin()()
	nilSet.End()
	if nilSet.HeldFor("k") != 0 || nilSet.Len() != 0 {
		t.Fatal("nil set misbehaves")
	}
	if d.Held("k") != 0 {
		t.Fatal("Add on a nil set must release the hold")
	}
}

// TestHolds_ConcurrentPinsAndEnd: many billers pin and unpin while the request
// ends. The holds are released exactly once, after the last unpin.
func TestHolds_ConcurrentPinsAndEnd(t *testing.T) {
	for range 200 {
		d := NewDegradedCounters()
		s := NewHolds()
		h, _ := d.TryReserve("k", 5, 10)
		s.Add(h)
		const billers = 8
		unpins := make([]func(), billers)
		for i := range unpins {
			unpins[i] = s.Pin()
		}
		var wg sync.WaitGroup
		for _, u := range unpins {
			wg.Add(1)
			go func() { defer wg.Done(); u() }()
		}
		wg.Add(1)
		go func() { defer wg.Done(); s.End() }()
		wg.Wait()
		if d.Held("k") != 0 {
			t.Fatalf("held = %d after every pin dropped", d.Held("k"))
		}
	}
}

// TestHoldsHeldFor_OnlyTheCurrentWindow: a Reset takes the hold off the
// counter, so HeldFor must stop counting it. Otherwise a re-check that spans
// two outages subtracts a hold the counter no longer has, and under-counts.
func TestHoldsHeldFor_OnlyTheCurrentWindow(t *testing.T) {
	for name, reset := range map[string]func(*DegradedCounters){
		"Reset":    func(d *DegradedCounters) { d.Reset("k") },
		"ResetAll": func(d *DegradedCounters) { d.ResetAll() },
	} {
		d := NewDegradedCounters()
		s := NewHolds()
		h, _ := d.TryReserve("k", 4, 10)
		s.Add(h)
		if got := s.HeldFor("k"); got != 4 {
			t.Fatalf("%s: HeldFor before the reset = %d, want 4", name, got)
		}
		reset(d)
		if got := s.HeldFor("k"); got != 0 {
			t.Fatalf("%s: HeldFor after the reset = %d, want 0 (the counter no longer has it)", name, got)
		}
		h.Release()
		if got := s.HeldFor("k"); got != 0 {
			t.Fatalf("%s: a released hold counted %d", name, got)
		}
	}
}

// TestDegradedCounters_BilledExcludesHolds: Get is billed + held (the gate),
// Billed is billed only (the dump).
func TestDegradedCounters_BilledExcludesHolds(t *testing.T) {
	d := NewDegradedCounters()
	d.Add("k", 3)
	if _, ok := d.TryReserve("k", 2, 10); !ok {
		t.Fatal("reserve")
	}
	if d.Get("k") != 5 || d.Billed("k") != 3 || d.Held("k") != 2 {
		t.Fatalf("get=%d billed=%d held=%d", d.Get("k"), d.Billed("k"), d.Held("k"))
	}
	if d.Billed("none") != 0 {
		t.Fatal("unknown key")
	}
}

// TestDegradedCounters_ReadsShareTheLock: Get, Held and Billed take the READ
// lock, so they do not wait for a reader that holds it.
func TestDegradedCounters_ReadsShareTheLock(t *testing.T) {
	d := NewDegradedCounters()
	d.Add("k", 1)
	d.mu.RLock()
	defer d.mu.RUnlock()
	done := make(chan struct{})
	go func() {
		_ = d.Get("k")
		_ = d.Held("k")
		_ = d.Billed("k")
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("a read waited for another reader: the counter takes the exclusive lock on reads")
	}
}

func BenchmarkDegradedCounters_GetParallel(b *testing.B) {
	d := NewDegradedCounters()
	d.Add("k", 1)
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			_ = d.Get("k")
		}
	})
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
