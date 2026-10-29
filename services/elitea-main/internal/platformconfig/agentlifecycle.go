package platformconfig

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/agentlifecycle"
)

// SectionAgentLifecycle is the platform's tier table of agent lifecycle
// governance (ADR-0040 decisions 3, 5 and 6, #1497): per risk tier, the
// approvals a change request needs and how many must come from the
// governance role, the recertification cadence and the mandatory
// attestation kinds; and what a lapsed certification does.
//
// A key that is absent keeps the ADR default (agentlifecycle.DefaultPolicy).
// A value below the ADR floor is read as the floor: the administrator may
// raise a tier and never lower it below the ADR table.
const SectionAgentLifecycle = "agent_lifecycle"

// Key suffixes of SectionAgentLifecycle. Each tier has the four keys
// "<tier>_<suffix>", for example "critical_approvals".
const (
	KeyLifecycleApprovalsSuffix           = "approvals"
	KeyLifecycleGovernanceApprovalsSuffix = "governance_approvals"
	KeyLifecycleRecertifyDaysSuffix       = "recertify_days"
	KeyLifecycleAttestationsSuffix        = "attestations"
	// KeyLifecycleLapseAction is warn (the default) or suspend.
	KeyLifecycleLapseAction = "lapse_action"
)

// LifecycleKey is the key of one tier's field.
func LifecycleKey(tier agentlifecycle.Tier, suffix string) string {
	return string(tier) + "_" + suffix
}

// AgentLifecyclePolicyFromValues is the overlay of the section on the ADR
// defaults. An unreadable number keeps the default; an unknown attestation
// kind is skipped; the policy clamps every tier to its floor when read.
func AgentLifecyclePolicyFromValues(values Values) agentlifecycle.Policy {
	policy := agentlifecycle.DefaultPolicy()
	for _, tier := range agentlifecycle.Tiers {
		requirement := policy.Tiers[tier]
		if n, ok := values.Int(LifecycleKey(tier, KeyLifecycleApprovalsSuffix)); ok {
			requirement.Approvals = int(min(max(n, 0), agentlifecycle.MaxApprovals))
		}
		if n, ok := values.Int(LifecycleKey(tier, KeyLifecycleGovernanceApprovalsSuffix)); ok {
			requirement.GovernanceApprovals = int(min(max(n, 0), agentlifecycle.MaxApprovals))
		}
		if n, ok := values.Int(LifecycleKey(tier, KeyLifecycleRecertifyDaysSuffix)); ok {
			requirement.RecertifyEvery = time.Duration(min(max(n, 0), agentlifecycle.MaxRecertifyDays)) * agentlifecycle.Day
		}
		if _, present := values[LifecycleKey(tier, KeyLifecycleAttestationsSuffix)]; present {
			kinds := []agentlifecycle.AttestationKind{}
			for _, raw := range values.Strings(LifecycleKey(tier, KeyLifecycleAttestationsSuffix)) {
				kind := agentlifecycle.AttestationKind(raw)
				if kind.Valid() && kind != agentlifecycle.AttestationCustom {
					kinds = append(kinds, kind)
				}
			}
			requirement.Attestations = kinds
		}
		policy.Tiers[tier] = requirement
	}
	if action := agentlifecycle.LapseAction(values.String(KeyLifecycleLapseAction, "")); action.Valid() {
		policy.LapseAction = action
	}
	return policy
}

// LoadAgentLifecyclePolicy reads the section. A database without the table
// never saved it and answers the ADR defaults.
func LoadAgentLifecyclePolicy(ctx context.Context, pool *pgxpool.Pool) (agentlifecycle.Policy, error) {
	values, err := Load(ctx, pool, SectionAgentLifecycle)
	if missingTable(err) {
		return agentlifecycle.DefaultPolicy(), nil
	}
	if err != nil {
		return agentlifecycle.Policy{}, err
	}
	return AgentLifecyclePolicyFromValues(values), nil
}
