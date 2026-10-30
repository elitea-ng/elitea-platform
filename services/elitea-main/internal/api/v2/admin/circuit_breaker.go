package admin

import (
	"fmt"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/breaker"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/safety"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/agentlifecycle"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/platformconfig"
)

// The circuit breaker section (ADR-0041 decision 2, #1498). It is LIVE: the
// main.agent_breaker routine reads it at every pass (every minute), so a
// save applies at the next pass. It uses configuration.governance, the
// permission of the other runtime governance sections, so no migration
// seeds a new one.

func circuitBreakerSection() map[string]any {
	const section = platformconfig.SectionCircuitBreaker
	defaults := breaker.DefaultSettings()
	fields := []map[string]any{
		{
			"key":   platformconfig.KeyBreakerEnabled,
			"type":  "boolean",
			"title": "Circuit Breaker On",
			"description": "Every minute, the main.agent_breaker routine checks each agent that had runs against " +
				"the rules below. A rule that trips stops the agent: its new runs are refused with " +
				"execution_suspended and its runs in flight are cancelled, until an administrator resumes it.",
			"section": section,
			"default": defaults.Enabled,
		},
		{
			"key":   platformconfig.KeyBreakerErrorRateWindowRuns,
			"type":  "integer",
			"title": "Error Rate Window (runs)",
			"description": fmt.Sprintf("How many of the agent's latest ended runs the error rate reads. An agent "+
				"with fewer runs since its last resume is not judged. From %d to %d.", breaker.MinWindowRuns, breaker.MaxWindowRuns),
			"section": section,
			"default": breaker.DefaultWindowRuns,
			"minimum": breaker.MinWindowRuns,
			"maximum": breaker.MaxWindowRuns,
		},
		{
			"key":   platformconfig.KeyBreakerStormLimit,
			"type":  "integer",
			"title": "Storm Limit (agents per pass)",
			"description": "When more agents than this would trip on the error rate or the tool failure rule in " +
				"one pass, a shared dependency (a model provider, an external API) is the likely cause: the " +
				"pass stops none of them and raises the EliteaAgentBreakerStorm alert. 0 is no limit.",
			"section": section,
			"default": breaker.DefaultStormLimit,
			"minimum": 0,
			"maximum": breaker.MaxStormLimit,
		},
	}
	for _, tier := range agentlifecycle.Tiers {
		title := lifecycleTierTitles[tier]
		rules := defaults.Tier(tier)
		fields = append(fields,
			map[string]any{
				"key":   breaker.TierKey(tier, platformconfig.KeyBreakerErrorRatePercentSfx),
				"type":  "integer",
				"title": title + " Tier: Error Rate (%)",
				"description": "Trips when more than this percentage of the window's runs failed. 0 turns the " +
					"rule off.",
				"section": section,
				"default": rules.Rules.ErrorRatePercent,
				"minimum": 0,
				"maximum": breaker.MaxErrorRatePercent,
			},
			map[string]any{
				"key":   breaker.TierKey(tier, platformconfig.KeyBreakerSpendPerHourUSDSfx),
				"type":  "number",
				"title": title + " Tier: Spend per Hour (USD)",
				"description": "Trips when the agent's model and metered tool spend in the last hour is over " +
					"this amount. 0 turns the rule off.",
				"section": section,
				"default": rules.Rules.SpendPerHourUSD,
				"minimum": 0,
				"maximum": breaker.MaxSpendPerHourUSD,
			},
			map[string]any{
				"key":   breaker.TierKey(tier, platformconfig.KeyBreakerToolFailureStreakSfx),
				"type":  "integer",
				"title": title + " Tier: Same Tool Failing in a Row",
				"description": "Trips when one tool fails this many times in a row inside one run. 0 turns the " +
					"rule off.",
				"section": section,
				"default": rules.Rules.ToolFailureStreak,
				"minimum": 0,
				"maximum": breaker.MaxToolStreak,
			},
			map[string]any{
				"key":   breaker.TierKey(tier, platformconfig.KeyBreakerRunsPerHourSfx),
				"type":  "integer",
				"title": title + " Tier: Runs per Hour",
				"description": "Trips when more runs of the agent than this ended in the last hour. 0 turns " +
					"the rule off.",
				"section": section,
				"default": rules.Rules.RunsPerHour,
				"minimum": 0,
				"maximum": breaker.MaxRunsPerHour,
			},
			map[string]any{
				"key":   breaker.TierKey(tier, platformconfig.KeyBreakerResumeRequiresSfx),
				"type":  "string",
				"title": title + " Tier: Who Can Resume",
				"description": "any_admin: a project or platform administrator. governance_role: a platform " +
					"administrator. two_person: two different platform administrators (the first records an " +
					"approval, the second resumes).",
				"section": section,
				"default": string(rules.ResumeRequires),
				"enum": []string{
					string(safety.ResumeAnyAdmin), string(safety.ResumeGovernanceRole), string(safety.ResumeTwoPerson),
				},
			},
		)
	}
	return map[string]any{
		"id":    section,
		"title": "Circuit Breaker",
		"description": "Stops an agent that misbehaves, by rule, per risk tier: error rate over a window of " +
			"runs, spend per hour, the same tool failing again and again inside one run, and runs per hour. " +
			"An agent's registry record can tighten these limits and never loosen them. Every trip is in the " +
			"audit trail (agent.breaker.tripped), notifies the agent's owners and the project admins, and " +
			"is a metric your alerting can page on.",
		"order":               7,
		"icon":                "bolt",
		"always_visible":      true,
		"required_permission": "configuration.governance",
		"fields":              fields,
	}
}

// validateCircuitBreakerValues applies the ranges and vocabularies the
// schema cannot enforce (JSON numbers arrive as float64, so integrality
// too).
func validateCircuitBreakerValues(section string, values map[string]any) string {
	if section != platformconfig.SectionCircuitBreaker {
		return ""
	}
	return breaker.ValidateValues(values)
}
