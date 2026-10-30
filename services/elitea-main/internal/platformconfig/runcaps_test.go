package platformconfig

import (
	"testing"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/agentlifecycle"
)

// Unsaved, every tier has the ADR default: 200 tool calls and no other cap,
// and no per-tier key is stored (Main sends no field 69).
func TestRunCapsDefaultsChangeNothingButToolCalls(t *testing.T) {
	caps := RunCapsFromValues(Values{KeyRunMaxWallSeconds: float64(600)})
	if caps.TierCapsStored {
		t.Fatal("the wall cap alone stored per-tier caps")
	}
	for _, tier := range agentlifecycle.Tiers {
		if got := caps.For(tier); got != (TierRunCaps{MaxToolCalls: DefaultRunMaxToolCalls}) {
			t.Fatalf("%s default = %+v", tier, got)
		}
	}
	if caps.For("bogus") != DefaultTierRunCaps() {
		t.Fatal("an unknown tier did not read as low")
	}
}

func TestRunCapsReadPerTier(t *testing.T) {
	caps := RunCapsFromValues(Values{
		"high_max_tool_calls":   float64(50),
		"high_max_model_turns":  float64(30),
		"high_max_spend":        2.5,
		"high_max_records_read": float64(1000),
		"low_max_tool_calls":    float64(0),
	})
	if !caps.TierCapsStored {
		t.Fatal("per-tier keys were not reported stored")
	}
	if got := caps.For(agentlifecycle.TierHigh); got != (TierRunCaps{MaxToolCalls: 50, MaxModelTurns: 30, MaxSpend: 2.5, MaxRecordsRead: 1000}) {
		t.Fatalf("high = %+v", got)
	}
	if got := caps.For(agentlifecycle.TierLow); got.MaxToolCalls != 0 {
		t.Fatalf("an explicit 0 is no cap, got %+v", got)
	}
	if got := caps.For(agentlifecycle.TierMedium); got != DefaultTierRunCaps() {
		t.Fatalf("an unsaved tier kept %+v", got)
	}
}

// A hand-written value that does not read tightens, never removes a cap.
func TestRunCapsUnreadableValueIsTightest(t *testing.T) {
	caps := RunCapsFromValues(Values{
		"critical_max_tool_calls":   "many",
		"critical_max_spend":        -3.0,
		"critical_max_model_turns":  float64(1e12),
		"critical_max_records_read": 2.6,
	})
	got := caps.For(agentlifecycle.TierCritical)
	want := TierRunCaps{MaxToolCalls: 1, MaxSpend: 0.0001, MaxModelTurns: MaxRunCapModelTurns, MaxRecordsRead: 2}
	if got != want {
		t.Fatalf("got %+v, want %+v", got, want)
	}
}

func TestTierRunCapsLowerAndRaise(t *testing.T) {
	tier := TierRunCaps{MaxToolCalls: 200, MaxSpend: 5}
	author := TierRunCaps{MaxToolCalls: 50, MaxModelTurns: 10, MaxSpend: 9}
	if got := tier.Lower(author); got != (TierRunCaps{MaxToolCalls: 50, MaxModelTurns: 10, MaxSpend: 5}) {
		t.Fatalf("lower = %+v", got)
	}
	raised := TierRunCaps{MaxToolCalls: 50, MaxModelTurns: 10}.Raised(RunCapMaxToolCalls)
	if raised != (TierRunCaps{MaxToolCalls: 100, MaxModelTurns: 10}) {
		t.Fatalf("raised = %+v", raised)
	}
	if off := (TierRunCaps{}).Raised(RunCapMaxSpend); off.MaxSpend != 0 {
		t.Fatal("raising an off cap turned it on")
	}
}

func TestValidRunCap(t *testing.T) {
	for _, tc := range []struct {
		name  string
		value any
		want  bool
	}{
		{RunCapMaxToolCalls, float64(200), true},
		{RunCapMaxToolCalls, 2.5, false},
		{RunCapMaxToolCalls, float64(MaxRunCapToolCalls + 1), false},
		{RunCapMaxSpend, 1.2345, true},
		{RunCapMaxSpend, 1.23456, false},
		{RunCapMaxSpend, "1", false},
		{RunCapMaxRecordsRead, float64(0), true},
		{RunCapMaxModelTurns, float64(-1), false},
	} {
		if got := ValidRunCap(tc.name, tc.value); got != tc.want {
			t.Errorf("ValidRunCap(%s, %v) = %v", tc.name, tc.value, got)
		}
	}
}
