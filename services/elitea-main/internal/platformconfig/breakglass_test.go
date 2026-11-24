package platformconfig

import (
	"testing"
	"time"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/adminaccess"
)

var breakGlassClock = time.Date(2026, time.November, 24, 9, 0, 0, 0, time.UTC)

// apply stores a plan's writes over the stored values, as the save path does.
func apply(stored Values, requested bool, plan BreakGlassApprovalPlan) Values {
	next := Values{}
	for key, value := range stored {
		next[key] = value
	}
	next[KeyBreakGlassApprovalRequired] = requested
	for key, value := range plan.Writes {
		next[key] = value
	}
	return next
}

// The ADR defaults (decision 5, owner decision 5).
func TestBreakGlassDefaults(t *testing.T) {
	t.Parallel()
	got := DefaultBreakGlassSettings()
	for name, c := range map[string]struct{ got, want time.Duration }{
		"online default":     {got.OnlineDefault, 60 * time.Minute},
		"online cap":         {got.OnlineMax, 240 * time.Minute},
		"sealed default":     {got.SealedDefault, 60 * time.Minute},
		"sealed cap":         {got.SealedMax, 120 * time.Minute},
		"request lifetime":   {got.RequestLifetime, 30 * time.Minute},
		"review online":      {got.ReviewOnline, 72 * time.Hour},
		"review sealed":      {got.ReviewSealed, 24 * time.Hour},
		"re-authentication":  {got.Reauthentication, 15 * time.Minute},
		"approval-off delay": {got.ApprovalOffDelay, 24 * time.Hour},
	} {
		if c.got != c.want {
			t.Errorf("%s = %s, want %s", name, c.got, c.want)
		}
	}
	policy := got.At(breakGlassClock)
	if !got.ApprovalRequested || !policy.ApprovalRequired || policy.ApprovalState != BreakGlassApprovalOn {
		t.Errorf("approval by default: %+v", policy)
	}
}

// The ceilings are the ledger's constants: one source.
func TestBreakGlassCeilingsAreTheLedgers(t *testing.T) {
	t.Parallel()
	if BreakGlassCeilingOnlineMinutes != 480 || BreakGlassCeilingSealedMinutes != 240 {
		t.Fatalf("ceilings %d/%d, want 480/240", BreakGlassCeilingOnlineMinutes, BreakGlassCeilingSealedMinutes)
	}
	if time.Duration(BreakGlassCeilingOnlineMinutes)*time.Minute != adminaccess.MaxElevationDurationOnline ||
		time.Duration(BreakGlassCeilingSealedMinutes)*time.Minute != adminaccess.MaxElevationDurationSealed {
		t.Fatal("the section's ceilings are not the ledger's")
	}
	for _, key := range []string{KeyBreakGlassOnlineMaxMinutes, KeyBreakGlassOnlineDefaultMinutes} {
		if bound, _ := BreakGlassBoundFor(key); bound.Max != BreakGlassCeilingOnlineMinutes {
			t.Errorf("%s maximum %d, want the online ceiling", key, bound.Max)
		}
	}
	for _, key := range []string{KeyBreakGlassSealedMaxMinutes, KeyBreakGlassSealedDefaultMinutes} {
		if bound, _ := BreakGlassBoundFor(key); bound.Max != BreakGlassCeilingSealedMinutes {
			t.Errorf("%s maximum %d, want the sealed ceiling", key, bound.Max)
		}
	}
}

// A stored value is read; one out of range keeps the default; a default above
// its cap is lowered to the cap.
func TestBreakGlassSettingsFromValues(t *testing.T) {
	t.Parallel()
	got := BreakGlassSettingsFromValues(Values{
		KeyBreakGlassOnlineMaxMinutes:     float64(480),
		KeyBreakGlassSealedMaxMinutes:     float64(241),
		KeyBreakGlassSealedDefaultMinutes: float64(90),
		KeyBreakGlassOnlineDefaultMinutes: "sixty",
		KeyBreakGlassRequestLifetimeMins:  float64(10),
	})
	if got.OnlineMax != 480*time.Minute || got.SealedMax != 120*time.Minute || got.SealedDefault != 90*time.Minute ||
		got.OnlineDefault != 60*time.Minute || got.RequestLifetime != 10*time.Minute {
		t.Errorf("settings %+v", got)
	}
	lowered := BreakGlassSettingsFromValues(Values{KeyBreakGlassOnlineMaxMinutes: float64(30)})
	if lowered.OnlineDefault != 30*time.Minute {
		t.Errorf("a default above its cap = %s, want the cap", lowered.OnlineDefault)
	}
}

// After a switch-off the effective policy still requires approval within the
// delay and stops requiring it after; switching it on inside the delay cancels
// the change, at once.
func TestBreakGlassApprovalOffTakesEffectAfterTheDelay(t *testing.T) {
	t.Parallel()
	stored := Values{}
	off := PlanBreakGlassApproval(stored, false, breakGlassClock)
	if off.Change != BreakGlassApprovalOffRequested || off.Before != BreakGlassApprovalOn ||
		!off.EffectiveFrom.Equal(breakGlassClock.Add(24*time.Hour)) {
		t.Fatalf("plan %+v", off)
	}
	stored = apply(stored, false, off)
	settings := BreakGlassSettingsFromValues(stored)
	for _, c := range []struct {
		at       time.Duration
		required bool
		state    string
	}{
		{0, true, BreakGlassApprovalOffScheduled},
		{23*time.Hour + 59*time.Minute, true, BreakGlassApprovalOffScheduled},
		{24 * time.Hour, false, BreakGlassApprovalOff},
		{48 * time.Hour, false, BreakGlassApprovalOff},
	} {
		policy := settings.At(breakGlassClock.Add(c.at))
		if policy.ApprovalRequired != c.required || policy.ApprovalState != c.state {
			t.Errorf("at +%s: required %v state %q, want %v %q", c.at, policy.ApprovalRequired,
				policy.ApprovalState, c.required, c.state)
		}
	}

	// Saving false again while pending keeps the time.
	again := PlanBreakGlassApproval(stored, false, breakGlassClock.Add(time.Hour))
	if again.Change != BreakGlassApprovalUnchanged || again.Writes != nil {
		t.Fatalf("a second switch-off re-planned: %+v", again)
	}

	// On again inside the delay: at once, and the switch-off is cancelled.
	on := PlanBreakGlassApproval(stored, true, breakGlassClock.Add(2*time.Hour))
	if on.Change != BreakGlassApprovalOnRestored || on.Before != BreakGlassApprovalOffScheduled {
		t.Fatalf("plan %+v", on)
	}
	stored = apply(stored, true, on)
	settings = BreakGlassSettingsFromValues(stored)
	if policy := settings.At(breakGlassClock.Add(48 * time.Hour)); !policy.ApprovalRequired ||
		policy.ApprovalState != BreakGlassApprovalOn {
		t.Fatalf("after the cancel, past the old delay: %+v", policy)
	}
}

// The delay of a switch-off is the one in force before the save.
func TestBreakGlassApprovalOffUsesTheStoredDelay(t *testing.T) {
	t.Parallel()
	plan := PlanBreakGlassApproval(Values{KeyBreakGlassApprovalOffDelayHours: float64(2)}, false, breakGlassClock)
	if !plan.EffectiveFrom.Equal(breakGlassClock.Add(2 * time.Hour)) {
		t.Fatalf("effective from %s, want +2h", plan.EffectiveFrom)
	}
}

// A switch-off without a readable time (a hand edit) fails closed: approval on.
func TestBreakGlassApprovalOffWithoutATimeStaysOn(t *testing.T) {
	t.Parallel()
	for name, values := range map[string]Values{
		"no time":     {KeyBreakGlassApprovalRequired: false},
		"a bad time":  {KeyBreakGlassApprovalRequired: false, KeyBreakGlassApprovalOffEffectiveFrom: "soon"},
		"a null time": {KeyBreakGlassApprovalRequired: false, KeyBreakGlassApprovalOffEffectiveFrom: nil},
	} {
		if policy := BreakGlassSettingsFromValues(values).At(breakGlassClock); !policy.ApprovalRequired {
			t.Errorf("%s: approval is off", name)
		}
	}
}
