package gateway

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/google/cel-go/cel"
)

// The governance CEL variable set — design-governance-config-authoring §3.1.
// A routing-rule expression is a boolean predicate over exactly these variables.
// elitea-main does NOT import bifrost/core (it is the thin auth edge), so this
// declares the same variable surface locally; the gateway's GovernanceStore
// compiles the identical set for enforcement. Keep the two in lock-step: a rule
// that type-checks here MUST type-check in the gateway.
//
//	budget_used     number  — fraction/amount of budget consumed
//	tokens_used     int     — tokens consumed on the request path
//	provider        string  — upstream provider id
//	model           string  — model name
//	team_id         string  — requesting team
//	customer_id     string  — requesting customer
//	complexity_tier string  — routing complexity bucket
//	headers         map[string]string — request headers
//	params          map[string]dyn    — request parameters
func newRoutingCELEnv() (*cel.Env, error) {
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

// routingCELEnv is built once — env construction parses the standard macro/decl
// set and is not free, while a routing-rule save is on a human-interactive path.
var routingCELEnv struct {
	once sync.Once
	env  *cel.Env
	err  error
}

func governanceCELEnv() (*cel.Env, error) {
	routingCELEnv.once.Do(func() {
		routingCELEnv.env, routingCELEnv.err = newRoutingCELEnv()
	})
	return routingCELEnv.env, routingCELEnv.err
}

// unevaluableCELVariables names the variables the gateway DECLARES but cannot
// supply a real value for, with the reason. A rule that references one is
// refused on write.
//
// This is the alternative to the quieter option, which is to accept the rule
// and let it evaluate against a zero value for ever. That option produces a
// rule an operator can see in the list, that reports itself valid, and that can
// never match — the exact shape of the defect this whole issue exists to close.
//
// The map MUST agree with policy.UnevaluableCELVariables in the gateway module.
// The two are separate declarations because the gateway is a separate Go module
// that elitea-main does not import; TestUnevaluableCELVariablesMatchTheGateway
// reads the gateway source and fails when they drift.
var unevaluableCELVariables = map[string]string{
	"team_id": "this platform has no teams: no teams table exists in any migration and no request carries a " +
		"team, so team_id would always be the empty string",
	"tokens_used": "a routing decision is made before dispatch, when no tokens have been consumed; the gateway " +
		"has no token count to supply and tokens_used would always be 0",
	"complexity_tier": "nothing in this platform classifies a request into a complexity tier, so complexity_tier " +
		"would always be the empty string",
	"headers": "request headers are not exposed to routing rules: the edge strips authentication material but " +
		"the remaining headers are caller-controlled, and routing on them would let a caller pick its own provider",
}

// UnevaluableCELVariableNames returns the refused variable names in a stable
// order. It backs both the validation error and the admin page's editor hints,
// so the operator is told the same thing before and after they press save.
func UnevaluableCELVariableNames() []string {
	out := make([]string, 0, len(unevaluableCELVariables))
	for name := range unevaluableCELVariables {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// UnevaluableCELVariableReason returns the reason a variable is refused.
func UnevaluableCELVariableReason(name string) string { return unevaluableCELVariables[name] }

// referencedUnevaluable returns the refused variables an expression names.
//
// It is a lexical check, not an AST walk, and it MUST stay identical to
// policy.ReferencedUnevaluable in the gateway. A match that is part of a longer
// identifier does not count, and neither does a field selector: `params.headers`
// names a map key, not the `headers` variable.
func referencedUnevaluable(expr string) []string {
	var found []string
	for _, name := range UnevaluableCELVariableNames() {
		if referencesIdent(expr, name) {
			found = append(found, name)
		}
	}
	return found
}

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

// maxRoutingCELBytes caps a routing-rule predicate before it reaches the CEL
// parser. A rule is one boolean over nine variables; 8 KiB is far above any
// hand-written predicate. cel-go's own guard (100,000 code points, recursion
// depth 250) would otherwise be the only bound, and every byte accepted here is
// compiled again by the gateway on every policy load.
const maxRoutingCELBytes = 8 << 10

// ErrRoutingCELTooLong reports a predicate longer than maxRoutingCELBytes.
var ErrRoutingCELTooLong = errors.New("CEL expression is too long")

// CompileRoutingCEL type-checks a routing-rule CEL expression against the
// governance variable set and requires it to evaluate to a boolean. It returns a
// user-facing error describing the first compile/type problem, or nil when the
// expression is a valid boolean predicate. This is the server-side source of
// truth — the admin editor's inline hints are UX only (design §3.1, §4).
//
// It also refuses an expression that references a variable the gateway cannot
// supply. That check is NOT part of the CEL type system — such an expression
// type-checks perfectly — so it runs after the compile and reports the reason
// the gateway gives, not a generic rejection.
func CompileRoutingCEL(expr string) error {
	if expr == "" {
		return fmt.Errorf("CEL expression must not be empty")
	}
	if len(expr) > maxRoutingCELBytes {
		return fmt.Errorf("%w: %d bytes, limit %d", ErrRoutingCELTooLong, len(expr), maxRoutingCELBytes)
	}
	env, err := governanceCELEnv()
	if err != nil {
		return fmt.Errorf("CEL environment unavailable: %w", err)
	}
	ast, issues := env.Compile(expr)
	if issues != nil && issues.Err() != nil {
		return fmt.Errorf("CEL compile error: %s", issues.Err())
	}
	if out := ast.OutputType(); !out.IsExactType(cel.BoolType) {
		return fmt.Errorf("CEL expression must evaluate to bool, got %s", out.TypeName())
	}
	if bad := referencedUnevaluable(expr); len(bad) > 0 {
		return fmt.Errorf("CEL expression references %s, which the gateway cannot evaluate: %s",
			strings.Join(bad, ", "), UnevaluableCELVariableReason(bad[0]))
	}
	return nil
}
