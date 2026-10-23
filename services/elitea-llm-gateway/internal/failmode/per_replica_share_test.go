package failmode

import "testing"

// TestPerReplicaShare pins the rounding of the fleet-wide degraded allowance
// (human decision, DECISIONS.md 2026-10-22): FLOOR, minimum 0.
func TestPerReplicaShare(t *testing.T) {
	cases := []struct {
		total    int64
		replicas int
		want     int64
	}{
		{10 * NanoUSD, 1, 10 * NanoUSD},
		{10 * NanoUSD, 2, 5 * NanoUSD},
		{10 * NanoUSD, 3, 3333333333}, // floor of 3.333... USD
		{10 * NanoUSD, 7, 1428571428}, // floor of 1.428... USD
		{100, 7, 14},
		{5, 7, 0}, // a cap below the replica count: no degraded spend
		{1, 2, 0},
		{0, 3, 0},
		{-5, 3, 0},
		{10, 0, 10},  // a replica count below 1 is read as 1
		{10, -4, 10}, // never a divide by zero
	}
	for _, tc := range cases {
		if got := PerReplicaShare(tc.total, tc.replicas); got != tc.want {
			t.Errorf("PerReplicaShare(%d, %d) = %d, want %d", tc.total, tc.replicas, got, tc.want)
		}
	}
}

// TestPerReplicaShare_FleetNeverExceedsTotal is the property the rounding
// exists for: N replicas that each spend their full share stay at or below the
// fleet allowance, for every N and every total.
func TestPerReplicaShare_FleetNeverExceedsTotal(t *testing.T) {
	totals := []int64{1, 2, 3, 5, 6, 7, 9, 10, 99, 100, 101, 1000003, 10 * NanoUSD, 92 * 1_000_000 * NanoUSD}
	for _, total := range totals {
		for n := 1; n <= 64; n++ {
			share := PerReplicaShare(total, n)
			if share < 0 {
				t.Fatalf("PerReplicaShare(%d, %d) = %d, want >= 0", total, n, share)
			}
			if fleet := share * int64(n); fleet > total {
				t.Fatalf("PerReplicaShare(%d, %d) = %d: fleet spends %d > cap %d", total, n, share, fleet, total)
			}
			// Floor loses less than one nano-USD per replica.
			if waste := total - share*int64(n); waste >= int64(n) {
				t.Fatalf("PerReplicaShare(%d, %d) = %d wastes %d >= %d replicas", total, n, share, waste, n)
			}
		}
	}
}

// TestDecide_FreshSafe_DegradedCapDividedByReplicas proves the FRESH_SAFE cap is
// the FLEET bound: each replica admits floor(cap / ExpectedReplicas), and a
// simulated fleet that spends every admitted nano stays within the cap.
func TestDecide_FreshSafe_DegradedCapDividedByReplicas(t *testing.T) {
	snap := Snapshot{
		HardLimitNano:   100 * NanoUSD,
		AccumulatedNano: 10 * NanoUSD, // FRESH_SAFE
		SoftAlertPct:    80,
		Found:           true,
	}
	for _, n := range []int{1, 2, 3, 7} {
		for _, explicit := range []bool{true, false} {
			p := tieredParams()
			p.ExpectedReplicas = n
			fleetCap := int64(10 * NanoUSD) // the 10% default of a 100 USD limit
			if explicit {
				p.DegradedCapNano = 7 * NanoUSD
				fleetCap = p.DegradedCapNano
			}
			share := fleetCap / int64(n)
			if d := Decide(false, 0, 0, snap, share, p); d.Verdict != Allow || d.State != StateDownPGFreshSafe {
				t.Fatalf("n=%d explicit=%v: a request of exactly the share %d must pass: %+v", n, explicit, share, d)
			}
			if d := Decide(false, 0, share, snap, 1, p); d.Verdict != Block402 || d.State != StateDownPGFreshSafe {
				t.Fatalf("n=%d explicit=%v: one nano-USD past the share %d must 402: %+v", n, explicit, share, d)
			}
			// Each replica admits requests until its own counter refuses. Each
			// replica sees only its own counter, as in production.
			var fleet int64
			step := max(1, share/1000)
			for r := 0; r < n; r++ {
				var spent int64
				for Decide(false, 0, spent, snap, step, p).Verdict == Allow {
					spent += step
				}
				fleet += spent
			}
			if fleet > fleetCap {
				t.Fatalf("n=%d explicit=%v: fleet admitted %d > cap %d", n, explicit, fleet, fleetCap)
			}
			if n > 1 && fleet <= fleetCap/int64(n) {
				t.Fatalf("n=%d explicit=%v: fleet admitted only %d; the simulation did not run", n, explicit, fleet)
			}
		}
	}
}

// TestDecide_FreshSafe_CapBelowReplicaCount: a cap smaller than the replica
// count gives each replica a share of 0. The replica then refuses every priced
// request; the cap is not disabled.
func TestDecide_FreshSafe_CapBelowReplicaCount(t *testing.T) {
	p := tieredParams()
	p.ExpectedReplicas = 7
	snap := Snapshot{
		HardLimitNano:   50, // default cap max(1, 50/10) = 5 nano-USD < 7 replicas
		AccumulatedNano: 0,
		SoftAlertPct:    80,
		Found:           true,
	}
	if d := Decide(false, 0, 0, snap, 1, p); d.Verdict != Block402 || d.State != StateDownPGFreshSafe {
		t.Fatalf("share 0 must refuse a 1 nano-USD request: %+v", d)
	}
	// Production admission passes reqCostNano=0 (the cost is not known yet).
	// A share of 0 must still refuse the FIRST request, or each replica would
	// admit one unbounded request.
	if d := Decide(false, 0, 0, snap, 0, p); d.Verdict != Block402 || d.State != StateDownPGFreshSafe {
		t.Fatalf("share 0 must refuse an admission check with no cost estimate: %+v", d)
	}
	p.DegradedCapNano = 6 // an explicit cap, still below 7 replicas
	if d := Decide(false, 0, 0, snap, 0, p); d.Verdict != Block402 || d.State != StateDownPGFreshSafe {
		t.Fatalf("an explicit cap below the replica count must refuse: %+v", d)
	}
	p.DegradedCapNano = 7 // exactly one nano-USD per replica: admits again
	if d := Decide(false, 0, 0, snap, 0, p); d.Verdict != Allow {
		t.Fatalf("a share of 1 nano-USD must admit an unspent replica: %+v", d)
	}
}

// TestDecide_FreshNear_ShareBelowOneNano: the FRESH_NEAR remaining budget is
// divided the same way, and a share of 0 refuses even with no cost estimate.
func TestDecide_FreshNear_ShareBelowOneNano(t *testing.T) {
	p := tieredParams()
	p.ExpectedReplicas = 7
	snap := Snapshot{
		HardLimitNano:   100,
		AccumulatedNano: 95, // NEAR; 5 nano-USD remain for 7 replicas
		SoftAlertPct:    80,
		Found:           true,
	}
	if d := Decide(false, 0, 0, snap, 0, p); d.Verdict != Block402 || d.State != StateDownPGFreshNear {
		t.Fatalf("a NEAR share of 0 must refuse: %+v", d)
	}
	p.ExpectedReplicas = 5 // one nano-USD each
	if d := Decide(false, 0, 0, snap, 0, p); d.Verdict != Allow || d.State != StateDownPGFreshNear {
		t.Fatalf("a NEAR share of 1 must admit an unspent replica: %+v", d)
	}
}
