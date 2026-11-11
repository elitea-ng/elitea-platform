package admin

import (
	"fmt"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/proactivity"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/platformconfig"
)

// The proactivity section: the platform switch, the maximums of each
// agent's daily envelope and the personal-project rule (ADR-0050 decision
// 2, #1618). It is LIVE: the agent.heartbeat.scan.v1 job reads it at every
// pass, and the heartbeat settings route reads personal_self_approve.

// proactivitySection uses the permission of the other runtime
// governance sections, so no migration seeds a new one.
func proactivitySection() map[string]any {
	const section = platformconfig.SectionProactivity
	return map[string]any{
		"id":    section,
		"title": "Proactive agents",
		"description": "Proactive agents wake up on a timer, check their goals and the new messages, and then " +
			"do nothing, notify their owner or start a run. These settings are the platform maximums. An " +
			"agent's own daily limits can only be lower. A run that an agent starts always uses its owner's " +
			"mandate, waits for a person at every approval and never gets a sensitive tool grant.",
		"order":               8,
		"icon":                "alarm",
		"always_visible":      true,
		"required_permission": "configuration.governance",
		"fields": []map[string]any{
			{
				"key":   platformconfig.KeyProactivityEnabled,
				"type":  "boolean",
				"title": "Allow Proactive Agents",
				"description": "Off stops every heartbeat on the platform at its next wake. The settings of each " +
					"agent stay.",
				"section": section,
				"default": true,
			},
			{
				"key":   platformconfig.KeyProactivityMaxWakesPerDay,
				"type":  "integer",
				"title": "Maximum Wakes per Day",
				"description": fmt.Sprintf("The most wakes of one agent in one day, from 1 to %d.",
					proactivity.MaxWakesPerDay),
				"section": section,
				"default": platformconfig.DefaultProactivityMaxWakesPerDay,
				"minimum": 1,
				"maximum": proactivity.MaxWakesPerDay,
			},
			{
				"key":   platformconfig.KeyProactivityMaxActsPerDay,
				"type":  "integer",
				"title": "Maximum Runs per Day",
				"description": fmt.Sprintf("The most runs that one agent can start in one day, from 0 to %d. "+
					"0 lets agents notify but never act.", proactivity.MaxActsPerDay),
				"section": section,
				"default": platformconfig.DefaultProactivityMaxActsPerDay,
				"minimum": 0,
				"maximum": proactivity.MaxActsPerDay,
			},
			{
				"key":   platformconfig.KeyProactivityMaxSpendPerDayUSD,
				"type":  "number",
				"title": "Maximum Spend per Day (USD)",
				"description": fmt.Sprintf("The most that one agent can spend in one day on its triage and "+
					"the runs it starts, from 0 to %g.", proactivity.MaxSpendPerDayUSD),
				"section": section,
				"default": platformconfig.DefaultProactivityMaxSpendPerDayUSD,
				"minimum": 0,
				"maximum": proactivity.MaxSpendPerDayUSD,
			},
			{
				"key":   platformconfig.KeyProactivityPersonalSelfApprove,
				"type":  "boolean",
				"title": "Personal Projects Approve Their Own Agents",
				"description": "On (the default): the one member of a personal project can turn on an agent's " +
					"heartbeat without a review. Off: a personal-project agent must be at least medium risk, " +
					"which needs a second person, so personal agents cannot be proactive.",
				"section": section,
				"default": true,
			},
			{
				"key":   platformconfig.KeyProactivityPersonalActsPerDay,
				"type":  "integer",
				"title": "Runs per Day in a Personal Project",
				"description": fmt.Sprintf("The most runs that one agent of a personal project can start in "+
					"one day, from 0 to %d.", proactivity.MaxActsPerDay),
				"section": section,
				"default": platformconfig.DefaultProactivityPersonalActsPerDay,
				"minimum": 0,
				"maximum": proactivity.MaxActsPerDay,
			},
		},
	}
}

// validateProactivityValues applies the ranges that the schema cannot
// enforce.
func validateProactivityValues(section string, values map[string]any) string {
	if section != platformconfig.SectionProactivity {
		return ""
	}
	for _, key := range []string{platformconfig.KeyProactivityEnabled, platformconfig.KeyProactivityPersonalSelfApprove} {
		if raw, present := values[key]; present && raw != nil {
			if _, ok := raw.(bool); !ok {
				return fmt.Sprintf("%q must be true or false", key)
			}
		}
	}
	whole := []struct {
		key       string
		low, high int
	}{
		{platformconfig.KeyProactivityMaxWakesPerDay, 1, proactivity.MaxWakesPerDay},
		{platformconfig.KeyProactivityMaxActsPerDay, 0, proactivity.MaxActsPerDay},
		{platformconfig.KeyProactivityPersonalActsPerDay, 0, proactivity.MaxActsPerDay},
	}
	for _, field := range whole {
		if raw, present := values[field.key]; present && raw != nil &&
			!platformconfig.ValidProactivityWhole(raw, field.low, field.high) {
			return fmt.Sprintf("%q must be a whole number from %d to %d", field.key, field.low, field.high)
		}
	}
	if raw, present := values[platformconfig.KeyProactivityMaxSpendPerDayUSD]; present && raw != nil &&
		!platformconfig.ValidProactivitySpend(raw) {
		return fmt.Sprintf("%q must be a number from 0 to %g", platformconfig.KeyProactivityMaxSpendPerDayUSD,
			proactivity.MaxSpendPerDayUSD)
	}
	return ""
}
