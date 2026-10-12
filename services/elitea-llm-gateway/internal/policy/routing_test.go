package policy

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"
)

func routingRow(name, cel string, priority float64, targets ...map[string]any) Row {
	anyTargets := make([]any, 0, len(targets))
	for _, t := range targets {
		anyTargets = append(anyTargets, t)
	}
	return row("r-"+name, TypeRoutingRule, name, map[string]any{
		"cel": cel, "priority": priority, "targets": anyTargets,
	})
}

func target(provider, model string, weight float64) map[string]any {
	return map[string]any{"provider": provider, "model": model, "weight": weight}
}

func TestRouteMatchesAndRewrites(t *testing.T) {
	t.Parallel()

	snap := Compile([]Row{
		routingRow("to-claude", `provider == "openai" && model == "gpt-4o"`, 10,
			target("anthropic", "claude-sonnet", 1.0)),
	}, testNow)

	sub := Subject{ProjectID: 1, Provider: "openai", Model: "gpt-4o"}
	dec := snap.Route(sub, RoutingInputs{Provider: "openai", Model: "gpt-4o"}, nil, nil)
	if !dec.Matched {
		t.Fatalf("rule did not match; rejections: %+v", snap.Rejected)
	}
	if dec.Target.Provider != "anthropic" || dec.Target.Model != "claude-sonnet" {
		t.Errorf("target = %+v", dec.Target)
	}

	// A request the predicate does not select is untouched.
	other := snap.Route(sub, RoutingInputs{Provider: "cohere", Model: "command"}, nil, nil)
	if other.Matched {
		t.Error("the rule matched a request its predicate excludes")
	}
}

// TestHighestPriorityRuleWins pins the ordering, and pins that it is stable:
// two replicas must pick the same rule for the same request.
func TestHighestPriorityRuleWins(t *testing.T) {
	t.Parallel()

	snap := Compile([]Row{
		routingRow("low", `true`, 1, target("openai", "low", 1.0)),
		routingRow("high", `true`, 99, target("openai", "high", 1.0)),
	}, testNow)

	dec := snap.Route(Subject{ProjectID: 1}, RoutingInputs{}, nil, nil)
	if dec.Rule != "high" || dec.Target.Model != "high" {
		t.Errorf("rule = %q target = %q, want the priority-99 rule", dec.Rule, dec.Target.Model)
	}
}

// TestWeightedTargetsFollowTheDraw is why the draw is injectable: without a
// fixed source a weighted rule has no assertable outcome.
func TestWeightedTargetsFollowTheDraw(t *testing.T) {
	t.Parallel()

	snap := Compile([]Row{
		routingRow("split", `true`, 1,
			target("anthropic", "claude", 0.3),
			target("openai", "gpt-4o", 0.7)),
	}, testNow)

	// Targets are sorted by (provider, model), so anthropic/claude occupies
	// [0, 0.3) and openai/gpt-4o occupies [0.3, 1.0).
	cases := []struct {
		draw      float64
		wantModel string
	}{
		{0.0, "claude"},
		{0.29, "claude"},
		{0.3, "gpt-4o"},
		{0.99, "gpt-4o"},
	}
	for _, tc := range cases {
		dec := snap.Route(Subject{ProjectID: 1}, RoutingInputs{},
			func(total float64) float64 { return tc.draw * total }, nil)
		if dec.Target.Model != tc.wantModel {
			t.Errorf("draw %v selected %q, want %q", tc.draw, dec.Target.Model, tc.wantModel)
		}
	}
}

// TestWeightsMustSumToOne mirrors elitea-main's write-side check. A row can
// reach this table from a restore or a manual UPDATE, so the load side repeats
// it rather than trusting the authoring surface.
func TestWeightsMustSumToOne(t *testing.T) {
	t.Parallel()

	snap := Compile([]Row{
		routingRow("bad", `true`, 1,
			target("openai", "a", 0.5),
			target("openai", "b", 0.2)),
	}, testNow)

	if len(snap.Rejected) != 1 || !strings.Contains(snap.Rejected[0].Reason, "sum to 1.0") {
		t.Fatalf("want a weight-sum rejection, got %+v", snap.Rejected)
	}
}

// TestErroringRuleIsSkippedNotMatched is the safety direction that matters
// most: a typo in one rule must not re-route a deployment.
func TestErroringRuleIsSkippedNotMatched(t *testing.T) {
	t.Parallel()

	// Indexing an absent key is a CEL runtime error, not `false`.
	snap := Compile([]Row{
		routingRow("indexes-missing-key", `params.absent == "x"`, 10, target("openai", "a", 1.0)),
		routingRow("fallback", `true`, 1, target("openai", "b", 1.0)),
	}, testNow)
	if len(snap.Rejected) != 0 {
		t.Fatalf("both rules should compile: %+v", snap.Rejected)
	}

	var gotRule string
	var gotErr error
	dec := snap.Route(Subject{ProjectID: 1}, RoutingInputs{}, nil, func(rule string, err error) {
		gotRule, gotErr = rule, err
	})
	if gotRule != "indexes-missing-key" || gotErr == nil {
		t.Errorf("the erroring rule was not reported: rule=%q err=%v", gotRule, gotErr)
	}
	if !dec.Matched || dec.Rule != "fallback" {
		t.Errorf("evaluation did not continue past the erroring rule: %+v", dec)
	}
}

// TestBudgetUsedIsResolvedLazilyAndOnce pins the cost control on the one
// expensive CEL input.
func TestBudgetUsedIsResolvedLazilyAndOnce(t *testing.T) {
	t.Parallel()

	calls := 0
	in := RoutingInputs{BudgetUsed: func() float64 { calls++; return 0.95 }}

	// A rule set that never names budget_used must not pay for it.
	noRef := Compile([]Row{routingRow("plain", `true`, 1, target("openai", "a", 1.0))}, testNow)
	noRef.Route(Subject{ProjectID: 1}, in, nil, nil)
	if calls != 0 {
		t.Errorf("budget_used was read %d times by a rule set that does not reference it", calls)
	}

	// A rule that names it twice reads it once.
	twice := Compile([]Row{
		routingRow("hot", `budget_used > 0.9 && budget_used < 2.0`, 1, target("anthropic", "cheap", 1.0)),
	}, testNow)
	dec := twice.Route(Subject{ProjectID: 1}, in, nil, nil)
	if !dec.Matched {
		t.Fatalf("the budget_used rule did not match at 0.95: %+v", twice.Rejected)
	}
	if calls != 1 {
		t.Errorf("budget_used was read %d times in one evaluation, want 1", calls)
	}
}

// TestRuleReferencingUnevaluableVariableIsRejected is the load-side half of the
// authoring refusal. A row that predates the validation must not load and
// silently never match.
func TestRuleReferencingUnevaluableVariableIsRejected(t *testing.T) {
	t.Parallel()

	snap := Compile([]Row{
		routingRow("legacy", `tokens_used > 10`, 1, target("openai", "a", 1.0)),
	}, testNow)

	if len(snap.Rejected) != 1 {
		t.Fatalf("want the legacy rule rejected, got %+v", snap.Rejected)
	}
	if !strings.Contains(snap.Rejected[0].Reason, "tokens_used") {
		t.Errorf("the rejection does not name the variable: %q", snap.Rejected[0].Reason)
	}
}

func TestReferencedUnevaluable(t *testing.T) {
	t.Parallel()

	cases := []struct {
		expr string
		want []string
	}{
		{`tokens_used > 1`, []string{"tokens_used"}},
		{`headers["a"] == complexity_tier`, []string{"complexity_tier", "headers"}},
		// A map key that spells a refused variable is not a reference to it.
		{`params.headers == "x"`, nil},
		// An identifier that merely starts with one is not a reference either.
		{`provider == "team_idle"`, nil},
		{`provider == "openai"`, nil},
	}
	for _, tc := range cases {
		if got := ReferencedUnevaluable(tc.expr); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("ReferencedUnevaluable(%q) = %v, want %v", tc.expr, got, tc.want)
		}
	}
}

// TestRoutingRulesArrayShapeLoads covers the payload the admin form's
// `routing.rules` field produces, alongside the one-rule-per-row shape.
func TestRoutingRulesArrayShapeLoads(t *testing.T) {
	t.Parallel()

	snap := Compile([]Row{
		row("1", TypeRoutingRule, "set", map[string]any{
			"routing": map[string]any{"rules": []any{
				map[string]any{"cel": `provider == "openai"`, "priority": 5.0,
					"targets": []any{target("anthropic", "claude", 1.0)}},
				map[string]any{"cel": `provider == "cohere"`, "priority": 6.0,
					"targets": []any{target("openai", "gpt-4o", 1.0)}},
			}},
		}),
	}, testNow)

	if len(snap.Rejected) != 0 {
		t.Fatalf("rejections: %+v", snap.Rejected)
	}
	if got := snap.Diagnostics().RoutingRules; got != 2 {
		t.Errorf("loaded %d rules, want 2", got)
	}
	dec := snap.Route(Subject{ProjectID: 1, Provider: "openai"}, RoutingInputs{Provider: "openai"}, nil, nil)
	if !dec.Matched || dec.Target.Model != "claude" {
		t.Errorf("array-shaped rule did not route: %+v", dec)
	}
}

func TestChooseTargetHandlesDegenerateWeights(t *testing.T) {
	t.Parallel()

	// Every target unusable.
	if _, ok := chooseTarget([]RoutingTarget{{Provider: "openai", Weight: 1}}, nil); ok {
		t.Error("a target with no model was treated as usable")
	}
	// A draw at or beyond the total returns the last target rather than none.
	got, ok := chooseTarget([]RoutingTarget{
		{Provider: "a", Model: "one", Weight: 0.5},
		{Provider: "b", Model: "two", Weight: 0.5},
	}, func(total float64) float64 { return total })
	if !ok || got.Model != "two" {
		t.Errorf("draw at the total returned %+v, ok=%v", got, ok)
	}
}

func TestCompileCELRejectsNonBool(t *testing.T) {
	t.Parallel()

	if _, err := CompileCEL(`provider`); err == nil {
		t.Error("a string-valued expression compiled as a predicate")
	}
	if _, err := CompileCEL(``); err == nil {
		t.Error("an empty expression compiled")
	}
	if _, err := CompileCEL(`unknown_var == 1`); err == nil {
		t.Error("an expression over an undeclared variable compiled")
	}
	if _, err := CompileCEL(`provider == "openai"`); err != nil {
		t.Errorf("a valid predicate failed to compile: %v", err)
	}
}

// celOfLength returns a valid boolean routing predicate exactly n bytes long.
func celOfLength(t *testing.T, n int) string {
	t.Helper()
	const prefix, suffix = `provider == "`, `"`
	pad := n - len(prefix) - len(suffix)
	if pad < 0 {
		t.Fatalf("cannot build a %d-byte predicate", n)
	}
	return prefix + strings.Repeat("a", pad) + suffix
}

func TestCompileCELLengthBound(t *testing.T) {
	t.Parallel()

	atLimit := celOfLength(t, maxRoutingCELBytes)
	if len(atLimit) != maxRoutingCELBytes {
		t.Fatalf("fixture is %d bytes, want %d", len(atLimit), maxRoutingCELBytes)
	}
	if _, err := CompileCEL(atLimit); err != nil {
		t.Fatalf("an expression at the limit was refused: %v", err)
	}
	overLimit := celOfLength(t, maxRoutingCELBytes+1)
	_, err := CompileCEL(overLimit)
	if !errors.Is(err, ErrRoutingCELTooLong) {
		t.Fatalf("err = %v, want ErrRoutingCELTooLong", err)
	}
	if strings.Contains(err.Error(), overLimit) {
		t.Error("the refusal echoes the expression back")
	}
}

// TestHandWrittenOverlongRuleIsRejectedAtLoad is the load-side half of the
// authoring cap: a row that never passed elitea-main (a restore, a manual
// UPDATE) is still bounded here. The rule at the limit loads beside it.
func TestHandWrittenOverlongRuleIsRejectedAtLoad(t *testing.T) {
	t.Parallel()

	snap := Compile([]Row{
		routingRow("at-limit", celOfLength(t, maxRoutingCELBytes), 1, target("openai", "a", 1.0)),
		routingRow("over-limit", celOfLength(t, maxRoutingCELBytes+1), 2, target("openai", "b", 1.0)),
	}, testNow)

	if len(snap.routing) != 1 || snap.routing[0].Name != "at-limit" {
		t.Fatalf("loaded rules = %+v, want only at-limit; rejections: %+v", snap.routing, snap.Rejected)
	}
	if len(snap.Rejected) != 1 {
		t.Fatalf("want the over-limit rule rejected, got %+v", snap.Rejected)
	}
	reason := snap.Rejected[0].Reason
	if !strings.Contains(reason, "over-limit") || !strings.Contains(reason, ErrRoutingCELTooLong.Error()) {
		t.Errorf("the rejection does not name the rule and the cap: %q", reason)
	}
	if strings.Contains(reason, strings.Repeat("a", 64)) {
		t.Error("the rejection echoes the expression back")
	}
}

// TestRoutingCELCapMatchesMain keeps the two copies of the cap equal. A rule
// elitea-main accepts at authoring must not be rejected here at load, and the
// gateway must not admit more than elitea-main does. The modules cannot share
// the constant (elitea-main's is internal), so the test compares the sources.
func TestRoutingCELCapMatchesMain(t *testing.T) {
	t.Parallel()

	decl := regexp.MustCompile(`(?m)^const maxRoutingCELBytes = (.+)$`)
	read := func(path string) string {
		t.Helper()
		src, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		m := decl.FindSubmatch(src)
		if m == nil {
			t.Fatalf("%s declares no maxRoutingCELBytes", path)
		}
		return strings.TrimSpace(string(m[1]))
	}
	gateway := read("routing.go")
	main := read(filepath.Join("..", "..", "..", "elitea-main", "internal", "api", "gateway", "routing_cel.go"))
	if gateway != main {
		t.Fatalf("maxRoutingCELBytes is %q here and %q in elitea-main; they must match", gateway, main)
	}
}
