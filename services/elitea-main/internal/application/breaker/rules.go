// Package breaker is the agent circuit breaker (ADR-0041 decision 2, issue
// #1498).
//
// The breaker is per agent and per project. The main.agent_breaker routine
// runs every minute, off the request path, and reads what the platform
// already records:
//
//   - run outcomes (elitea_runtime.execution_jobs, copied into the breaker's
//     own window, elitea_runtime.agent_breaker_runs);
//   - tool-call failures (elitea_runtime.run_steps, kind tool_call with
//     summary status error);
//   - spend (gateway.llm_request_logs by execution_id).
//
// Four rules, authored by the platform administrator per risk tier in the
// circuit_breaker platform-config section:
//
//	error_rate           more than P % of the last N ended runs failed
//	spend_per_hour       the agent's spend in the last hour is over a ceiling
//	tool_failure_streak  one tool failed N times in a row inside one run
//	runs_per_hour        more runs ended in the last hour than a ceiling
//
// An agent's registry record may carry an override that only tightens the
// tier default (CheckOverride; the routine applies the tighter of the two
// in Effective). A tripped rule writes a suspension of source breaker at
// agent scope (package safety): admission refuses the agent's runs with
// execution_suspended and its runs in flight are cancelled. Who may resume
// comes from the tier. The routine does not trip an agent that is already
// suspended, and after a resume it counts only runs that started after it.
package breaker

import (
	"fmt"
	"math"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/safety"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/agentlifecycle"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/platformconfig"
)

// Rule names a breaker rule. It is the `rule` label of
// elitea_breaker_trips_total and the `rule` of a suspension's detail.
type Rule string

const (
	RuleErrorRate         Rule = "error_rate"
	RuleSpendPerHour      Rule = "spend_per_hour"
	RuleToolFailureStreak Rule = "tool_failure_streak"
	RuleRunsPerHour       Rule = "runs_per_hour"
)

// AllRules lists the rules in the order the routine checks them: the two
// that measure the agent itself first, then the two a failing dependency
// can also cause.
var AllRules = []Rule{RuleSpendPerHour, RuleRunsPerHour, RuleToolFailureStreak, RuleErrorRate}

// dependencySensitive reports whether a failing dependency (a model
// provider outage, a down external API) can trip the rule for many agents
// at once. The storm limit applies to these rules only.
func (r Rule) dependencySensitive() bool {
	return r == RuleErrorRate || r == RuleToolFailureStreak
}

// Bounds of the limits. A zero limit is off.
const (
	MinErrorRatePercent = 1
	MaxErrorRatePercent = 99
	MinWindowRuns       = 5
	MaxWindowRuns       = 200
	MinToolStreak       = 2
	MaxToolStreak       = 100
	MaxRunsPerHour      = 1_000_000
	MaxSpendPerHourUSD  = 1_000_000
	MaxStormLimit       = 10_000
)

// Defaults. See DefaultSettings for why.
const (
	DefaultErrorRatePercent = 50
	DefaultWindowRuns       = 20
	DefaultToolStreak       = 5
	DefaultStormLimit       = 5
)

// Rules are the limits of one tier, or the effective limits of one agent.
// A zero limit is off.
type Rules struct {
	// ErrorRatePercent trips when more than this percentage of the last
	// ErrorRateWindowRuns ended runs failed.
	ErrorRatePercent int
	// ErrorRateWindowRuns is how many ended runs the error rate reads. The
	// rule needs a full window: an agent with fewer runs never trips it.
	ErrorRateWindowRuns int
	// SpendPerHourUSD trips when the agent's spend in the last hour is
	// over it.
	SpendPerHourUSD float64
	// ToolFailureStreak trips when one tool failed this many times in a row
	// inside one run that ended in the last hour.
	ToolFailureStreak int
	// RunsPerHour trips when more runs than this ended in the last hour.
	RunsPerHour int
}

// TierSettings are one tier's rules and its resume requirement.
type TierSettings struct {
	Rules          Rules
	ResumeRequires safety.ResumeRequirement
}

// Settings is the circuit_breaker section.
type Settings struct {
	// Enabled runs the routine's evaluation. Off, the routine records
	// nothing and trips nothing.
	Enabled bool
	// StormLimit is the most agents the error_rate and tool_failure_streak
	// rules may trip in one pass. When more would trip, a dependency (a
	// model provider, an external API) is the likely cause, not the agents:
	// the pass trips none of them and counts a storm instead. 0 is no limit.
	StormLimit int
	Tiers      map[agentlifecycle.Tier]TierSettings
}

// DefaultSettings is a deployment that never saved the section.
//
//   - Enabled: on. The ADR places the breaker on the scheduler, one job, off
//     the request path; its defaults trip only on clear misbehaviour (more
//     than half of 20 runs failing, or one tool failing five times in a row
//     in one run), and the storm limit keeps a provider outage from stopping
//     every agent. An install that wants no automatic stop turns it off.
//   - error_rate: over 50 % of the last 20 ended runs (the ADR default).
//   - tool_failure_streak: 5. A model that retries a failing tool twice and
//     then changes course is normal; five calls of the same failing tool in
//     a row is a loop that is not recovering, and each call may have side
//     effects. The limit is far below the 200 tool calls of a run cap.
//   - spend_per_hour and runs_per_hour: off. A ceiling depends on the
//     install's prices and traffic; no default fits every install.
//   - resume: low and medium any_admin, high governance_role, critical
//     two_person.
func DefaultSettings() Settings {
	base := Rules{
		ErrorRatePercent: DefaultErrorRatePercent, ErrorRateWindowRuns: DefaultWindowRuns,
		ToolFailureStreak: DefaultToolStreak,
	}
	return Settings{
		Enabled:    true,
		StormLimit: DefaultStormLimit,
		Tiers: map[agentlifecycle.Tier]TierSettings{
			agentlifecycle.TierLow:      {Rules: base, ResumeRequires: safety.ResumeAnyAdmin},
			agentlifecycle.TierMedium:   {Rules: base, ResumeRequires: safety.ResumeAnyAdmin},
			agentlifecycle.TierHigh:     {Rules: base, ResumeRequires: safety.ResumeGovernanceRole},
			agentlifecycle.TierCritical: {Rules: base, ResumeRequires: safety.ResumeTwoPerson},
		},
	}
}

// Tier answers a tier's settings; an unknown tier is low.
func (s Settings) Tier(tier agentlifecycle.Tier) TierSettings {
	if found, ok := s.Tiers[tier]; ok {
		return found
	}
	return s.Tiers[agentlifecycle.TierLow]
}

// TierKey is the section key of one tier's field.
func TierKey(tier agentlifecycle.Tier, suffix string) string { return string(tier) + "_" + suffix }

// SettingsFromValues overlays the stored section on the defaults. A value
// of the wrong type or out of range keeps the default: the write path
// validates every save (ValidateValues), so such a value arrived by hand.
func SettingsFromValues(values platformconfig.Values) Settings {
	settings := DefaultSettings()
	settings.Enabled = values.Bool(platformconfig.KeyBreakerEnabled, settings.Enabled)
	window := DefaultWindowRuns
	if n, ok := values.Int(platformconfig.KeyBreakerErrorRateWindowRuns); ok && n >= MinWindowRuns && n <= MaxWindowRuns {
		window = int(n)
	}
	if n, ok := values.Int(platformconfig.KeyBreakerStormLimit); ok && n >= 0 && n <= MaxStormLimit {
		settings.StormLimit = int(n)
	}
	for _, tier := range agentlifecycle.Tiers {
		current := settings.Tiers[tier]
		current.Rules.ErrorRateWindowRuns = window
		if n, ok := values.Int(TierKey(tier, platformconfig.KeyBreakerErrorRatePercentSfx)); ok && (n == 0 || (n >= MinErrorRatePercent && n <= MaxErrorRatePercent)) {
			current.Rules.ErrorRatePercent = int(n)
		}
		if raw, present := values[TierKey(tier, platformconfig.KeyBreakerSpendPerHourUSDSfx)]; present {
			if usd, ok := raw.(float64); ok && usd >= 0 && usd <= MaxSpendPerHourUSD && !math.IsNaN(usd) {
				current.Rules.SpendPerHourUSD = usd
			}
		}
		if n, ok := values.Int(TierKey(tier, platformconfig.KeyBreakerToolFailureStreakSfx)); ok && (n == 0 || (n >= MinToolStreak && n <= MaxToolStreak)) {
			current.Rules.ToolFailureStreak = int(n)
		}
		if n, ok := values.Int(TierKey(tier, platformconfig.KeyBreakerRunsPerHourSfx)); ok && n >= 0 && n <= MaxRunsPerHour {
			current.Rules.RunsPerHour = int(n)
		}
		if requires := safety.ResumeRequirement(values.String(TierKey(tier, platformconfig.KeyBreakerResumeRequiresSfx), "")); requires.Valid() {
			current.ResumeRequires = requires
		}
		settings.Tiers[tier] = current
	}
	return settings
}

// ValidateValues answers why a save of the section is refused, or "".
func ValidateValues(values map[string]any) string {
	integer := func(key string, lowest, highest int64, zeroIsOff bool) string {
		raw, present := values[key]
		if !present {
			return ""
		}
		number, ok := raw.(float64)
		if !ok || number != math.Trunc(number) {
			return fmt.Sprintf("%q must be a whole number", key)
		}
		if zeroIsOff && number == 0 {
			return ""
		}
		if int64(number) < lowest || int64(number) > highest {
			if zeroIsOff {
				return fmt.Sprintf("%q must be 0 (off) or from %d to %d", key, lowest, highest)
			}
			return fmt.Sprintf("%q must be from %d to %d", key, lowest, highest)
		}
		return ""
	}
	if raw, present := values[platformconfig.KeyBreakerEnabled]; present {
		if _, ok := raw.(bool); !ok {
			return fmt.Sprintf("%q must be true or false", platformconfig.KeyBreakerEnabled)
		}
	}
	if reason := integer(platformconfig.KeyBreakerErrorRateWindowRuns, MinWindowRuns, MaxWindowRuns, false); reason != "" {
		return reason
	}
	if reason := integer(platformconfig.KeyBreakerStormLimit, 0, MaxStormLimit, false); reason != "" {
		return reason
	}
	for _, tier := range agentlifecycle.Tiers {
		if reason := integer(TierKey(tier, platformconfig.KeyBreakerErrorRatePercentSfx), MinErrorRatePercent, MaxErrorRatePercent, true); reason != "" {
			return reason
		}
		if reason := integer(TierKey(tier, platformconfig.KeyBreakerToolFailureStreakSfx), MinToolStreak, MaxToolStreak, true); reason != "" {
			return reason
		}
		if reason := integer(TierKey(tier, platformconfig.KeyBreakerRunsPerHourSfx), 1, MaxRunsPerHour, true); reason != "" {
			return reason
		}
		key := TierKey(tier, platformconfig.KeyBreakerSpendPerHourUSDSfx)
		if raw, present := values[key]; present {
			usd, ok := raw.(float64)
			if !ok || usd < 0 || usd > MaxSpendPerHourUSD || math.IsNaN(usd) {
				return fmt.Sprintf("%q must be 0 (off) or a USD amount up to %d", key, MaxSpendPerHourUSD)
			}
		}
		key = TierKey(tier, platformconfig.KeyBreakerResumeRequiresSfx)
		if raw, present := values[key]; present {
			text, _ := raw.(string)
			if !safety.ResumeRequirement(text).Valid() {
				return fmt.Sprintf("%q must be any_admin, governance_role or two_person", key)
			}
		}
	}
	return ""
}

// tightInt is the tighter of two limits where 0 is off and lower is tighter.
func tightInt(a, b int) int {
	switch {
	case a == 0:
		return b
	case b == 0:
		return a
	}
	return min(a, b)
}

func tightFloat(a, b float64) float64 {
	switch {
	case a == 0:
		return b
	case b == 0:
		return a
	}
	return math.Min(a, b)
}

// Effective is the tighter of the tier's rules and the agent's override:
// a later change of the tier default can never loosen an override, and an
// override can never loosen the tier.
func Effective(tier Rules, override Override) Rules {
	out := tier
	if override.ErrorRatePercent != nil {
		out.ErrorRatePercent = tightInt(out.ErrorRatePercent, *override.ErrorRatePercent)
	}
	if override.ErrorRateWindowRuns != nil && *override.ErrorRateWindowRuns > 0 {
		out.ErrorRateWindowRuns = min(out.ErrorRateWindowRuns, *override.ErrorRateWindowRuns)
	}
	if override.SpendPerHourUSD != nil {
		out.SpendPerHourUSD = tightFloat(out.SpendPerHourUSD, *override.SpendPerHourUSD)
	}
	if override.ToolFailureStreak != nil {
		out.ToolFailureStreak = tightInt(out.ToolFailureStreak, *override.ToolFailureStreak)
	}
	if override.RunsPerHour != nil {
		out.RunsPerHour = tightInt(out.RunsPerHour, *override.RunsPerHour)
	}
	return out
}
