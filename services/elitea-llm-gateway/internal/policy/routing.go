package policy

import (
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"sync"

	"github.com/google/cel-go/cel"
	"github.com/google/cel-go/common/types/ref"
)

// weightSumEpsilon bounds float error when re-verifying that a rule's target
// weights sum to 1.0. It MUST match elitea-main's constant of the same name
// (internal/api/gateway/governance.go): a rule the authoring surface accepted
// must not be rejected here for a rounding difference.
const weightSumEpsilon = 1e-6

// maxRoutingCELBytes caps a routing-rule predicate before it reaches the CEL
// parser. It MUST match elitea-main's constant of the same name
// (internal/api/gateway/routing_cel.go), which refuses a longer rule at
// authoring. This copy is the gateway's own bound: a row can reach
// gateway.governance_config from a restore or a hand-written UPDATE without
// passing elitea-main, and every replica compiles every rule on every policy
// load. Without it cel-go's own guard (100,000 code points) is the only bound.
const maxRoutingCELBytes = 8 << 10

// ErrRoutingCELTooLong reports a predicate longer than maxRoutingCELBytes. The
// error never carries the expression itself.
var ErrRoutingCELTooLong = errors.New("CEL expression is too long")

// CELVariables is the governance CEL variable set, declared identically here
// and in elitea-main (internal/api/gateway/routing_cel.go). The two compilers
// MUST agree: a rule that type-checks on the authoring side has to type-check
// on the enforcement side, or an operator sees "valid" and the gateway silently
// drops the rule.
//
// Declaring a variable is NOT the same as being able to evaluate it. Four of
// the nine name data this gateway does not have at routing time, and rules that
// reference them are refused AT AUTHORING by elitea-main (see
// UnevaluableCELVariables) rather than being accepted and quietly never
// matching. They stay declared so the two environments remain identical.
func celEnv() (*cel.Env, error) {
	return cel.NewEnv(
		cel.Variable("budget_used", cel.DoubleType),
		cel.Variable("tokens_used", cel.IntType),
		cel.Variable("provider", cel.StringType),
		cel.Variable("model", cel.StringType),
		cel.Variable("team_id", cel.StringType),
		cel.Variable("customer_id", cel.StringType),
		cel.Variable("complexity_tier", cel.StringType),
		cel.Variable("headers", cel.MapType(cel.StringType, cel.StringType)),
		cel.Variable("params", cel.MapType(cel.StringType, cel.DynType)),
	)
}

// UnevaluableCELVariables maps each declared-but-unevaluable variable to the
// reason the gateway cannot supply it. elitea-main refuses to SAVE a rule
// referencing any of them and quotes the reason back to the operator.
//
// This map is the single source of that truth, exported so the authoring side
// cannot drift from the enforcement side. Moving a variable out of here means
// the gateway now supplies it; nothing else has to change.
var UnevaluableCELVariables = map[string]string{
	"team_id": "this platform has no teams: no teams table exists in any migration and no request carries a " +
		"team, so team_id would always be the empty string",
	"tokens_used": "a routing decision is made before dispatch, when no tokens have been consumed; the gateway " +
		"has no token count to supply and tokens_used would always be 0",
	"complexity_tier": "nothing in this platform classifies a request into a complexity tier, so complexity_tier " +
		"would always be the empty string",
	"headers": "request headers are not exposed to routing rules: the edge strips authentication material but " +
		"the remaining headers are caller-controlled, and routing on them would let a caller pick its own provider",
}

// RoutingRuleDef is one compiled routing rule. The cel.Program is built once at
// snapshot compile time — compiling on the request path would put a parser in
// front of every /llm call.
type RoutingRuleDef struct {
	Name     string
	Scope    Scope
	CEL      string
	Priority int
	Targets  []RoutingTarget

	program cel.Program
}

// routingEnv is built once per process. Env construction parses the standard
// macro and declaration set, which is not free, and every snapshot reload would
// otherwise pay for it again.
var routingEnv struct {
	once sync.Once
	env  *cel.Env
	err  error
}

func governanceCELEnv() (*cel.Env, error) {
	routingEnv.once.Do(func() { routingEnv.env, routingEnv.err = celEnv() })
	return routingEnv.env, routingEnv.err
}

// CompileCEL type-checks expr against the governance variable set and requires
// a boolean result, mirroring elitea-main's CompileRoutingCEL. An expression
// longer than maxRoutingCELBytes is refused before the parser sees it.
func CompileCEL(expr string) (cel.Program, error) {
	if strings.TrimSpace(expr) == "" {
		return nil, errors.New("CEL expression must not be empty")
	}
	if len(expr) > maxRoutingCELBytes {
		return nil, fmt.Errorf("%w: %d bytes, limit %d", ErrRoutingCELTooLong, len(expr), maxRoutingCELBytes)
	}
	env, err := governanceCELEnv()
	if err != nil {
		return nil, fmt.Errorf("CEL environment unavailable: %w", err)
	}
	ast, issues := env.Compile(expr)
	if issues != nil && issues.Err() != nil {
		return nil, fmt.Errorf("CEL compile error: %s", issues.Err())
	}
	if out := ast.OutputType(); !out.IsExactType(cel.BoolType) {
		return nil, fmt.Errorf("CEL expression must evaluate to bool, got %s", out.TypeName())
	}
	prg, err := env.Program(ast)
	if err != nil {
		return nil, fmt.Errorf("CEL program build failed: %w", err)
	}
	return prg, nil
}

// RoutingInputs are the values a rule is evaluated against. BudgetUsed is a
// function rather than a value because reading it costs a NATS round-trip: a
// deployment whose rules never mention budget_used must not pay for it on every
// request. It is called at most once per evaluation and memoised.
type RoutingInputs struct {
	Provider   string
	Model      string
	CustomerID string
	// Params are the request parameters exposed as CEL `params`. Nil is a valid
	// empty map; a rule indexing a key that is absent errors, and an erroring
	// rule is SKIPPED (never treated as a match).
	Params map[string]any
	// BudgetUsed returns the fraction of the project's budget consumed in the
	// current period, in [0, 1+). nil resolves budget_used to 0.
	BudgetUsed func() float64
}

// routingActivation resolves CEL variables lazily. cel.Program.Eval accepts any
// Activation, so the expensive input (budget_used) is fetched only when an
// expression actually names it.
type routingActivation struct {
	in RoutingInputs

	once   sync.Once
	budget float64
}

func (a *routingActivation) Parent() cel.Activation { return nil }

func (a *routingActivation) ResolveName(name string) (any, bool) {
	switch name {
	case "provider":
		return a.in.Provider, true
	case "model":
		return a.in.Model, true
	case "customer_id":
		return a.in.CustomerID, true
	case "params":
		if a.in.Params == nil {
			return map[string]any{}, true
		}
		return a.in.Params, true
	case "budget_used":
		a.once.Do(func() {
			if a.in.BudgetUsed != nil {
				a.budget = a.in.BudgetUsed()
			}
		})
		return a.budget, true
	// The four unevaluable variables resolve to their zero values. A rule that
	// names one cannot be SAVED (UnevaluableCELVariables), so reaching these
	// arms means a row predates that validation; the zero value keeps
	// evaluation total instead of erroring on a legacy row.
	case "team_id", "complexity_tier":
		return "", true
	case "tokens_used":
		return int64(0), true
	case "headers":
		return map[string]string{}, true
	}
	return nil, false
}

// RoutingDecision is the outcome of evaluating the rule set.
type RoutingDecision struct {
	// Matched is true when a rule fired and chose a target.
	Matched bool
	// Rule is the name of the rule that fired.
	Rule string
	// Target is the provider/model the request should dispatch to.
	Target RoutingTarget
}

// Route evaluates the rule set against sub and in, returning the target chosen
// by the highest-priority matching rule.
//
// pick selects a target from the cumulative weight distribution: it receives
// the total weight and returns a value in [0, total). Production passes a
// random source; a test passes a fixed one so a weighted rule is assertable.
// Passing nil selects the first target, which is the only stable choice
// available without a source of randomness.
//
// A rule whose expression ERRORS is skipped and reported through onError, never
// treated as a match. An erroring predicate says nothing about whether the rule
// should apply, and defaulting it to "apply" would let a typo in one rule
// silently re-route a whole deployment.
func (s *Snapshot) Route(sub Subject, in RoutingInputs, pick func(total float64) float64, onError func(rule string, err error)) RoutingDecision {
	if s == nil || len(s.routing) == 0 {
		return RoutingDecision{}
	}
	act := &routingActivation{in: in}
	for _, rule := range s.routing {
		if !rule.Scope.selects(sub) || rule.program == nil {
			continue
		}
		out, _, err := rule.program.Eval(act)
		if err != nil {
			if onError != nil {
				onError(rule.Name, err)
			}
			continue
		}
		if !truthy(out) {
			continue
		}
		target, ok := chooseTarget(rule.Targets, pick)
		if !ok {
			if onError != nil {
				onError(rule.Name, errors.New("rule matched but carries no usable target"))
			}
			continue
		}
		return RoutingDecision{Matched: true, Rule: rule.Name, Target: target}
	}
	return RoutingDecision{}
}

// truthy reports whether a CEL result is boolean true. Anything else — an
// error value, a non-bool, a nil — is false. Compilation already required a
// bool output type, so this is a belt-and-braces read of the runtime value.
func truthy(v ref.Val) bool {
	if v == nil {
		return false
	}
	b, ok := v.Value().(bool)
	return ok && b
}

// chooseTarget picks one target from the weighted list.
//
// Weights are validated to sum to 1.0 on both the authoring and the loading
// side, but this walks the CUMULATIVE weight against the actual total rather
// than assuming 1.0. A rule whose weights drifted still distributes across all
// of its targets instead of concentrating on the first — and a float sum that
// lands a hair under the draw returns the last target rather than nothing.
func chooseTarget(targets []RoutingTarget, pick func(total float64) float64) (RoutingTarget, bool) {
	usable := make([]RoutingTarget, 0, len(targets))
	total := 0.0
	for _, t := range targets {
		if t.Model == "" || t.Weight <= 0 {
			continue
		}
		usable = append(usable, t)
		total += t.Weight
	}
	if len(usable) == 0 {
		return RoutingTarget{}, false
	}
	if pick == nil || total <= 0 {
		return usable[0], true
	}
	draw := pick(total)
	if draw < 0 || math.IsNaN(draw) {
		draw = 0
	}
	cum := 0.0
	for _, t := range usable {
		cum += t.Weight
		if draw < cum {
			return t, true
		}
	}
	return usable[len(usable)-1], true
}

// parseRoutingRule builds a compiled rule from a row's data payload. It applies
// the SAME two checks elitea-main applies on write — the CEL compiles to a
// bool, and the target weights sum to 1.0 — because a row can reach this table
// from a database restore, a manual UPDATE, or a version of the authoring
// surface older than these rules.
func parseRoutingRule(name string, data map[string]any) (RoutingRuleDef, error) {
	rule := RoutingRuleDef{
		Name:     name,
		Scope:    parseScope(data),
		CEL:      stringField(data, "cel"),
		Priority: intField(data, "priority"),
	}
	prg, err := CompileCEL(rule.CEL)
	if err != nil {
		return RoutingRuleDef{}, err
	}
	rule.program = prg

	rawTargets, _ := data["targets"].([]any)
	if len(rawTargets) == 0 {
		return RoutingRuleDef{}, errors.New("routing rule requires at least one weighted target")
	}
	sum := 0.0
	for _, rt := range rawTargets {
		t, ok := rt.(map[string]any)
		if !ok {
			return RoutingRuleDef{}, errors.New("each routing target must be an object")
		}
		provider := strings.TrimSpace(stringField(t, "provider"))
		model := strings.TrimSpace(stringField(t, "model"))
		if provider == "" {
			return RoutingRuleDef{}, errors.New("each routing target requires a provider")
		}
		if model == "" {
			return RoutingRuleDef{}, errors.New("each routing target requires a model")
		}
		weight, ok := floatField(t, "weight")
		if !ok {
			return RoutingRuleDef{}, errors.New("each routing target requires a numeric weight")
		}
		if weight < 0 {
			return RoutingRuleDef{}, errors.New("routing target weights must be non-negative")
		}
		sum += weight
		rule.Targets = append(rule.Targets, RoutingTarget{Provider: provider, Model: model, Weight: weight})
	}
	if math.Abs(sum-1.0) > weightSumEpsilon {
		return RoutingRuleDef{}, fmt.Errorf("routing target weights must sum to 1.0, got %g", sum)
	}
	// Order targets deterministically so the same draw picks the same target on
	// every replica. Without this the cumulative walk depends on JSONB key
	// order, and two gateways would split traffic differently for one rule.
	sort.SliceStable(rule.Targets, func(i, j int) bool {
		if rule.Targets[i].Provider != rule.Targets[j].Provider {
			return rule.Targets[i].Provider < rule.Targets[j].Provider
		}
		return rule.Targets[i].Model < rule.Targets[j].Model
	})
	return rule, nil
}

// ReferencedUnevaluable returns the unevaluable variables an expression names,
// in a stable order. It is a lexical check over the compiled source rather than
// an AST walk: the variable set is fixed and small, and the check must produce
// the same answer in elitea-main, which builds no program.
func ReferencedUnevaluable(expr string) []string {
	var found []string
	for _, name := range sortedKeys(UnevaluableCELVariables) {
		if referencesIdent(expr, name) {
			found = append(found, name)
		}
	}
	return found
}

// referencesIdent reports whether expr uses name as an identifier — a match
// that is not part of a longer identifier and is not a field selector
// (`params.headers` names a map key, not the `headers` variable).
func referencesIdent(expr, name string) bool {
	for i := 0; i+len(name) <= len(expr); i++ {
		if expr[i:i+len(name)] != name {
			continue
		}
		if i > 0 {
			prev := expr[i-1]
			if isIdentByte(prev) || prev == '.' {
				continue
			}
		}
		if i+len(name) < len(expr) && isIdentByte(expr[i+len(name)]) {
			continue
		}
		return true
	}
	return false
}

func isIdentByte(b byte) bool {
	return b == '_' || (b >= '0' && b <= '9') || (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z')
}

func sortedKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
