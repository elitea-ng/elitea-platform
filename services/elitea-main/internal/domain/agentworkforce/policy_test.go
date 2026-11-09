package agentworkforce

import (
	"testing"
)

func TestDefaultPolicyIsOffAndItsRulesPassTheCheck(t *testing.T) {
	policy := DefaultPolicy()
	if policy.AutoHireEnabled || policy.AutoDismissEnabled {
		t.Fatalf("a project without a policy has automatic actions on: %+v", policy)
	}
	if policy.MinSuccessfulRuns != 3 || policy.MaxAutoHired != 5 || policy.GraceDays != 7 || len(policy.Rules) != 4 {
		t.Fatalf("defaults = %+v", policy)
	}
	if _, err := CheckPolicy(policy); err != nil {
		t.Fatalf("the defaults are refused: %v", err)
	}
}

func TestCheckPolicyNeverAllowsDemoteOrDeleteOrAnUnknownStage(t *testing.T) {
	for _, stage := range []Stage{StageDemote, StageDelete, "purge", ""} {
		policy := DefaultPolicy()
		policy.Rules = []Rule{{ID: "x", Conditions: []Condition{{MetricRuns30d, ">=", 1}}, Then: stage}}
		_, err := CheckPolicy(policy)
		if refusal, ok := AsRefusal(err); !ok || refusal.Code != PolicyInvalidCode || refusal.Status() != 422 {
			t.Fatalf("stage %q: %v", stage, err)
		}
	}
	for _, stage := range []Stage{StageWatch, StageSuspend, StageRetire} {
		if !AutomaticStage(stage) {
			t.Fatalf("%s is automatic", stage)
		}
	}
	for _, stage := range []Stage{StageDemote, StageDelete} {
		if AutomaticStage(stage) {
			t.Fatalf("%s is never automatic", stage)
		}
	}
}

func TestCheckPolicyBounds(t *testing.T) {
	cases := map[string]func(*Policy){
		"grace 0":         func(p *Policy) { p.GraceDays = 0 },
		"grace 91":        func(p *Policy) { p.GraceDays = 91 },
		"max 0":           func(p *Policy) { p.MaxAutoHired = 0 },
		"runs 0":          func(p *Policy) { p.MinSuccessfulRuns = 0 },
		"unknown metric":  func(p *Policy) { p.Rules[0].Conditions[0].Metric = "secret_rate" },
		"bad op":          func(p *Policy) { p.Rules[0].Conditions[0].Op = "!=" },
		"negative value":  func(p *Policy) { p.Rules[0].Conditions[0].Value = -1 },
		"no conditions":   func(p *Policy) { p.Rules[0].Conditions = nil },
		"duplicate id":    func(p *Policy) { p.Rules[1].ID = p.Rules[0].ID },
		"reserved id":     func(p *Policy) { p.Rules[0].ID = RuleOrphan },
		"bad id":          func(p *Policy) { p.Rules[0].ID = "Has Space" },
		"too many rules":  func(p *Policy) { p.Rules = make([]Rule, MaxRules+1) },
		"auto_hire id":    func(p *Policy) { p.Rules[0].ID = RuleAutoHire },
		"value too large": func(p *Policy) { p.Rules[0].Conditions[0].Value = 2e9 },
	}
	for name, mutate := range cases {
		policy := DefaultPolicy()
		mutate(&policy)
		if _, err := CheckPolicy(policy); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestRulesMatchAllConditionsAndTheStrongestStageWins(t *testing.T) {
	rules := DefaultRules()
	card := Scorecard{Runs30d: 25, Failed30d: 20, FailureRate30d: 0.8, BreakerTrips7d: 3}
	facts := FactsOf(card, 0, false, false)
	rule, ok := FirstMatch(rules, facts)
	if !ok || rule.ID != "breaker_trips" || rule.Then != StageSuspend {
		t.Fatalf("suspend beats watch: %+v %v", rule, ok)
	}
	facts = FactsOf(Scorecard{Runs30d: 10, Failed30d: 9, FailureRate30d: 0.9}, 0, false, false)
	if _, ok := FirstMatch(rules, facts); ok {
		t.Fatal("failure_rate needs 20 runs too")
	}
	facts = FactsOf(Scorecard{}, 90, false, false)
	if rule, ok := FirstMatch(rules, facts); !ok || rule.Then != StageRetire {
		t.Fatalf("unused 90 days: %+v %v", rule, ok)
	}
	failed := Scorecard{BreakerTrips7d: 1}
	if !ProbationFailed(failed) || ProbationFailed(Scorecard{Runs30d: 2, Failed30d: 2, FailureRate30d: 1}) {
		t.Fatal("probation failure reading")
	}
	if rule, ok := FirstMatch([]Rule{rules[3]}, FactsOf(failed, 0, true, true)); !ok || rule.ID != "probation_failed" {
		t.Fatalf("probation failed: %+v %v", rule, ok)
	}
	if _, ok := FirstMatch([]Rule{rules[3]}, FactsOf(failed, 0, false, true)); ok {
		t.Fatal("probation_failed needs auto_hired")
	}
	// A rule whose stage is not automatic never matches, even if stored.
	if _, ok := FirstMatch([]Rule{{ID: "x", Conditions: []Condition{{MetricRuns30d, ">=", 0}}, Then: StageDelete}}, facts); ok {
		t.Fatal("a delete rule matched")
	}
}

func TestSpendRatioReadsTheProjectMedian(t *testing.T) {
	facts := FactsOf(Scorecard{Spend30dUSD: 30, ProjectMedianSpendUSD: 10}, 0, false, false)
	if facts[MetricSpendRatio] != 3 {
		t.Fatalf("ratio = %v", facts[MetricSpendRatio])
	}
	if FactsOf(Scorecard{Spend30dUSD: 30}, 0, false, false)[MetricSpendRatio] != 0 {
		t.Fatal("no median is no ratio")
	}
}

func TestSuggestedTierAndCheckHire(t *testing.T) {
	if SuggestedTier(nil) != TierLow || SuggestedTier([]string{"read", "read"}) != TierLow ||
		SuggestedTier([]string{"read", "write"}) != TierMedium || SuggestedTier([]string{"read", "code_execution"}) != TierHigh {
		t.Fatal("suggested tier")
	}
	policy := DefaultPolicy()
	ok := HireEvidence{ToolCategories: []string{"read"}, Successful: 3}
	if CheckHire(policy, ok) != HirePolicyDisable {
		t.Fatal("off by default")
	}
	policy.AutoHireEnabled = true
	if reason := CheckHire(policy, ok); reason != "" {
		t.Fatalf("eligible: %s", reason)
	}
	cases := map[string]HireEvidence{
		HireNotLowTier:   {ToolCategories: []string{"read", "write"}, Successful: 3},
		HireFailedRun:    {ToolCategories: []string{"read"}, Successful: 5, Failed: 1},
		HireFewRuns:      {ToolCategories: []string{"read"}, Successful: 2},
		HireCapReached:   {ToolCategories: []string{"read"}, Successful: 3, OpenAutoHired: 5},
		HireUndoneRecent: {ToolCategories: []string{"read"}, Successful: 3, Undone: true},
	}
	for want, evidence := range cases {
		if got := CheckHire(policy, evidence); got != want {
			t.Errorf("%s: got %q", want, got)
		}
	}
}

func TestExemptions(t *testing.T) {
	if !ExemptFromAutomatic("critical", false) || !ExemptFromAutomatic("low", true) || ExemptFromAutomatic("high", false) {
		t.Fatal("exemption")
	}
}
