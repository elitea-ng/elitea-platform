package admin

import (
	"fmt"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/platformconfig"
)

// The delegation policy section (ADR-0044 decision 3, #1501). It is LIVE:
// Main reads it through a per-replica cache (platformconfig.CachedSection)
// every time it prepares an agent execution command, and a save on this
// replica drops the cache at once; other replicas see a save within
// platformconfig.DefaultSectionTTL.

// WithDelegationCache supplies the cache a delegation save drops.
func WithDelegationCache(cache SectionInvalidator) Option {
	return func(h *Handler) {
		if cache != nil {
			h.delegation = cache
		}
	}
}

func (h *Handler) invalidateDelegation(section string) {
	if section == platformconfig.SectionDelegation && h.delegation != nil {
		h.delegation.Invalidate()
	}
}

// delegationSection bounds what a delegated agent may do. It uses the
// permission of the other runtime governance sections (worker egress, run
// caps, configuration.governance), so no migration seeds a new one.
func delegationSection() map[string]any {
	const section = platformconfig.SectionDelegation
	defaults := platformconfig.DefaultDelegationSettings()
	return map[string]any{
		"id":    section,
		"title": "Delegation",
		"description": "Limits on delegation. When an agent calls a saved agent or pipeline, a pipeline node runs " +
			"one, or an agent starts a sub-agent, the delegated agent runs with only the toolkits, tools, " +
			"credentials, data classes and egress hosts that both it and its caller hold. A delegation that " +
			"needs more stops with DELEGATION_GRANT_MISSING, and one deeper than the maximum depth stops with " +
			"DELEGATION_DEPTH_EXCEEDED. The model reads the refusal, and the run's evidence records it.",
		"order":               6,
		"icon":                "account_tree",
		"always_visible":      true,
		"required_permission": "configuration.governance",
		"fields": []map[string]any{
			{
				"key":   platformconfig.KeyDelegationMaxDepth,
				"type":  "integer",
				"title": "Maximum Delegation Depth",
				"description": fmt.Sprintf("How many delegations deep a run may go. The root agent is depth 0, "+
					"and saved agents, pipelines and sub-agents count together. From %d to %d; the default is %d.",
					platformconfig.MinDelegationMaxDepth, platformconfig.MaxDelegationMaxDepth,
					platformconfig.DefaultDelegationMaxDepth),
				"section": section,
				"default": defaults.MaxDepth,
				"minimum": platformconfig.MinDelegationMaxDepth,
				"maximum": platformconfig.MaxDelegationMaxDepth,
			},
			{
				"key":   platformconfig.KeyDelegationMode,
				"type":  "string",
				"title": "Mode",
				"description": "enforce (the default) refuses a delegation that needs a grant its caller does " +
					"not hold. report records the missing grant on the delegation step and lets the delegated " +
					"agent run, so you can find the compositions to fix before you enforce. The depth is " +
					"enforced in both modes. Runs under the governed credential profile always enforce.",
				"section": section,
				"default": string(defaults.Mode),
				"enum":    []string{string(platformconfig.DelegationEnforce), string(platformconfig.DelegationReport)},
			},
		},
	}
}

// validateDelegationValues applies the range and the mode the schema cannot
// enforce (JSON numbers arrive as float64, so integrality too).
func validateDelegationValues(section string, values map[string]any) string {
	if section != platformconfig.SectionDelegation {
		return ""
	}
	if raw, present := values[platformconfig.KeyDelegationMaxDepth]; present &&
		!platformconfig.ValidDelegationMaxDepth(raw) {
		return fmt.Sprintf("%q must be a whole number from %d to %d", platformconfig.KeyDelegationMaxDepth,
			platformconfig.MinDelegationMaxDepth, platformconfig.MaxDelegationMaxDepth)
	}
	if raw, present := values[platformconfig.KeyDelegationMode]; present {
		mode, _ := raw.(string)
		switch platformconfig.DelegationMode(mode) {
		case platformconfig.DelegationEnforce, platformconfig.DelegationReport:
		default:
			return fmt.Sprintf("%q must be enforce or report", platformconfig.KeyDelegationMode)
		}
	}
	return ""
}
