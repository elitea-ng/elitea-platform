package llmproxy

// soft_alert_switch_test.go — the platform soft-alert switch (issue #322).
//
// Before this, PUT /admin/gateway/budget-alerts returned 200 for a value no
// gateway read: `grep -rn 'budget_alert|threshold_pct' services/elitea-llm-gateway`
// returned zero hits. An operator who turned alerts off saw a changed GET and
// alerts that kept firing.
//
// Both directions are asserted against the same handler and the same crossing:
// with the switch on the alert fires, with it off it does not. Without the
// "on" case, a trySoftAlert that never fired would pass the suppression test.

import (
	"context"
	"testing"

	"github.com/maximhq/bifrost/core/schemas"

	"github.com/elitea-ng/elitea-platform/services/elitea-llm-gateway/internal/failmode"
)

// crossedDecision is a post-increment decision that HAS crossed the soft
// threshold on the healthy path, with the switch set as given.
func crossedDecision(alertsDisabled bool) failmode.Decision {
	return failmode.Decision{
		Verdict:            failmode.Allow,
		State:              failmode.StateNATSHealthy,
		SoftThresholdNear:  true,
		SoftAlertsDisabled: alertsDisabled,
	}
}

// belowThreshold is the pre-increment decision: the spend was under the soft
// threshold, so this increment is the crossing.
func belowThreshold() failmode.Decision {
	return failmode.Decision{
		Verdict:           failmode.Allow,
		State:             failmode.StateNATSHealthy,
		SoftThresholdNear: false,
	}
}

func TestSoftAlertSwitch_EnabledFires(t *testing.T) {
	checker := &softAlertChecker{checkResult: crossedDecision(false), alertFired: true}
	h := NewHandler(nil, nil, nil, WithBudgetGate(checker, &fakeCostEstimator{}))

	ctx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
	h.trySoftAlert(ctx, h.budget(), 1, budgetScopeProject, "1", 0, 500_000, belowThreshold())

	if checker.alertCalled.Load() != 1 {
		t.Fatalf("TryAlertCooldown called %d times, want 1 — the crossing must alert while the switch is on",
			checker.alertCalled.Load())
	}
}

func TestSoftAlertSwitch_DisabledSuppresses(t *testing.T) {
	// Identical to the test above in every respect except the switch.
	checker := &softAlertChecker{checkResult: crossedDecision(true), alertFired: true}
	h := NewHandler(nil, nil, nil, WithBudgetGate(checker, &fakeCostEstimator{}))

	ctx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
	h.trySoftAlert(ctx, h.budget(), 1, budgetScopeProject, "1", 0, 500_000, belowThreshold())

	if checker.alertCalled.Load() != 0 {
		t.Fatalf("TryAlertCooldown called %d times, want 0 — the operator turned soft alerts off",
			checker.alertCalled.Load())
	}
}

// TestSoftAlertSwitch_DisabledDoesNotClaimCooldown pins the ordering choice.
// The switch is checked BEFORE the cooldown claim, so the first crossing after
// an operator re-enables alerts still fires rather than being suppressed by a
// claim made silently while alerts were off.
//
// The assertion is the same call count as the test above; this test exists to
// name the property, so a refactor that moved the check after the claim has a
// failure whose message says what it broke.
func TestSoftAlertSwitch_DisabledDoesNotClaimCooldown(t *testing.T) {
	checker := &softAlertChecker{checkResult: crossedDecision(true), alertFired: true}
	h := NewHandler(nil, nil, nil, WithBudgetGate(checker, &fakeCostEstimator{}))

	ctx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
	h.trySoftAlert(ctx, h.budget(), 1, budgetScopeProject, "1", 0, 500_000, belowThreshold())

	if checker.alertCalled.Load() != 0 {
		t.Fatal("the cooldown was claimed while alerts were off; the first crossing after re-enabling would be suppressed")
	}
}
