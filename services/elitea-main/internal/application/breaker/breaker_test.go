package breaker

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/safety"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/agentlifecycle"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/platformconfig"
)

func ptr[T any](v T) *T { return &v }

func outcomes(failed, total int) []string {
	out := make([]string, 0, total)
	for i := range total {
		if i < failed {
			out = append(out, StateFailed)
		} else {
			out = append(out, StateSucceeded)
		}
	}
	return out
}

func TestDefaultSettings(t *testing.T) {
	s := DefaultSettings()
	if !s.Enabled || s.StormLimit != DefaultStormLimit {
		t.Fatalf("enabled=%v storm=%d", s.Enabled, s.StormLimit)
	}
	for tier, want := range map[agentlifecycle.Tier]safety.ResumeRequirement{
		agentlifecycle.TierLow: safety.ResumeAnyAdmin, agentlifecycle.TierMedium: safety.ResumeAnyAdmin,
		agentlifecycle.TierHigh: safety.ResumeGovernanceRole, agentlifecycle.TierCritical: safety.ResumeTwoPerson,
	} {
		got := s.Tier(tier)
		if got.ResumeRequires != want {
			t.Errorf("%s resume = %s, want %s", tier, got.ResumeRequires, want)
		}
		if got.Rules.ErrorRatePercent != 50 || got.Rules.ErrorRateWindowRuns != 20 || got.Rules.ToolFailureStreak != 5 ||
			got.Rules.SpendPerHourUSD != 0 || got.Rules.RunsPerHour != 0 {
			t.Errorf("%s rules = %+v", tier, got.Rules)
		}
	}
}

func TestSettingsFromValues(t *testing.T) {
	s := SettingsFromValues(platformconfig.Values{
		"enabled":                       false,
		"error_rate_window_runs":        float64(10),
		"storm_limit":                   float64(0),
		"high_error_rate_percent":       float64(25),
		"high_spend_per_hour_usd":       12.5,
		"critical_tool_failure_streak":  float64(0),
		"low_runs_per_hour":             float64(300),
		"medium_resume_requires":        "two_person",
		"low_error_rate_percent":        float64(150), // out of range: default kept
		"medium_tool_failure_streak":    "x",          // wrong type: default kept
		"critical_resume_requires":      "anyone",     // unknown: default kept
		"medium_spend_per_hour_usd":     -1.0,         // negative: default kept
		"error_rate_window_runs_ignore": float64(1),
	})
	if s.Enabled || s.StormLimit != 0 {
		t.Fatalf("enabled=%v storm=%d", s.Enabled, s.StormLimit)
	}
	if got := s.Tier(agentlifecycle.TierHigh).Rules; got.ErrorRatePercent != 25 || got.SpendPerHourUSD != 12.5 || got.ErrorRateWindowRuns != 10 {
		t.Errorf("high = %+v", got)
	}
	if got := s.Tier(agentlifecycle.TierCritical); got.Rules.ToolFailureStreak != 0 || got.ResumeRequires != safety.ResumeTwoPerson {
		t.Errorf("critical = %+v", got)
	}
	if got := s.Tier(agentlifecycle.TierLow).Rules; got.RunsPerHour != 300 || got.ErrorRatePercent != 50 {
		t.Errorf("low = %+v", got)
	}
	if got := s.Tier(agentlifecycle.TierMedium); got.ResumeRequires != safety.ResumeTwoPerson || got.Rules.ToolFailureStreak != 5 || got.Rules.SpendPerHourUSD != 0 {
		t.Errorf("medium = %+v", got)
	}
}

func TestValidateValues(t *testing.T) {
	for _, tc := range []struct {
		values map[string]any
		want   string
	}{
		{map[string]any{"enabled": true, "error_rate_window_runs": float64(20), "high_error_rate_percent": float64(0)}, ""},
		{map[string]any{"enabled": "yes"}, `"enabled"`},
		{map[string]any{"error_rate_window_runs": float64(2)}, `"error_rate_window_runs" must be from 5 to 200`},
		{map[string]any{"error_rate_window_runs": 20.5}, "whole number"},
		{map[string]any{"low_error_rate_percent": float64(100)}, `"low_error_rate_percent" must be 0 (off) or from 1 to 99`},
		{map[string]any{"critical_tool_failure_streak": float64(1)}, "critical_tool_failure_streak"},
		{map[string]any{"medium_runs_per_hour": float64(-3)}, "medium_runs_per_hour"},
		{map[string]any{"high_spend_per_hour_usd": "10"}, "high_spend_per_hour_usd"},
		{map[string]any{"high_resume_requires": "owner"}, "high_resume_requires"},
		{map[string]any{"storm_limit": float64(-1)}, "storm_limit"},
	} {
		got := ValidateValues(tc.values)
		if (tc.want == "") != (got == "") || (tc.want != "" && !strings.Contains(got, tc.want)) {
			t.Errorf("ValidateValues(%v) = %q, want %q", tc.values, got, tc.want)
		}
	}
}

func TestOverrideTightenOnly(t *testing.T) {
	tier := DefaultSettings().Tier(agentlifecycle.TierHigh).Rules
	for _, tc := range []struct {
		name  string
		raw   string
		loose []string
		bad   bool
	}{
		{name: "empty", raw: `{}`},
		{name: "null", raw: `null`},
		{name: "tighter", raw: `{"error_rate_percent":30,"error_rate_window_runs":10,"tool_failure_streak":3}`},
		{name: "enable an off rule", raw: `{"spend_per_hour_usd":5,"runs_per_hour":100}`},
		{name: "looser percent", raw: `{"error_rate_percent":80}`, loose: []string{"error_rate_percent"}},
		{name: "turn off", raw: `{"error_rate_percent":0,"tool_failure_streak":0}`, loose: []string{"error_rate_percent", "tool_failure_streak"}},
		{name: "wider window", raw: `{"error_rate_window_runs":50}`, loose: []string{"error_rate_window_runs"}},
		{name: "unknown key", raw: `{"error_rate":30}`, bad: true},
		{name: "out of range", raw: `{"tool_failure_streak":1}`, bad: true},
		{name: "not an object", raw: `[1]`, bad: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			override, err := ParseOverride([]byte(tc.raw))
			if tc.bad {
				if !errors.Is(err, ErrInvalidOverride) {
					t.Fatalf("err = %v, want ErrInvalidOverride", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			err = CheckOverride(tier, override)
			var loose *LooseningError
			if tc.loose == nil {
				if err != nil {
					t.Fatalf("err = %v", err)
				}
				return
			}
			if !errors.As(err, &loose) || strings.Join(loose.Fields, ",") != strings.Join(tc.loose, ",") {
				t.Fatalf("err = %v, want loosening of %v", err, tc.loose)
			}
		})
	}
	// Tighter of the two, whichever side moved later.
	got := Effective(Rules{ErrorRatePercent: 50, ErrorRateWindowRuns: 20, ToolFailureStreak: 5, SpendPerHourUSD: 10},
		Override{ErrorRatePercent: ptr(70), SpendPerHourUSD: ptr(4.0), RunsPerHour: ptr(9), ErrorRateWindowRuns: ptr(10)})
	if got.ErrorRatePercent != 50 || got.SpendPerHourUSD != 4 || got.RunsPerHour != 9 || got.ErrorRateWindowRuns != 10 || got.ToolFailureStreak != 5 {
		t.Fatalf("effective = %+v", got)
	}
}

func TestEvaluate(t *testing.T) {
	settings := DefaultSettings()
	high := settings.Tiers[agentlifecycle.TierHigh]
	high.Rules.SpendPerHourUSD, high.Rules.RunsPerHour = 10, 100
	settings.Tiers[agentlifecycle.TierHigh] = high
	key := AgentKey{ProjectID: 3, ApplicationID: 9}

	// Exactly 50 % of 20 is not "more than 50 %".
	if _, ok := Evaluate(settings, Observation{Key: key, Tier: agentlifecycle.TierLow, Outcomes: outcomes(10, 20)}); ok {
		t.Fatal("10 of 20 tripped")
	}
	// A window that is not full is not judged.
	if _, ok := Evaluate(settings, Observation{Key: key, Tier: agentlifecycle.TierLow, Outcomes: outcomes(19, 19)}); ok {
		t.Fatal("19 runs tripped")
	}
	trip, ok := Evaluate(settings, Observation{Key: key, Tier: agentlifecycle.TierLow, Outcomes: outcomes(11, 20)})
	if !ok || trip.Rule != RuleErrorRate || trip.FailedRuns != 11 || trip.WindowRuns != 20 || trip.Observed != 55 ||
		trip.Threshold != 50 || trip.ResumeRequires != safety.ResumeAnyAdmin {
		t.Fatalf("trip = %+v ok=%v", trip, ok)
	}
	if !strings.Contains(trip.Reason(), "11 of the last 20 runs failed (55 %)") {
		t.Errorf("reason = %q", trip.Reason())
	}
	// Suspended: never re-tripped.
	if _, ok := Evaluate(settings, Observation{Key: key, Suspended: true, Outcomes: outcomes(20, 20)}); ok {
		t.Fatal("a suspended agent tripped")
	}
	// The override tightens: 30 % of a 10-run window.
	trip, ok = Evaluate(settings, Observation{Key: key, Tier: agentlifecycle.TierLow,
		Override: Override{ErrorRatePercent: ptr(30), ErrorRateWindowRuns: ptr(10)}, Outcomes: outcomes(4, 20)})
	if !ok || trip.Rule != RuleErrorRate || trip.WindowRuns != 10 || trip.FailedRuns != 4 {
		t.Fatalf("override trip = %+v ok=%v", trip, ok)
	}
	// Tool streak.
	trip, ok = Evaluate(settings, Observation{Key: key, Tier: agentlifecycle.TierCritical, ToolStreak: 5, ToolName: "search", ToolExecutionID: "e1"})
	if !ok || trip.Rule != RuleToolFailureStreak || trip.ToolName != "search" || trip.ResumeRequires != safety.ResumeTwoPerson {
		t.Fatalf("tool trip = %+v ok=%v", trip, ok)
	}
	// Spend wins over the others at the high tier (checked first).
	trip, ok = Evaluate(settings, Observation{Key: key, Tier: agentlifecycle.TierHigh, SpendLastHourUSD: 10.004, RunsLastHour: 500, Outcomes: outcomes(20, 20)})
	if !ok || trip.Rule != RuleSpendPerHour || trip.Observed != 10 || trip.ResumeRequires != safety.ResumeGovernanceRole {
		t.Fatalf("spend trip = %+v ok=%v", trip, ok)
	}
	trip, ok = Evaluate(settings, Observation{Key: key, Tier: agentlifecycle.TierHigh, RunsLastHour: 101})
	if !ok || trip.Rule != RuleRunsPerHour || trip.WindowSeconds != 3600 {
		t.Fatalf("runs trip = %+v ok=%v", trip, ok)
	}
	reasons := strings.Join(trip.AuditReasons(42), " ")
	for _, want := range []string{"rule:runs_per_hour", "observed:101", "threshold:100", "suspension:42", "tier:high"} {
		if !strings.Contains(reasons, want) {
			t.Errorf("audit reasons %q lack %q", reasons, want)
		}
	}
}

// fakeStore is an in-memory Store.
type fakeStore struct {
	touched      []AgentKey
	observations map[AgentKey]Observation
	spend        map[AgentKey]float64
	spendReads   int
	trips        []Trip
	notified     int
	nextID       int64
}

func (f *fakeStore) IngestRuns(context.Context, time.Duration) ([]AgentKey, error) { return f.touched, nil }
func (f *fakeStore) SpendLastHour(context.Context) (map[AgentKey]float64, error) {
	f.spendReads++
	return f.spend, nil
}
func (f *fakeStore) AnySpendOverride(context.Context) (bool, error) { return false, nil }
func (f *fakeStore) Observe(_ context.Context, key AgentKey, _ int) (Observation, error) {
	o := f.observations[key]
	o.Key = key
	return o, nil
}
func (f *fakeStore) Trip(_ context.Context, trip Trip) (safety.Suspension, error) {
	f.trips = append(f.trips, trip)
	f.nextID++
	return safety.Suspension{ID: f.nextID, Source: safety.SourceBreaker}, nil
}
func (f *fakeStore) NotifyTrip(context.Context, Trip, safety.Suspension) (int, error) {
	f.notified++
	return 1, nil
}
func (f *fakeStore) Prune(context.Context, time.Duration) (int64, error) { return 0, nil }

func TestPassTripsNotifiesAndCounts(t *testing.T) {
	failing, healthy := AgentKey{1, 10}, AgentKey{1, 11}
	store := &fakeStore{
		touched: []AgentKey{failing, healthy},
		observations: map[AgentKey]Observation{
			failing: {Tier: agentlifecycle.TierHigh, Outcomes: outcomes(15, 20)},
			healthy: {Tier: agentlifecycle.TierHigh, Outcomes: outcomes(1, 20)},
		},
	}
	metrics := NewMetrics()
	var notified []int64
	pass := &Pass{Store: store, Settings: func(context.Context) (Settings, error) { return DefaultSettings(), nil },
		Notify: func(id int64) { notified = append(notified, id) }, Metrics: metrics}
	summary, err := pass.Run(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if summary.Tripped != 1 || len(store.trips) != 1 || store.trips[0].Key != failing || len(notified) != 1 || store.notified != 1 {
		t.Fatalf("summary %s trips %+v", summary.String(), store.trips)
	}
	if store.spendReads != 0 {
		t.Errorf("spend read with no spend limit")
	}
	if metrics.Trips(RuleErrorRate) != 1 {
		t.Errorf("trips metric = %d", metrics.Trips(RuleErrorRate))
	}
	var text strings.Builder
	if err := metrics.WriteText(&text); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`elitea_breaker_trips_total{rule="error_rate"} 1`,
		`elitea_breaker_trips_total{rule="spend_per_hour"} 0`,
		`elitea_safety_suspensions_active{scope="platform",source="manual"} 0`,
		"elitea_breaker_storms_total 0",
	} {
		if !strings.Contains(text.String(), want) {
			t.Errorf("exposition lacks %q", want)
		}
	}
}

func TestPassDisabledDoesNothing(t *testing.T) {
	store := &fakeStore{touched: []AgentKey{{1, 1}}}
	settings := DefaultSettings()
	settings.Enabled = false
	summary, err := (&Pass{Store: store, Settings: func(context.Context) (Settings, error) { return settings, nil }}).Run(t.Context())
	if err != nil || summary.Enabled || summary.Ingested != 0 {
		t.Fatalf("summary %s err %v", summary.String(), err)
	}
}

// More agents failing at once than the storm limit: a dependency, not the
// agents. None is tripped on error_rate; a spend trip still goes through.
func TestPassStormLimit(t *testing.T) {
	settings := DefaultSettings()
	settings.StormLimit = 2
	low := settings.Tiers[agentlifecycle.TierLow]
	low.Rules.SpendPerHourUSD = 1
	settings.Tiers[agentlifecycle.TierLow] = low
	store := &fakeStore{observations: map[AgentKey]Observation{}, spend: map[AgentKey]float64{}}
	for i := range 3 {
		key := AgentKey{1, int64(20 + i)}
		store.touched = append(store.touched, key)
		store.observations[key] = Observation{Tier: agentlifecycle.TierLow, Outcomes: outcomes(20, 20)}
	}
	spender := AgentKey{2, 30}
	store.spend[spender] = 5
	store.observations[spender] = Observation{Tier: agentlifecycle.TierLow}
	metrics := NewMetrics()
	summary, err := (&Pass{Store: store, Settings: func(context.Context) (Settings, error) { return settings, nil }, Metrics: metrics}).Run(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if !summary.Storm || summary.Tripped != 1 || store.trips[0].Rule != RuleSpendPerHour || metrics.Storms() != 1 {
		t.Fatalf("summary %s trips %+v", summary.String(), store.trips)
	}
}

func TestMetricsActiveGauge(t *testing.T) {
	metrics := NewMetrics()
	metrics.SetActiveSource(func(context.Context) ([]safety.Suspension, error) {
		return []safety.Suspension{
			{Target: safety.Target{Scope: safety.ScopePlatform}, Source: safety.SourceManual},
			{Target: safety.Target{Scope: safety.ScopeAgent}, Source: safety.SourceBreaker},
			{Target: safety.Target{Scope: safety.ScopeAgent}, Source: safety.SourceBreaker},
		}, nil
	})
	var text strings.Builder
	if err := metrics.WriteText(&text); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`elitea_safety_suspensions_active{scope="platform",source="manual"} 1`,
		`elitea_safety_suspensions_active{scope="agent",source="breaker"} 2`,
		`elitea_safety_suspensions_active{scope="project",source="manual"} 0`,
	} {
		if !strings.Contains(text.String(), want) {
			t.Errorf("exposition lacks %q:\n%s", want, text.String())
		}
	}
	// A failed read keeps the last counts.
	metrics.SetActiveSource(func(context.Context) ([]safety.Suspension, error) { return nil, errors.New("down") })
	text.Reset()
	_ = metrics.WriteText(&text)
	if !strings.Contains(text.String(), `elitea_safety_suspensions_active{scope="platform",source="manual"} 1`) {
		t.Errorf("a failed read reset the gauge")
	}
}
