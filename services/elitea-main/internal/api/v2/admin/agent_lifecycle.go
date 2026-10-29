package admin

import (
	"fmt"
	"strings"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/agentlifecycle"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/platformconfig"
)

// The Agent Lifecycle Configuration section (ADR-0040 decisions 3, 5 and
// 6): the tier table a change request is held to, the recertification
// cadence and mandatory attestations per tier, and what a lapse does. It is
// LIVE: the change request flow reads it at every submit and approval, and
// the recertification routine at every pass, so a save applies at once.
// It uses configuration.governance, the permission of the other runtime
// governance sections (run caps, worker egress), so no migration seeds a
// new one for it.

var lifecycleTierTitles = map[agentlifecycle.Tier]string{
	agentlifecycle.TierLow:      "Low",
	agentlifecycle.TierMedium:   "Medium",
	agentlifecycle.TierHigh:     "High",
	agentlifecycle.TierCritical: "Critical",
}

// mandatoryAttestationKinds are the kinds a tier may require. custom is
// left out: a custom attestation names its own label, and a tier cannot
// require one it cannot name.
func mandatoryAttestationKinds() []string {
	kinds := []string{}
	for _, kind := range agentlifecycle.AttestationKinds {
		if kind != agentlifecycle.AttestationCustom {
			kinds = append(kinds, string(kind))
		}
	}
	return kinds
}

func agentLifecycleSection() map[string]any {
	const section = platformconfig.SectionAgentLifecycle
	defaults := agentlifecycle.DefaultPolicy()
	fields := []map[string]any{}
	for _, tier := range agentlifecycle.Tiers {
		floor := agentlifecycle.Floors[tier]
		requirement := defaults.Tiers[tier]
		title := lifecycleTierTitles[tier]
		attestations := []any{}
		for _, kind := range requirement.Attestations {
			attestations = append(attestations, string(kind))
		}
		fields = append(fields,
			map[string]any{
				"key":   platformconfig.LifecycleKey(tier, platformconfig.KeyLifecycleApprovalsSuffix),
				"type":  "integer",
				"title": title + " Tier: Approvals",
				"description": fmt.Sprintf("Approvals a change request needs, none from its author. 0 keeps "+
					"single-click publishing. From %d to %d.", floor.Approvals, agentlifecycle.MaxApprovals),
				"section": section,
				"default": requirement.Approvals,
				"minimum": floor.Approvals,
				"maximum": agentlifecycle.MaxApprovals,
			},
			map[string]any{
				"key":   platformconfig.LifecycleKey(tier, platformconfig.KeyLifecycleGovernanceApprovalsSuffix),
				"type":  "integer",
				"title": title + " Tier: Governance Approvals",
				"description": fmt.Sprintf("How many of those approvals must come from a holder of %s. "+
					"Never more than the approvals. From %d to %d.", agentlifecycle.GovernancePermission,
					floor.GovernanceApprovals, agentlifecycle.MaxApprovals),
				"section": section,
				"default": requirement.GovernanceApprovals,
				"minimum": floor.GovernanceApprovals,
				"maximum": agentlifecycle.MaxApprovals,
			},
			map[string]any{
				"key":   platformconfig.LifecycleKey(tier, platformconfig.KeyLifecycleRecertifyDaysSuffix),
				"type":  "integer",
				"title": title + " Tier: Recertify Every (days)",
				"description": fmt.Sprintf("Days from certification to the recertification deadline. The agent "+
					"becomes due 30 days before it. 0 sets no deadline. From 0 to %d.", agentlifecycle.MaxRecertifyDays),
				"section": section,
				"default": int(requirement.RecertifyEvery / agentlifecycle.Day),
				"minimum": 0,
				"maximum": agentlifecycle.MaxRecertifyDays,
			},
			map[string]any{
				"key":         platformconfig.LifecycleKey(tier, platformconfig.KeyLifecycleAttestationsSuffix),
				"type":        "array",
				"items":       map[string]any{"type": "string", "enum": mandatoryAttestationKinds()},
				"title":       title + " Tier: Mandatory Attestations",
				"description": "Attestation kinds a change request needs, valid and not revoked, before it can be approved.",
				"section":     section,
				"default":     attestations,
				"maxItems":    len(mandatoryAttestationKinds()),
			},
		)
	}
	fields = append(fields, map[string]any{
		"key":   platformconfig.KeyLifecycleLapseAction,
		"type":  "string",
		"enum":  []string{string(agentlifecycle.LapseWarn), string(agentlifecycle.LapseSuspend)},
		"title": "When a Certification Lapses",
		"description": "warn shows the lapse and lets the agent run. suspend stops every run of the agent " +
			"until it is recertified.",
		"section": section,
		"default": string(defaults.LapseAction),
	})
	return map[string]any{
		"id":    section,
		"title": "Agent Lifecycle",
		"description": "What each risk tier requires before an agent release is published: approvals from people " +
			"other than its author, a governance role holder among them, mandatory attestations, and how often " +
			"the release must be recertified. A tier can be raised above the platform minimums, never lowered " +
			"below them.",
		"order":               8,
		"icon":                "verified_user",
		"always_visible":      true,
		"required_permission": "configuration.governance",
		"fields":              fields,
	}
}

// validateAgentLifecycleValues applies the ranges the schema cannot
// enforce: integrality, the per-tier floor and known attestation kinds.
func validateAgentLifecycleValues(section string, values map[string]any) string {
	if section != platformconfig.SectionAgentLifecycle {
		return ""
	}
	for _, tier := range agentlifecycle.Tiers {
		floor := agentlifecycle.Floors[tier]
		bounds := []struct {
			key            string
			minimum, limit int64
		}{
			{platformconfig.LifecycleKey(tier, platformconfig.KeyLifecycleApprovalsSuffix), int64(floor.Approvals), agentlifecycle.MaxApprovals},
			{platformconfig.LifecycleKey(tier, platformconfig.KeyLifecycleGovernanceApprovalsSuffix), int64(floor.GovernanceApprovals), agentlifecycle.MaxApprovals},
			{platformconfig.LifecycleKey(tier, platformconfig.KeyLifecycleRecertifyDaysSuffix), 0, agentlifecycle.MaxRecertifyDays},
		}
		for _, b := range bounds {
			raw, present := values[b.key]
			if !present {
				continue
			}
			number, ok := raw.(float64)
			if !ok || number != float64(int64(number)) || int64(number) < b.minimum || int64(number) > b.limit {
				return fmt.Sprintf("%q must be a whole number from %d to %d", b.key, b.minimum, b.limit)
			}
		}
		approvalsKey := platformconfig.LifecycleKey(tier, platformconfig.KeyLifecycleApprovalsSuffix)
		governanceKey := platformconfig.LifecycleKey(tier, platformconfig.KeyLifecycleGovernanceApprovalsSuffix)
		approvals, approvalsSet := values[approvalsKey].(float64)
		governance, governanceSet := values[governanceKey].(float64)
		if approvalsSet && governanceSet && governance > approvals {
			return fmt.Sprintf("%q must not be more than %q", governanceKey, approvalsKey)
		}
		attestationsKey := platformconfig.LifecycleKey(tier, platformconfig.KeyLifecycleAttestationsSuffix)
		if raw, present := values[attestationsKey]; present {
			entries, _ := raw.([]any)
			allowed := mandatoryAttestationKinds()
			for _, entry := range entries {
				text, _ := entry.(string)
				known := false
				for _, kind := range allowed {
					known = known || kind == text
				}
				if !known {
					return fmt.Sprintf("%q may hold only: %s", attestationsKey, strings.Join(allowed, ", "))
				}
			}
		}
	}
	return ""
}
