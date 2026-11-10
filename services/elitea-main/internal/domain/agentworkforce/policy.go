package agentworkforce

// The project's automatic hiring and dismissal policies (ADR-0050 decisions
// 5c and 5k, issue 1617). Both are OFF by default. An agent never decides
// either: the platform acts under a policy a person (an agent manager) set,
// and every action goes through the ladder of this package, audited with the
// actor policy:<rule>.
//
// AUTO HIRE. An open agent proposal (shared/0234) becomes an agent without a
// click when ALL of these hold (CheckHire):
//   - the suggested tier of its proposed tools is low: every tool is
//     read-only (a wildcard or an undeclared tool reads as write);
//   - its spec ran at least MinSuccessfulRuns times to completion, and never
//     failed or ran out of budget;
//   - the project holds fewer than MaxAutoHired automatically hired agents
//     nobody has reviewed yet.
// The agent is created unpublished, with NO tool bound (binding a toolkit is
// a person's step, where the gates apply), owned by the person the proposing
// run served, on probation; an agent manager can undo the hire for
// ProbationWindow, or keep it (which ends the probation).
//
// AUTO DISMISS. Rules map scorecard conditions to watch, suspend or retire
// (AutomaticStage); demote and delete are never automatic. watch and suspend
// apply at once; retire posts a notice and applies after GraceDays unless the
// owner or an agent manager objects. A critical-tier agent and an agent
// under legal hold are exempt (ExemptFromAutomatic). An agent whose owners
// left the project is flagged for reassignment and moved to watch after
// OrphanWatchAfter; it is never retired automatically.

import (
	"fmt"
	"regexp"
	"slices"
	"time"
)

const (
	// ProbationWindow is how long an automatic hire can be undone.
	ProbationWindow = 14 * 24 * time.Hour
	// OrphanWatchAfter is how long an orphaned agent waits for a new owner
	// before it moves to watch.
	OrphanWatchAfter = 30 * 24 * time.Hour
	// SuppressAfterObjection is how long a rule stays quiet for an agent
	// after a person objected to, or undid, what it did.
	SuppressAfterObjection = 30 * 24 * time.Hour

	DefaultMinSuccessfulRuns = 3
	DefaultMaxAutoHired      = 5
	DefaultGraceDays         = 7
	MaxRules                 = 20
	MaxConditions            = 8

	// Rule names of the actions the platform takes outside a project's
	// rules.
	RuleAutoHire = "auto_hire"
	RuleOrphan   = "orphaned"
)

// Kinds of automatic action (agent_automatic_actions.kind).
const (
	ActionAutoHire    = "auto_hire"
	ActionAutoDismiss = "auto_dismiss"
	ActionOrphan      = "orphan"
)

// Statuses of an automatic action.
const (
	ActionPending   = "pending"
	ActionFlagged   = "flagged"
	ActionApplied   = "applied"
	ActionHeld      = "held"
	ActionObjected  = "objected"
	ActionUndone    = "undone"
	ActionKept      = "kept"
	ActionResolved  = "resolved"
	ActionCancelled = "cancelled"
)

// Refusal codes of the policy routes.
const (
	PolicyInvalidCode = "workforce_policy_invalid"
	ActionClosedCode  = "workforce_action_closed"
)

// The metrics a condition may read: the scorecard's (5h) and two facts of
// the registry.
const (
	MetricRuns30d               = "runs_30d"
	MetricFailed30d             = "failed_30d"
	MetricCapReached30d         = "cap_reached_30d"
	MetricFailureRate30d        = "failure_rate_30d"
	MetricBreakerTrips7d        = "breaker_trips_7d"
	MetricApprovalRejections30d = "approval_rejections_30d"
	MetricGuardrailHits30d      = "guardrail_hits_30d"
	MetricSpendRatio            = "spend_ratio"
	MetricThumbsDown30d         = "thumbs_down_30d"
	MetricUnusedDays            = "unused_days"
	// MetricAutoHired is 1 for an automatically hired agent, else 0.
	MetricAutoHired = "auto_hired"
	// MetricProbationFailed is 1 for an automatically hired agent still on
	// probation whose scorecard failed it (ProbationFailed), else 0.
	MetricProbationFailed = "probation_failed"
)

// Metrics lists every metric a condition may name.
var Metrics = []string{
	MetricRuns30d, MetricFailed30d, MetricCapReached30d, MetricFailureRate30d, MetricBreakerTrips7d,
	MetricApprovalRejections30d, MetricGuardrailHits30d, MetricSpendRatio, MetricThumbsDown30d,
	MetricUnusedDays, MetricAutoHired, MetricProbationFailed,
}

// Operators lists the comparison operators.
var Operators = []string{">=", ">", "<=", "<", "="}

var ruleIDPattern = regexp.MustCompile(`^[a-z][a-z0-9_]{0,39}$`)

// ValidRuleID reports a rule id (also the audit's policy:<rule>).
func ValidRuleID(id string) bool { return ruleIDPattern.MatchString(id) }

// Condition compares one metric with a value.
type Condition struct {
	Metric string  `json:"metric"`
	Op     string  `json:"op"`
	Value  float64 `json:"value"`
}

// Rule maps conditions (all must hold) to a stage.
type Rule struct {
	ID         string      `json:"id"`
	Conditions []Condition `json:"conditions"`
	Stage      Stage       `json:"stage"`
}

// Policy is a project's workforce policy.
type Policy struct {
	AutoHireEnabled    bool   `json:"auto_hire_enabled"`
	MinSuccessfulRuns  int    `json:"min_successful_runs"`
	MaxAutoHired       int    `json:"max_auto_hired"`
	AutoDismissEnabled bool   `json:"auto_dismiss_enabled"`
	Rules              []Rule `json:"auto_dismiss_rules"`
	GraceDays          int    `json:"grace_days"`
}

// DefaultRules are the platform's starting rules (ADR-0050 decision 5k).
func DefaultRules() []Rule {
	return []Rule{
		{ID: "breaker_trips", Conditions: []Condition{{MetricBreakerTrips7d, ">=", 3}}, Stage: StageSuspend},
		{ID: "failure_rate", Conditions: []Condition{{MetricFailureRate30d, ">", 0.5}, {MetricRuns30d, ">=", 20}}, Stage: StageWatch},
		{ID: "unused", Conditions: []Condition{{MetricUnusedDays, ">=", 90}}, Stage: StageRetire},
		{ID: "probation_failed", Conditions: []Condition{{MetricAutoHired, "=", 1}, {MetricProbationFailed, "=", 1}}, Stage: StageRetire},
	}
}

// DefaultPolicy is a project's policy without a row: everything off, the
// platform defaults for the numbers and rules.
func DefaultPolicy() Policy {
	return Policy{
		MinSuccessfulRuns: DefaultMinSuccessfulRuns, MaxAutoHired: DefaultMaxAutoHired,
		Rules: DefaultRules(), GraceDays: DefaultGraceDays,
	}
}

// AutomaticStage reports a stage the policy may take: watch, suspend or
// retire. Demote is a judgement, delete is irreversible, and nothing goes
// past retire.
func AutomaticStage(stage Stage) bool {
	return stage == StageWatch || stage == StageSuspend || stage == StageRetire
}

// CheckPolicy bounds a policy and answers it canonical.
func CheckPolicy(p Policy) (Policy, error) {
	invalid := func(format string, args ...any) error {
		return Refuse(PolicyInvalidCode, fmt.Sprintf(format, args...))
	}
	if p.MinSuccessfulRuns < 1 || p.MinSuccessfulRuns > 100 {
		return Policy{}, invalid("min_successful_runs is between 1 and 100.")
	}
	if p.MaxAutoHired < 1 || p.MaxAutoHired > 50 {
		return Policy{}, invalid("max_auto_hired is between 1 and 50.")
	}
	if p.GraceDays < 1 || p.GraceDays > 90 {
		return Policy{}, invalid("grace_days is between 1 and 90.")
	}
	if len(p.Rules) > MaxRules {
		return Policy{}, invalid("At most %d rules.", MaxRules)
	}
	if p.Rules == nil {
		p.Rules = []Rule{}
	}
	seen := map[string]bool{}
	for i, rule := range p.Rules {
		if !ValidRuleID(rule.ID) || rule.ID == RuleAutoHire || rule.ID == RuleOrphan {
			return Policy{}, invalid("A rule id is lower-case letters, digits and _ (at most 40), and not auto_hire or orphaned.")
		}
		if seen[rule.ID] {
			return Policy{}, invalid("The rule id %s is used twice.", rule.ID)
		}
		seen[rule.ID] = true
		if !AutomaticStage(rule.Stage) {
			return Policy{}, invalid("A rule's stage is watch, suspend or retire: demote and delete are never automatic.")
		}
		if len(rule.Conditions) == 0 || len(rule.Conditions) > MaxConditions {
			return Policy{}, invalid("A rule has 1 to %d conditions.", MaxConditions)
		}
		for _, condition := range rule.Conditions {
			if !slices.Contains(Metrics, condition.Metric) {
				return Policy{}, invalid("Unknown metric %q.", condition.Metric)
			}
			if !slices.Contains(Operators, condition.Op) {
				return Policy{}, invalid("A condition's op is >=, >, <=, < or =.")
			}
			if condition.Value < 0 || condition.Value > 1e9 {
				return Policy{}, invalid("A condition's value is between 0 and 1e9.")
			}
		}
		p.Rules[i].Conditions = slices.Clone(rule.Conditions)
	}
	return p, nil
}

// Facts are the numbers a rule reads about one agent.
type Facts map[string]float64

// FactsOf builds the facts from a scorecard and the registry. unusedDays is
// the scorecard's, or the days since the registry row for an agent never
// used.
func FactsOf(card Scorecard, unusedDays int, autoHired, probationFailed bool) Facts {
	ratio := 0.0
	if card.ProjectMedianSpendUSD > 0 {
		ratio = card.Spend30dUSD / card.ProjectMedianSpendUSD
	}
	return Facts{
		MetricRuns30d: float64(card.Runs30d), MetricFailed30d: float64(card.Failed30d),
		MetricCapReached30d: float64(card.CapReached30d), MetricFailureRate30d: card.FailureRate30d,
		MetricBreakerTrips7d: float64(card.BreakerTrips7d), MetricApprovalRejections30d: float64(card.ApprovalRejections30d),
		MetricGuardrailHits30d: float64(card.GuardrailHits30d), MetricSpendRatio: ratio,
		MetricThumbsDown30d: float64(card.ThumbsDown30d), MetricUnusedDays: float64(unusedDays),
		MetricAutoHired: boolValue(autoHired), MetricProbationFailed: boolValue(probationFailed),
	}
}

func boolValue(b bool) float64 {
	if b {
		return 1
	}
	return 0
}

// Matches reports whether every condition of the rule holds.
func (r Rule) Matches(facts Facts) bool {
	if len(r.Conditions) == 0 {
		return false
	}
	for _, condition := range r.Conditions {
		value, known := facts[condition.Metric]
		if !known || !compare(value, condition.Op, condition.Value) {
			return false
		}
	}
	return true
}

func compare(value float64, op string, target float64) bool {
	switch op {
	case ">=":
		return value >= target
	case ">":
		return value > target
	case "<=":
		return value <= target
	case "<":
		return value < target
	case "=":
		return value == target
	}
	return false
}

// ProbationFailed reports whether an agent on probation failed it on its
// scorecard: a breaker trip, at least 3 guardrail hits, or more than half of
// at least 3 judged runs failing.
func ProbationFailed(card Scorecard) bool {
	judged := card.Runs30d - card.CapReached30d
	return card.BreakerTrips7d > 0 || card.GuardrailHits30d >= 3 || (judged >= 3 && card.FailureRate30d > 0.5)
}

// Matching answers every automatic rule that matches, strongest stage first
// and in policy order among equals. The pass takes the first one it may
// apply, so a suppressed (objected) retire rule does not hide a suspend
// rule that also matches.
func Matching(rules []Rule, facts Facts) []Rule {
	var out []Rule
	for _, rule := range rules {
		if AutomaticStage(rule.Stage) && rule.Matches(facts) {
			out = append(out, rule)
		}
	}
	slices.SortStableFunc(out, func(a, b Rule) int { return stageRank(b.Stage) - stageRank(a.Stage) })
	return out
}

func stageRank(stage Stage) int { return slices.Index(Stages, stage) }

// Suggested tiers from the ADR-0041 tool categories.
const (
	TierLow    = "low"
	TierMedium = "medium"
	TierHigh   = "high"
)

// SuggestedTier answers the tier a set of tool categories suggests: low when
// every tool is read-only, high when one sends outside, pays or runs code,
// medium otherwise (a write or a delete). No tool is low.
func SuggestedTier(categories []string) string {
	tier := TierLow
	for _, category := range categories {
		switch category {
		case "read":
		case "external_send", "payment", "code_execution":
			return TierHigh
		default:
			tier = TierMedium
		}
	}
	return tier
}

// HireEvidence is what CheckHire reads about one proposal.
type HireEvidence struct {
	ToolCategories []string
	// Successful and Failed count the spec's spawns in the project by the
	// proposing agent: completed, and failed or out of budget.
	Successful int
	Failed     int
	// OpenAutoHired counts the project's automatically hired agents nobody
	// has reviewed yet.
	OpenAutoHired int
	// Undone: an agent manager undid an automatic hire of the same spec in
	// the last SuppressAfterObjection.
	Undone bool
}

// Reasons CheckHire gives for leaving a proposal to a person.
const (
	HireNotLowTier    = "not_low_tier"
	HireFewRuns       = "too_few_successful_runs"
	HireFailedRun     = "failed_or_capped_run"
	HireCapReached    = "max_auto_hired_reached"
	HireUndoneRecent  = "undone_recently"
	HirePolicyDisable = "auto_hire_off"
)

// CheckHire answers "" when the proposal may be hired automatically, or the
// reason it waits for a person.
func CheckHire(policy Policy, evidence HireEvidence) string {
	switch {
	case !policy.AutoHireEnabled:
		return HirePolicyDisable
	case SuggestedTier(evidence.ToolCategories) != TierLow:
		return HireNotLowTier
	case evidence.Failed > 0:
		return HireFailedRun
	case evidence.Successful < policy.MinSuccessfulRuns:
		return HireFewRuns
	case evidence.OpenAutoHired >= policy.MaxAutoHired:
		return HireCapReached
	case evidence.Undone:
		return HireUndoneRecent
	}
	return ""
}
