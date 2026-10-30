package breaker

import (
	"fmt"
	"math"
	"strconv"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/safety"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/agentlifecycle"
)

// AgentKey is one agent: application ids are per tenant schema, so the
// project is part of the key.
type AgentKey struct {
	ProjectID     int64
	ApplicationID int64
}

// Run states the window reads.
const (
	StateSucceeded = "SUCCEEDED"
	StateFailed    = "FAILED"
	StateCancelled = "CANCELLED"
)

// Observation is what the store read about one agent for one pass. Every
// count is of runs that STARTED after the agent's last resume, so a resume
// restarts the window.
type Observation struct {
	Key  AgentKey
	Tier agentlifecycle.Tier
	// Override is the agent's registry override.
	Override Override
	// Suspended: an active agent suspension covers the agent (any source).
	// The breaker does not trip it again.
	Suspended bool
	// Outcomes are the states of its latest ended runs, newest first, at
	// most the error-rate window; cancelled runs are left out (a cancel is
	// a person's or a cap's stop, not a failure of the agent).
	Outcomes []string
	// RunsLastHour counts its runs that ended in the last hour, in any
	// terminal state.
	RunsLastHour int
	// ToolStreak is the longest run of consecutive failures of one tool in
	// one of its runs that ended in the last hour, with that tool's name and
	// the run.
	ToolStreak      int
	ToolName        string
	ToolExecutionID string
	// SpendLastHourUSD is its spend in the last hour.
	SpendLastHourUSD float64
}

// Trip is one rule that tripped for one agent.
type Trip struct {
	Key            AgentKey
	Tier           agentlifecycle.Tier
	Rule           Rule
	ResumeRequires safety.ResumeRequirement
	// Observed and Threshold are in Unit: percent, usd,
	// consecutive_failures or runs.
	Observed  float64
	Threshold float64
	Unit      string
	// WindowRuns and FailedRuns are set for error_rate, WindowSeconds for
	// the hourly rules, ToolName and ExecutionID for tool_failure_streak.
	WindowRuns    int
	FailedRuns    int
	WindowSeconds int
	ToolName      string
	ExecutionID   string
}

// Units of Trip.Unit.
const (
	UnitPercent             = "percent"
	UnitUSD                 = "usd"
	UnitConsecutiveFailures = "consecutive_failures"
	UnitRuns                = "runs"
)

const hourSeconds = 3600

// Evaluate answers the first rule that trips for the observation, in
// AllRules order, or false.
func Evaluate(settings Settings, o Observation) (Trip, bool) {
	if o.Suspended {
		return Trip{}, false
	}
	tier := settings.Tier(o.Tier)
	rules := Effective(tier.Rules, o.Override)
	trip := Trip{Key: o.Key, Tier: o.Tier, ResumeRequires: tier.ResumeRequires}
	if !trip.Tier.Valid() {
		trip.Tier = agentlifecycle.TierLow
	}
	for _, rule := range AllRules {
		switch rule {
		case RuleSpendPerHour:
			if rules.SpendPerHourUSD > 0 && o.SpendLastHourUSD > rules.SpendPerHourUSD {
				trip.Rule, trip.Unit, trip.WindowSeconds = rule, UnitUSD, hourSeconds
				trip.Observed, trip.Threshold = roundCents(o.SpendLastHourUSD), rules.SpendPerHourUSD
				return trip, true
			}
		case RuleRunsPerHour:
			if rules.RunsPerHour > 0 && o.RunsLastHour > rules.RunsPerHour {
				trip.Rule, trip.Unit, trip.WindowSeconds = rule, UnitRuns, hourSeconds
				trip.Observed, trip.Threshold = float64(o.RunsLastHour), float64(rules.RunsPerHour)
				return trip, true
			}
		case RuleToolFailureStreak:
			if rules.ToolFailureStreak > 0 && o.ToolStreak >= rules.ToolFailureStreak {
				trip.Rule, trip.Unit = rule, UnitConsecutiveFailures
				trip.Observed, trip.Threshold = float64(o.ToolStreak), float64(rules.ToolFailureStreak)
				trip.ToolName, trip.ExecutionID = o.ToolName, o.ToolExecutionID
				return trip, true
			}
		case RuleErrorRate:
			window := rules.ErrorRateWindowRuns
			if rules.ErrorRatePercent <= 0 || window <= 0 || len(o.Outcomes) < window {
				continue
			}
			failed := 0
			for _, state := range o.Outcomes[:window] {
				if state == StateFailed {
					failed++
				}
			}
			// "More than P % of N": failed / N > P / 100, in integers.
			if failed*100 > rules.ErrorRatePercent*window {
				trip.Rule, trip.Unit = rule, UnitPercent
				trip.Observed = math.Round(float64(failed)*1000/float64(window)) / 10
				trip.Threshold = float64(rules.ErrorRatePercent)
				trip.WindowRuns, trip.FailedRuns = window, failed
				return trip, true
			}
		}
	}
	return Trip{}, false
}

func roundCents(usd float64) float64 { return math.Round(usd*100) / 100 }

// Reason is the suspension's reason: one sentence with the rule and the
// observed values, read on the admin console and in the agent's stopped
// chip.
func (t Trip) Reason() string {
	switch t.Rule {
	case RuleErrorRate:
		return fmt.Sprintf("Circuit breaker: %d of the last %d runs failed (%s %%), over the limit of %s %%.",
			t.FailedRuns, t.WindowRuns, number(t.Observed), number(t.Threshold))
	case RuleSpendPerHour:
		return fmt.Sprintf("Circuit breaker: the agent spent %.2f USD in the last hour, over the limit of %.2f USD.",
			t.Observed, t.Threshold)
	case RuleToolFailureStreak:
		return fmt.Sprintf("Circuit breaker: the tool %q failed %s times in a row in one run (limit %s).",
			truncate(t.ToolName, 128), number(t.Observed), number(t.Threshold))
	case RuleRunsPerHour:
		return fmt.Sprintf("Circuit breaker: %s runs ended in the last hour, over the limit of %s.",
			number(t.Observed), number(t.Threshold))
	}
	return "Circuit breaker: a rule tripped."
}

// Detail is the suspension's structured record of the trip
// (execution_suspensions.detail).
func (t Trip) Detail() map[string]any {
	detail := map[string]any{
		"rule": string(t.Rule), "observed": t.Observed, "threshold": t.Threshold, "unit": t.Unit,
		"tier": string(t.Tier),
	}
	if t.WindowRuns > 0 {
		detail["window_runs"] = t.WindowRuns
		detail["failed_runs"] = t.FailedRuns
	}
	if t.WindowSeconds > 0 {
		detail["window_seconds"] = t.WindowSeconds
	}
	if t.ToolName != "" {
		detail["tool_name"] = truncate(t.ToolName, 128)
	}
	if t.ExecutionID != "" {
		detail["execution_id"] = truncate(t.ExecutionID, 128)
	}
	return detail
}

// AuditReasons are the security audit event's reason codes: references and
// numbers only.
func (t Trip) AuditReasons(suspensionID int64) []string {
	reasons := []string{
		"rule:" + string(t.Rule),
		"observed:" + number(t.Observed),
		"threshold:" + number(t.Threshold),
		"unit:" + t.Unit,
		"tier:" + string(t.Tier),
		"resume_requires:" + string(t.ResumeRequires),
		"suspension:" + strconv.FormatInt(suspensionID, 10),
	}
	if t.WindowRuns > 0 {
		reasons = append(reasons, "window_runs:"+strconv.Itoa(t.WindowRuns), "failed_runs:"+strconv.Itoa(t.FailedRuns))
	}
	if t.WindowSeconds > 0 {
		reasons = append(reasons, "window_seconds:"+strconv.Itoa(t.WindowSeconds))
	}
	return reasons
}

func number(v float64) string { return strconv.FormatFloat(v, 'f', -1, 64) }

func truncate(text string, limit int) string {
	if len(text) <= limit {
		return text
	}
	cut := limit
	for cut > 0 && !utf8RuneStart(text[cut]) {
		cut--
	}
	return text[:cut]
}

func utf8RuneStart(b byte) bool { return b&0xC0 != 0x80 }
