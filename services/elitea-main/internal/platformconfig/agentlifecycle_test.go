package platformconfig

import (
	"testing"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/agentlifecycle"
)

func TestAgentLifecyclePolicyFromValues(t *testing.T) {
	// No saved key: the ADR defaults.
	policy := AgentLifecyclePolicyFromValues(Values{})
	if got := policy.Requirement(agentlifecycle.TierCritical); got.Approvals != 2 || len(got.Attestations) != 2 {
		t.Fatalf("defaults: %+v", got)
	}

	policy = AgentLifecyclePolicyFromValues(Values{
		"medium_approvals":          float64(2),
		"high_recertify_days":       float64(30),
		"low_approvals":             float64(1),
		"critical_attestations":     []any{"legal", "custom", "nonsense"},
		"medium_attestations":       []any{"data_owner"},
		"critical_approvals":        float64(1), // below the floor
		"high_governance_approvals": 1.5,        // not an integer: kept
		"lapse_action":              "suspend",
	})
	if got := policy.Requirement(agentlifecycle.TierMedium); got.Approvals != 2 || len(got.Attestations) != 1 {
		t.Errorf("medium = %+v", got)
	}
	if got := policy.Requirement(agentlifecycle.TierHigh); got.RecertifyEvery != 30*agentlifecycle.Day || got.GovernanceApprovals != 1 {
		t.Errorf("high = %+v", got)
	}
	if got := policy.Requirement(agentlifecycle.TierLow); !got.RequiresApproval() {
		t.Errorf("low raised to one approval = %+v", got)
	}
	critical := policy.Requirement(agentlifecycle.TierCritical)
	if critical.Approvals != 2 || len(critical.Attestations) != 1 || critical.Attestations[0] != agentlifecycle.AttestationLegal {
		t.Errorf("critical = %+v", critical)
	}
	if policy.LapseAction != agentlifecycle.LapseSuspend {
		t.Errorf("lapse action = %s", policy.LapseAction)
	}
	if got := AgentLifecyclePolicyFromValues(Values{"lapse_action": "explode"}); got.LapseAction != agentlifecycle.LapseWarn {
		t.Errorf("unknown lapse action = %s", got.LapseAction)
	}
}
