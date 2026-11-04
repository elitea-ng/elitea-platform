package platformconfig

import (
	"context"
	"time"

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

// LoadAgentLifecyclePolicyVia reads the section through query: the caller's
// own transaction, not a second pool connection. The lifecycle store reads
// the tier table while it holds the registry row's lock in a transaction, and
// a read on the pool there needs a second connection per caller: as many
// concurrent publishes as the pool has connections then each hold one and
// wait for another, and the process stops answering until a request's
// context ends. A database without the table answers the ADR defaults
// without running a query that fails, so the caller's transaction is not
// aborted.
func LoadAgentLifecyclePolicyVia(ctx context.Context, query QueryFunc) (agentlifecycle.Policy, error) {
	present, err := tableExists(ctx, query, "centry.platform_config")
	if err != nil {
		return agentlifecycle.Policy{}, err
	}
	if !present {
		return agentlifecycle.DefaultPolicy(), nil
	}
	values, err := LoadVia(ctx, query, SectionAgentLifecycle)
	if err != nil {
		return agentlifecycle.Policy{}, err
	}
	return AgentLifecyclePolicyFromValues(values), nil
}

// tableExists answers whether the named relation exists, through query.
func tableExists(ctx context.Context, query QueryFunc, name string) (bool, error) {
	rows, err := query(ctx, `SELECT to_regclass($1) IS NOT NULL`, name)
	if err != nil {
		return false, err
	}
	defer rows.Close()
	present := false
	if rows.Next() {
		if err := rows.Scan(&present); err != nil {
			return false, err
		}
	}
	return present, rows.Err()
}
