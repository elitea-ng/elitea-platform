package failmode

import (
	"testing"
	"time"
)

func TestResolveFailMode(t *testing.T) {
	tests := []struct {
		name       string
		perProject string
		baseline   FailMode
		want       FailMode
	}{
		{"empty inherits baseline", "", ModeTieredHybrid, ModeTieredHybrid},
		{"valid override wins", "fail_open", ModeTieredHybrid, ModeFailOpen},
		{"fail_closed override wins", "fail_closed", ModeFailOpen, ModeFailClosed},
		{"tiered_hybrid override", "tiered_hybrid", ModeFailClosed, ModeTieredHybrid},
		{"typo falls back to baseline", "fail-open", ModeFailClosed, ModeFailClosed},
		{"garbage falls back", "nonsense", ModeTieredHybrid, ModeTieredHybrid},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := ResolveFailMode(tc.perProject, tc.baseline); got != tc.want {
				t.Fatalf("ResolveFailMode(%q,%v) = %v, want %v", tc.perProject, tc.baseline, got, tc.want)
			}
		})
	}
}

func TestStateString(t *testing.T) {
	cases := map[State]string{
		StateNATSHealthy:     "NATS_HEALTHY",
		StateDownPGFreshSafe: "NATS_DOWN_PG_FRESH_SAFE",
		StateDownPGFreshNear: "NATS_DOWN_PG_FRESH_NEAR",
		StateDownPGFreshOver: "NATS_DOWN_PG_FRESH_OVER",
		StateDownPGStale:     "NATS_DOWN_PG_STALE",
		StateForcedClosed:    "NATS_DOWN_FORCED_CLOSED",
		State(99):            "UNKNOWN",
	}
	for st, want := range cases {
		if got := st.String(); got != want {
			t.Errorf("State(%d).String() = %q, want %q", st, got, want)
		}
	}
}

// tieredParams is a baseline tiered_hybrid Params with a 5m freshness window,
// 1 expected replica, and no absolute degraded cap.
func tieredParams() Params {
	return Params{
		Mode:             ModeTieredHybrid,
		PGFreshness:      5 * time.Minute,
		ExpectedReplicas: 1,
	}
}

func TestDecide_Healthy(t *testing.T) {
	limit := int64(100) * NanoUSD
	snap := Snapshot{HardLimitNano: limit, Found: true}

	t.Run("under limit allows", func(t *testing.T) {
		d := Decide(true, limit-1, 0, snap, 0, tieredParams())
		if d.Verdict != Allow || d.State != StateNATSHealthy || d.Degraded {
			t.Fatalf("got %+v", d)
		}
	})
	t.Run("at limit blocks 402", func(t *testing.T) {
		d := Decide(true, limit, 0, snap, 0, tieredParams())
		if d.Verdict != Block402 || d.State != StateNATSHealthy {
			t.Fatalf("got %+v", d)
		}
	})
	t.Run("unlimited always allows", func(t *testing.T) {
		d := Decide(true, limit*10, 0, Snapshot{IsUnlimited: true}, 0, tieredParams())
		if d.Verdict != Allow || d.State != StateNATSHealthy {
			t.Fatalf("got %+v", d)
		}
	})
}

func TestDecide_ForcedClosedOverridesEverything(t *testing.T) {
	p := tieredParams()
	p.OutageExceededMax = true

	// Even unlimited + fail_open must be 503 under FORCED_CLOSED.
	p.Mode = ModeFailOpen
	d := Decide(false, 0, 0, Snapshot{IsUnlimited: true}, 0, p)
	if d.Verdict != Block503 || d.State != StateForcedClosed || !d.Degraded {
		t.Fatalf("forced-closed did not override fail_open/unlimited: %+v", d)
	}
}

func TestDecide_ExplicitModes(t *testing.T) {
	snap := Snapshot{HardLimitNano: 100 * NanoUSD, Found: true}

	t.Run("fail_closed blocks 402 while down", func(t *testing.T) {
		p := tieredParams()
		p.Mode = ModeFailClosed
		d := Decide(false, 0, 0, snap, 0, p)
		if d.Verdict != Block402 || !d.Degraded {
			t.Fatalf("got %+v", d)
		}
		// State must be the policy/budget block label, not the infra-staleness one.
		if d.State != StateDownPGFreshOver {
			t.Fatalf("fail_closed state = %v, want StateDownPGFreshOver", d.State)
		}
	})
	t.Run("fail_open allows while down", func(t *testing.T) {
		p := tieredParams()
		p.Mode = ModeFailOpen
		d := Decide(false, 0, 0, snap, 0, p)
		if d.Verdict != Allow || !d.Degraded {
			t.Fatalf("got %+v", d)
		}
	})
}

func TestDecide_UnlimitedWhileDown(t *testing.T) {
	d := Decide(false, 0, 0, Snapshot{IsUnlimited: true}, 0, tieredParams())
	if d.Verdict != Allow || d.State != StateDownPGFreshSafe || !d.Degraded {
		t.Fatalf("got %+v", d)
	}
}

func TestDecide_StaleSnapshot(t *testing.T) {
	p := tieredParams()
	snap := Snapshot{
		HardLimitNano:   100 * NanoUSD,
		AccumulatedNano: 10 * NanoUSD,
		Found:           true,
		Age:             6 * time.Minute, // ≥ 5m freshness
	}
	d := Decide(false, 0, 0, snap, 0, p)
	if d.Verdict != Block503 || d.State != StateDownPGStale || !d.Degraded {
		t.Fatalf("stale snapshot should 503: %+v", d)
	}
}

func TestDecide_MissingRowIsFreshZero(t *testing.T) {
	// Found=false ⇒ no spend yet this period ⇒ treated fresh even with a large Age.
	p := tieredParams()
	snap := Snapshot{
		HardLimitNano: 100 * NanoUSD,
		Found:         false,
		Age:           time.Hour,
	}
	d := Decide(false, 0, 0, snap, 0, p)
	if d.Verdict != Allow || d.State != StateDownPGFreshSafe {
		t.Fatalf("missing row should be fresh-safe allow: %+v", d)
	}
}

func TestDecide_FreshOver(t *testing.T) {
	p := tieredParams()
	snap := Snapshot{
		HardLimitNano:   100 * NanoUSD,
		AccumulatedNano: 100 * NanoUSD, // at limit
		Found:           true,
	}
	d := Decide(false, 0, 0, snap, 0, p)
	if d.Verdict != Block402 || d.State != StateDownPGFreshOver {
		t.Fatalf("at-limit while down should 402 over: %+v", d)
	}
}

func TestDecide_FreshNear_PerReplicaCap(t *testing.T) {
	// limit=100, accumulated=90 (≥80% soft), remaining=10 split across 2 replicas
	// ⇒ per-replica cap = 5.
	p := tieredParams()
	p.ExpectedReplicas = 2
	snap := Snapshot{
		HardLimitNano:   100 * NanoUSD,
		AccumulatedNano: 90 * NanoUSD,
		SoftAlertPct:    80,
		Found:           true,
	}
	t.Run("under per-replica cap allows", func(t *testing.T) {
		d := Decide(false, 0, 4*NanoUSD /*already billed*/, snap, 1*NanoUSD /*req*/, p)
		if d.Verdict != Allow || d.State != StateDownPGFreshNear {
			t.Fatalf("got %+v", d)
		}
	})
	t.Run("over per-replica cap blocks 402", func(t *testing.T) {
		d := Decide(false, 0, 4*NanoUSD, snap, 2*NanoUSD, p) // 4+2=6 > cap 5
		if d.Verdict != Block402 || d.State != StateDownPGFreshNear {
			t.Fatalf("got %+v", d)
		}
	})
}

func TestDecide_FreshNear_DefaultSoftPct(t *testing.T) {
	// soft_alert_pct=0 ⇒ treated as 80%. accumulated=80 sits exactly on the
	// NEAR boundary.
	p := tieredParams()
	snap := Snapshot{
		HardLimitNano:   100 * NanoUSD,
		AccumulatedNano: 80 * NanoUSD,
		SoftAlertPct:    0,
		Found:           true,
	}
	d := Decide(false, 0, 0, snap, 0, p)
	if d.State != StateDownPGFreshNear {
		t.Fatalf("80%% with default soft pct should be NEAR: %+v", d)
	}
}

func TestDecide_FreshSafe_DegradedCap(t *testing.T) {
	// Below soft threshold: allow, but bounded by an absolute degraded cap.
	p := tieredParams()
	p.DegradedCapNano = 5 * NanoUSD
	snap := Snapshot{
		HardLimitNano:   100 * NanoUSD,
		AccumulatedNano: 10 * NanoUSD, // well below 80
		SoftAlertPct:    80,
		Found:           true,
	}
	t.Run("under cap allows", func(t *testing.T) {
		d := Decide(false, 0, 4*NanoUSD, snap, 1*NanoUSD, p) // 4+1=5, not > 5
		if d.Verdict != Allow || d.State != StateDownPGFreshSafe {
			t.Fatalf("got %+v", d)
		}
	})
	t.Run("over cap blocks 402", func(t *testing.T) {
		d := Decide(false, 0, 5*NanoUSD, snap, 1*NanoUSD, p) // 6 > 5
		if d.Verdict != Block402 || d.State != StateDownPGFreshSafe {
			t.Fatalf("got %+v", d)
		}
	})
	t.Run("no explicit cap defaults to 10% of limit", func(t *testing.T) {
		// Design §8.5: when DegradedCapNano==0 the cap defaults to 10% of
		// HardLimitNano. With limit=100 USD the default cap is 10 USD.
		p2 := tieredParams() // DegradedCapNano = 0 → 10% of limit
		// degraded=9 USD + req=1 USD = 10 = exactly at the 10% cap → allowed.
		d := Decide(false, 0, 9*NanoUSD, snap, 1*NanoUSD, p2)
		if d.Verdict != Allow || d.State != StateDownPGFreshSafe {
			t.Fatalf("at 10%% cap should allow: %+v", d)
		}
		// degraded=10 USD + req=1 USD = 11 > 10% cap → block.
		d = Decide(false, 0, 10*NanoUSD, snap, 1*NanoUSD, p2)
		if d.Verdict != Block402 || d.State != StateDownPGFreshSafe {
			t.Fatalf("over 10%% cap should block: %+v", d)
		}
	})
}

// TestDecide_FreshSafe_DefaultDegradedCap_SmallLimit exercises FIX 1 (fsm.go:275)
// for small limits: the 10% default is limit/10 (integer), so a 50 nano-USD limit
// yields a 5 nano-USD cap. Guard against the "cap accidentally 0" edge case.
func TestDecide_FreshSafe_DefaultDegradedCap_SmallLimit(t *testing.T) {
	// limit = 50 nano-USD (deliberately tiny: 10% = 5 nano-USD).
	snap := Snapshot{
		HardLimitNano:   50,
		AccumulatedNano: 0,
		SoftAlertPct:    80, // threshold = 40; accumulated=0 → SAFE branch
		Found:           true,
	}
	p := tieredParams() // DegradedCapNano = 0 → default to 10% = 5
	// degraded=4 + req=1 = 5 — at cap, not over — must allow.
	if d := Decide(false, 0, 4, snap, 1, p); d.Verdict != Allow {
		t.Fatalf("small limit at cap: expected Allow, got %v", d.Verdict)
	}
	// degraded=4 + req=2 = 6 > 5 — must block.
	if d := Decide(false, 0, 4, snap, 2, p); d.Verdict != Block402 {
		t.Fatalf("small limit over cap: expected Block402, got %v", d.Verdict)
	}
}

// TestDecide_FreshSafe_DefaultDegradedCap_LargeLimit exercises FIX 1 for a $100M
// limit (largest realistic NUMERIC(12,2) budget): the 10% default cap is $10M.
func TestDecide_FreshSafe_DefaultDegradedCap_LargeLimit(t *testing.T) {
	const limitNano = int64(100_000_000) * NanoUSD // $100M
	snap := Snapshot{
		HardLimitNano:   limitNano,
		AccumulatedNano: 0,
		SoftAlertPct:    80,
		Found:           true,
	}
	p := tieredParams()       // DegradedCapNano = 0 → default to 10% = $10M nano
	capNano := limitNano / 10 // $10M
	// One nano below the cap with no cost estimate: allow.
	if d := Decide(false, 0, capNano-1, snap, 0, p); d.Verdict != Allow {
		t.Fatalf("large limit below 10%% cap should allow: %+v", d)
	}
	// Exactly AT the cap with no cost estimate: nothing is left, so refuse
	// (an unknown cost uses spent >= share).
	if d := Decide(false, 0, capNano, snap, 0, p); d.Verdict != Block402 {
		t.Fatalf("large limit at 10%% cap with an unknown cost should block: %+v", d)
	}
	// One nano over the cap: must block.
	if d := Decide(false, 0, capNano, snap, 1, p); d.Verdict != Block402 {
		t.Fatalf("large limit over 10%% cap should block: %+v", d)
	}
}

// TestDecide_SoftThresholdSmallLimit exercises FIX 1: with a limit smaller than
// 100 nanoUSD the old limit/100*pct formula yielded 0 (integer truncation),
// putting every request into FRESH_NEAR even with zero spend. The corrected
// limit*pct/100 formula produces the right threshold.
func TestDecide_SoftThresholdSmallLimit(t *testing.T) {
	// limit=50 nanoUSD, softPct=80 → threshold should be 40 (50*80/100=40).
	// With the OLD formula: 50/100*80 = 0*80 = 0, so accumulated=1 ≥ 0 → NEAR.
	// With the CORRECT formula: 50*80/100 = 4000/100 = 40, so accumulated=1 < 40 → SAFE.
	const limit int64 = 50 // 50 nanoUSD — intentionally sub-100
	p := tieredParams()
	snap := Snapshot{
		HardLimitNano:   limit,
		AccumulatedNano: 1, // 1 nanoUSD — well below the 80% threshold of 40
		SoftAlertPct:    80,
		Found:           true,
	}
	d := Decide(false, 0, 0, snap, 0, p)
	if d.State != StateDownPGFreshSafe {
		t.Fatalf("small-limit threshold regression: expected FRESH_SAFE got %v (accumulated=1 limit=50 pct=80 threshold=40)", d.State)
	}
	if d.Verdict != Allow {
		t.Fatalf("small-limit threshold: expected Allow got %v", d.Verdict)
	}

	// Also confirm that spend at exactly the threshold (40) lands in NEAR.
	snap.AccumulatedNano = 40
	d = Decide(false, 0, 0, snap, 0, p)
	if d.State != StateDownPGFreshNear {
		t.Fatalf("at threshold (40) should be FRESH_NEAR, got %v", d.State)
	}
}

// TestDecide_FailClosedState exercises FIX 2: ModeFailClosed must return a
// policy/budget State constant (StateDownPGFreshOver), not the infra-staleness
// label StateDownPGStale — which would pollute metrics and alarms.
func TestDecide_FailClosedState(t *testing.T) {
	p := tieredParams()
	p.Mode = ModeFailClosed
	snap := Snapshot{HardLimitNano: 100 * NanoUSD, Found: true}
	d := Decide(false, 0, 0, snap, 0, p)
	if d.Verdict != Block402 {
		t.Fatalf("fail_closed must Block402, got %v", d.Verdict)
	}
	if d.State == StateDownPGStale {
		t.Fatalf("fail_closed must NOT use StateDownPGStale (infra label); got StateDownPGStale")
	}
	if d.State != StateDownPGFreshOver {
		t.Fatalf("fail_closed must use StateDownPGFreshOver (policy block), got %v", d.State)
	}
	if !d.Degraded {
		t.Fatal("fail_closed decision must be marked Degraded")
	}
}

func TestDecide_NonPositiveLimitAllows(t *testing.T) {
	// Budgeted (not unlimited) but limit ≤ 0 is a config error: allow in SAFE
	// rather than 402 every request.
	p := tieredParams()
	snap := Snapshot{HardLimitNano: 0, Found: true}
	d := Decide(false, 0, 0, snap, 0, p)
	if d.Verdict != Allow || d.State != StateDownPGFreshSafe {
		t.Fatalf("got %+v", d)
	}
}

func TestDecide_ExpectedReplicasFloor(t *testing.T) {
	// ExpectedReplicas < 1 must floor to 1 (no divide-by-zero).
	p := tieredParams()
	p.ExpectedReplicas = 0
	snap := Snapshot{
		HardLimitNano:   100 * NanoUSD,
		AccumulatedNano: 90 * NanoUSD,
		SoftAlertPct:    80,
		Found:           true,
	}
	// remaining=10, N floored to 1 ⇒ cap 10; req 10 allowed, 11 blocked.
	if d := Decide(false, 0, 0, snap, 10*NanoUSD, p); d.Verdict != Allow {
		t.Fatalf("req at cap should allow: %+v", d)
	}
	if d := Decide(false, 0, 0, snap, 11*NanoUSD, p); d.Verdict != Block402 {
		t.Fatalf("req over cap should 402: %+v", d)
	}
}
