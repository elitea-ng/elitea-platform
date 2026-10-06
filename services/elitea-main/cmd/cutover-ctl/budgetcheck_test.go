package main

import (
	"testing"
)

// TestBudgetCheck_Pass_402AlertInTime verifies that a result with the correct
// 402 error shape, soft-alert fired within the latency window, and a 200 for
// the under-budget control case all pass cleanly.
func TestBudgetCheck_Pass_402AlertInTime(t *testing.T) {
	r := budgetCheckResult{
		HardBlockStatus: 402,
		HardBlockType:   "budget_exceeded",
		HardBlockCode:   "insufficient_quota",
		RouterCalled:    false,

		SoftAlertFired:    true,
		SoftAlertLatencyS: 3.2,

		UnderBudgetStatus: 200,
	}
	out := checkBudgetResult(r, 10.0)

	if !out.HardBlockPass {
		t.Errorf("HardBlockPass = false, want true; reason: %q", out.HardBlockReason)
	}
	if !out.SoftAlertPass {
		t.Errorf("SoftAlertPass = false, want true; reason: %q", out.SoftAlertReason)
	}
	if !out.UnderBudgetPass {
		t.Errorf("UnderBudgetPass = false, want true; reason: %q", out.UnderBudgetReason)
	}
}

// TestBudgetCheck_Fail_200InsteadOf402 verifies that a non-402 response to an
// over-budget request is caught and fails the hard-block gate.
func TestBudgetCheck_Fail_200InsteadOf402(t *testing.T) {
	r := budgetCheckResult{
		HardBlockStatus: 200, // wrong — should have been blocked
		HardBlockType:   "",
		HardBlockCode:   "",
		RouterCalled:    true,

		SoftAlertFired:    true,
		SoftAlertLatencyS: 1.0,

		UnderBudgetStatus: 200,
	}
	out := checkBudgetResult(r, 10.0)

	if out.HardBlockPass {
		t.Error("HardBlockPass = true, want false (200 should not pass the hard-block gate)")
	}
}

// TestBudgetCheck_Fail_WrongErrorType verifies that a 402 with wrong error.type
// fails the hard-block gate.
func TestBudgetCheck_Fail_WrongErrorType(t *testing.T) {
	r := budgetCheckResult{
		HardBlockStatus: 402,
		HardBlockType:   "rate_limit_error", // wrong type
		HardBlockCode:   "insufficient_quota",
		RouterCalled:    false,

		SoftAlertFired:    true,
		SoftAlertLatencyS: 1.0,

		UnderBudgetStatus: 200,
	}
	out := checkBudgetResult(r, 10.0)

	if out.HardBlockPass {
		t.Errorf("HardBlockPass = true, want false; got type=%q, want budget_exceeded", r.HardBlockType)
	}
}

// TestBudgetCheck_Fail_WrongErrorCode verifies that a 402 with wrong error.code
// fails the hard-block gate.
func TestBudgetCheck_Fail_WrongErrorCode(t *testing.T) {
	r := budgetCheckResult{
		HardBlockStatus: 402,
		HardBlockType:   "budget_exceeded",
		HardBlockCode:   "quota_exceeded", // wrong code
		RouterCalled:    false,

		SoftAlertFired:    true,
		SoftAlertLatencyS: 1.0,

		UnderBudgetStatus: 200,
	}
	out := checkBudgetResult(r, 10.0)

	if out.HardBlockPass {
		t.Errorf("HardBlockPass = true, want false; got code=%q, want insufficient_quota", r.HardBlockCode)
	}
}

// TestBudgetCheck_Fail_RouterCalledBeforeBlock verifies that a 402 with the
// correct type/code but with RouterCalled=true fails — the gate must block at
// admission, before the LLM provider is invoked.
func TestBudgetCheck_Fail_RouterCalledBeforeBlock(t *testing.T) {
	r := budgetCheckResult{
		HardBlockStatus: 402,
		HardBlockType:   "budget_exceeded",
		HardBlockCode:   "insufficient_quota",
		RouterCalled:    true, // provider was called — gate fired too late

		SoftAlertFired:    true,
		SoftAlertLatencyS: 1.0,

		UnderBudgetStatus: 200,
	}
	out := checkBudgetResult(r, 10.0)

	if out.HardBlockPass {
		t.Error("HardBlockPass = true, want false (provider was called before the 402 was returned)")
	}
}

// TestBudgetCheck_Fail_NoAlert verifies that an absent soft-alert fails the
// soft-alert gate.
func TestBudgetCheck_Fail_NoAlert(t *testing.T) {
	r := budgetCheckResult{
		HardBlockStatus: 402,
		HardBlockType:   "budget_exceeded",
		HardBlockCode:   "insufficient_quota",
		RouterCalled:    false,

		SoftAlertFired:    false, // no alert observed
		SoftAlertLatencyS: 0,

		UnderBudgetStatus: 200,
	}
	out := checkBudgetResult(r, 10.0)

	if out.SoftAlertPass {
		t.Error("SoftAlertPass = true, want false (alert was not fired)")
	}
}

// TestBudgetCheck_Fail_LateAlert verifies that an alert observed after the
// configured window fails the soft-alert gate.
func TestBudgetCheck_Fail_LateAlert(t *testing.T) {
	r := budgetCheckResult{
		HardBlockStatus: 402,
		HardBlockType:   "budget_exceeded",
		HardBlockCode:   "insufficient_quota",
		RouterCalled:    false,

		SoftAlertFired:    true,
		SoftAlertLatencyS: 15.7, // exceeds default 10 s window

		UnderBudgetStatus: 200,
	}
	out := checkBudgetResult(r, 10.0)

	if out.SoftAlertPass {
		t.Errorf("SoftAlertPass = true, want false (latency %.1f s exceeds 10 s window)", r.SoftAlertLatencyS)
	}
}

// TestBudgetCheck_Fail_UnderBudgetNotAllowed verifies that a non-200 for the
// under-budget control case fails the gate.
func TestBudgetCheck_Fail_UnderBudgetNotAllowed(t *testing.T) {
	r := budgetCheckResult{
		HardBlockStatus: 402,
		HardBlockType:   "budget_exceeded",
		HardBlockCode:   "insufficient_quota",
		RouterCalled:    false,

		SoftAlertFired:    true,
		SoftAlertLatencyS: 1.0,

		UnderBudgetStatus: 403, // wrong — under-budget requests should be allowed
	}
	out := checkBudgetResult(r, 10.0)

	if out.UnderBudgetPass {
		t.Errorf("UnderBudgetPass = true, want false (under-budget returned HTTP %d)", r.UnderBudgetStatus)
	}
}

// TestBudgetCheck_AlertExactlyAtThreshold_Pass verifies that an alert observed
// at exactly the configured latency threshold is accepted (boundary condition).
func TestBudgetCheck_AlertExactlyAtThreshold_Pass(t *testing.T) {
	r := budgetCheckResult{
		HardBlockStatus: 402,
		HardBlockType:   "budget_exceeded",
		HardBlockCode:   "insufficient_quota",
		RouterCalled:    false,

		SoftAlertFired:    true,
		SoftAlertLatencyS: 10.0, // exactly at the boundary

		UnderBudgetStatus: 200,
	}
	out := checkBudgetResult(r, 10.0)

	// Exactly at threshold: 10.0 <= 10.0 → pass.
	if !out.SoftAlertPass {
		t.Errorf("SoftAlertPass = false at exactly the threshold; reason: %q", out.SoftAlertReason)
	}
}

// TestBudgetCheck_AllGatesFail verifies that all three gates can fail
// independently and the outcome reflects each failure.
func TestBudgetCheck_AllGatesFail(t *testing.T) {
	r := budgetCheckResult{
		HardBlockStatus: 200,
		HardBlockType:   "",
		HardBlockCode:   "",
		RouterCalled:    true,

		SoftAlertFired:    false,
		SoftAlertLatencyS: 0,

		UnderBudgetStatus: 429,
	}
	out := checkBudgetResult(r, 10.0)

	if out.HardBlockPass {
		t.Error("HardBlockPass = true, want false")
	}
	if out.SoftAlertPass {
		t.Error("SoftAlertPass = true, want false")
	}
	if out.UnderBudgetPass {
		t.Error("UnderBudgetPass = true, want false")
	}
	// Reasons should be non-empty.
	if out.HardBlockReason == "" {
		t.Error("HardBlockReason is empty despite failure")
	}
	if out.SoftAlertReason == "" {
		t.Error("SoftAlertReason is empty despite failure")
	}
	if out.UnderBudgetReason == "" {
		t.Error("UnderBudgetReason is empty despite failure")
	}
}

// budget-check presents elitea-main's NATS identity: half-configured TLS
// material is refused before any connection is attempted, never silently
// downgraded to plaintext.
func TestBudgetCheckNATSRefusesPartialTLSMaterial(t *testing.T) {
	env := map[string]string{"ELITEA_EVENTS_NATS_TLS_CERT_FILE": "/does/not/exist.crt"}
	lookup := func(k string) (string, bool) { v, ok := env[k]; return v, ok }
	if _, err := connectBudgetCheckNATS("tls://127.0.0.1:1", lookup); err == nil {
		t.Fatal("partial TLS material was accepted")
	}
}
