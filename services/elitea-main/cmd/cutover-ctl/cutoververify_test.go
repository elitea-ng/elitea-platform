package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// cleanState returns a cutoverState where all three post-cutover invariants hold.
// Use it as the base for individual failing-path tests so each test only tweaks
// one field.
func cleanState() cutoverState {
	return cutoverState{
		Gateway5xxCount:       0,
		LegacySvcTrafficRPS:   0.0,
		HardBlock402Confirmed: true,
		WindowMinutes:         15,
	}
}

// TestCutoverVerify_CleanState_Pass asserts that a fully-healthy post-cutover
// state (zero 5xx, zero legacy traffic, 402 confirmed)
// evaluates to ok=true with no reasons.
func TestCutoverVerify_CleanState_Pass(t *testing.T) {
	ok, reasons := evaluateCutover(cleanState())
	if !ok {
		t.Errorf("expected ok=true for clean state, got false; reasons: %v", reasons)
	}
	if len(reasons) != 0 {
		t.Errorf("expected zero reasons for clean state, got %d: %v", len(reasons), reasons)
	}
}

// TestCutoverVerify_5xxPresent_Fail asserts that a non-zero gateway 5xx count
// causes the gate to fail with exactly one reason that includes the count.
func TestCutoverVerify_5xxPresent_Fail(t *testing.T) {
	state := cleanState()
	state.Gateway5xxCount = 3

	ok, reasons := evaluateCutover(state)
	if ok {
		t.Fatal("expected ok=false when Gateway5xxCount=3, got true")
	}
	if len(reasons) != 1 {
		t.Fatalf("expected 1 reason, got %d: %v", len(reasons), reasons)
	}
	if reasons[0] == "" {
		t.Error("reason must not be empty")
	}
}

// TestCutoverVerify_LegacyTraffic_Fail asserts that any positive traffic rate
// to litellm-svc:4000 causes the gate to fail with exactly one reason.
func TestCutoverVerify_LegacyTraffic_Fail(t *testing.T) {
	state := cleanState()
	state.LegacySvcTrafficRPS = 0.42

	ok, reasons := evaluateCutover(state)
	if ok {
		t.Fatal("expected ok=false when LegacySvcTrafficRPS=0.42, got true")
	}
	if len(reasons) != 1 {
		t.Fatalf("expected 1 reason, got %d: %v", len(reasons), reasons)
	}
	if reasons[0] == "" {
		t.Error("reason must not be empty")
	}
}

// TestCutoverVerify_No402_Fail asserts that a missing 402 hard-block
// confirmation causes the gate to fail with exactly one reason.
func TestCutoverVerify_No402_Fail(t *testing.T) {
	state := cleanState()
	state.HardBlock402Confirmed = false

	ok, reasons := evaluateCutover(state)
	if ok {
		t.Fatal("expected ok=false when HardBlock402Confirmed=false, got true")
	}
	if len(reasons) != 1 {
		t.Fatalf("expected 1 reason, got %d: %v", len(reasons), reasons)
	}
	if reasons[0] == "" {
		t.Error("reason must not be empty")
	}
}

// TestCutoverVerify_AllFailing_ThreeReasons asserts that a state where all three
// invariants are broken produces exactly three reasons.
func TestCutoverVerify_AllFailing_ThreeReasons(t *testing.T) {
	state := cutoverState{
		Gateway5xxCount:       7,
		LegacySvcTrafficRPS:   1.5,
		HardBlock402Confirmed: false,
		WindowMinutes:         15,
	}

	ok, reasons := evaluateCutover(state)
	if ok {
		t.Fatal("expected ok=false when all invariants fail, got true")
	}
	if len(reasons) != 3 {
		t.Errorf("expected 3 reasons, got %d: %v", len(reasons), reasons)
	}
}

// TestCutoverVerify_FixtureLoad_CleanState asserts that loadCutoverStateFromFile
// correctly deserialises testdata/cutover_state_clean.json and that evaluateCutover
// passes on it — validating the fixture round-trip.
func TestCutoverVerify_FixtureLoad_CleanState(t *testing.T) {
	path := filepath.Join("testdata", "cutover_state_clean.json")
	state, err := loadCutoverStateFromFile(path)
	if err != nil {
		t.Fatalf("loadCutoverStateFromFile(%q): %v", path, err)
	}

	// Verify the fixture was deserialised with the expected field values.
	if state.Gateway5xxCount != 0 {
		t.Errorf("Gateway5xxCount = %d, want 0", state.Gateway5xxCount)
	}
	if state.LegacySvcTrafficRPS != 0.0 {
		t.Errorf("LegacySvcTrafficRPS = %v, want 0.0", state.LegacySvcTrafficRPS)
	}
	if !state.HardBlock402Confirmed {
		t.Error("HardBlock402Confirmed = false, want true")
	}
	if state.WindowMinutes != 15 {
		t.Errorf("WindowMinutes = %d, want 15", state.WindowMinutes)
	}

	// The gate must pass on this fixture.
	ok, reasons := evaluateCutover(state)
	if !ok {
		t.Errorf("evaluateCutover(clean fixture) = false, want true; reasons: %v", reasons)
	}
}

// TestCutoverVerify_FixtureLoad_DirtyState asserts that
// testdata/cutover_state_dirty.json loads correctly and that evaluateCutover
// fails on it (traffic still reaching litellm-svc).
func TestCutoverVerify_FixtureLoad_DirtyState(t *testing.T) {
	path := filepath.Join("testdata", "cutover_state_dirty.json")
	state, err := loadCutoverStateFromFile(path)
	if err != nil {
		t.Fatalf("loadCutoverStateFromFile(%q): %v", path, err)
	}

	if state.LegacySvcTrafficRPS <= 0 {
		t.Errorf("expected positive LegacySvcTrafficRPS in dirty fixture, got %v", state.LegacySvcTrafficRPS)
	}

	ok, reasons := evaluateCutover(state)
	if ok {
		t.Error("evaluateCutover(dirty fixture) = true, want false")
	}
	if len(reasons) == 0 {
		t.Error("expected at least one reason from dirty fixture, got none")
	}
}

// TestCutoverVerify_FixtureLoad_MissingFile asserts that loadCutoverStateFromFile
// returns a non-nil error (no panic, no os.Exit) when the path does not exist.
func TestCutoverVerify_FixtureLoad_MissingFile(t *testing.T) {
	_, err := loadCutoverStateFromFile("testdata/does_not_exist.json")
	if err == nil {
		t.Fatal("expected error for missing fixture file, got nil")
	}
}

// TestCutoverVerify_FixtureLoad_MalformedJSON asserts that loadCutoverStateFromFile
// returns a non-nil error when the file contains malformed JSON.
func TestCutoverVerify_FixtureLoad_MalformedJSON(t *testing.T) {
	tmpPath := filepath.Join(t.TempDir(), "bad.json")
	if err := os.WriteFile(tmpPath, []byte(`{not valid json`), 0o644); err != nil {
		t.Fatalf("setup: %v", err)
	}

	_, err := loadCutoverStateFromFile(tmpPath)
	if err == nil {
		t.Fatal("expected error for malformed JSON fixture, got nil")
	}
}

// TestCutoverVerify_FixtureLoad_UnknownField asserts that a state recorded
// with a field the gate no longer reads (the removed litellm_subprocess_present
// invariant) is refused rather than silently passing.
func TestCutoverVerify_FixtureLoad_UnknownField(t *testing.T) {
	tmpPath := filepath.Join(t.TempDir(), "old.json")
	body := `{"litellm_subprocess_present":true,"legacy_svc_traffic_rps":0,"hard_block_402_confirmed":true}`
	if err := os.WriteFile(tmpPath, []byte(body), 0o644); err != nil {
		t.Fatalf("setup: %v", err)
	}

	_, err := loadCutoverStateFromFile(tmpPath)
	if err == nil || !strings.Contains(err.Error(), "litellm_subprocess_present") {
		t.Fatalf("expected an unknown-field error naming litellm_subprocess_present, got %v", err)
	}
}

// TestCutoverVerify_WindowMinutes_Informational asserts that WindowMinutes is
// carried through the state struct without affecting the pass/fail decision.
func TestCutoverVerify_WindowMinutes_Informational(t *testing.T) {
	// Same healthy state, different window values — should always pass.
	for _, w := range []int{5, 15, 60} {
		state := cleanState()
		state.WindowMinutes = w
		ok, _ := evaluateCutover(state)
		if !ok {
			t.Errorf("WindowMinutes=%d: expected ok=true, got false", w)
		}
	}
}
