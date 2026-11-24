package gateway

// gates.go — the route gates of the /admin/gateway sub-surfaces (ADR-0059
// R0.3).
//
// Until R0.3 one `r.Use(central("configuration.governance"))` in router.go
// gated the whole group, and the handlers here applied none. The group gate is
// gone: every Routes()/Register here takes the gates its routes need, so a
// route cannot be mounted without one. router.go builds each gate so that it
// accepts the narrow permission of ADR-0059 decision 2 OR
// configuration.governance, and a holder of the legacy name sees no change
// (internal/api/testdata/admin_gateway_legacy.golden).

import (
	"fmt"
	"net/http"
	"slices"
)

// Gate admits the caller to the route it wraps or answers the refusal.
type Gate = func(http.Handler) http.Handler

// mustGate refuses to mount a route with no gate: a nil gate would otherwise
// make the route open to every authenticated caller, which is how a mount
// that forgets its gate fails.
func mustGate(surface, name string, gate Gate) Gate {
	if gate == nil {
		panic(fmt.Sprintf("gateway: %s mounted without its %s gate", surface, name))
	}
	return gate
}

// LLMProxyGates are the gates of the LLM Proxy surface.
type LLMProxyGates struct {
	// Usage gates the gateway status, the usage rollup and the request logs
	// (llm.usage.view).
	Usage Gate
	// CatalogueView gates the price catalogue reads (llm.catalogue.view).
	CatalogueView Gate
	// CatalogueManage gates the price writes (llm.catalogue.manage).
	CatalogueManage Gate
}

// GovernanceGates are the gates of the governance rule surface.
type GovernanceGates struct {
	// View gates the list and the CEL validation, which changes nothing
	// (llm.policy.view).
	View Gate
	// Manage gates every write. It admits a holder of llm.policy.manage OR
	// llm.guardrails.manage (or the legacy name); the handler then decides by
	// the kind of the row, so a policy manager cannot write a guardrail
	// because the route is the same (governanceWriter).
	Manage Gate
	// Holds answers whether the caller holds any of names, from the
	// resolution the Manage gate made for this request.
	Holds func(r *http.Request, names ...string) bool
}

// policyRuleTypes are the governance row types llm.policy.manage writes: the
// routing and throttle rules of ADR-0059 decision 2 (`budget` and
// `model_config` are the cost side of the same rules).
var policyRuleTypes = []string{"routing_rule", "rate_limit", "budget", "model_config"}

// governanceWriter is what one caller may write on the governance surface,
// read from the permissions the route gate resolved.
//
//   - A guardrail is a safety control (ADR-0041, owner decision 13), so it
//     takes llm.guardrails.manage, never llm.policy.manage.
//   - The routing and throttle rules take llm.policy.manage.
//   - Every other type — the egress and MCP allowlists, the credential policy,
//     the budget alert row the budget-alerts route owns, and any type not
//     known here — has no narrow name yet and keeps the legacy name only.
//
// configuration.governance writes every type, as before.
//
// A personal access token the token rule refuses the guardrail outcome
// (ADR-0059 R1.2b, issue 2993) writes no stored guardrail, whatever it holds:
// tokenNoGuardrail.
type governanceWriter struct {
	legacy, guardrails, policy bool
	tokenNoGuardrail           bool
}

// may answers whether the caller may write a row of rowType.
func (g governanceWriter) may(rowType string) bool {
	switch {
	case g.legacy:
		return true
	case rowType == guardrailType:
		return g.guardrails
	case slices.Contains(policyRuleTypes, rowType):
		return g.policy
	}
	return false
}

// storedClause is the SQL form of may over the stored row's `type` column,
// for the WHERE of an update or a delete, with its arguments numbered from
// first. The constants are bound, never spliced. The token rule's guardrail
// refusal is part of the same clause, so a row cannot become a guardrail
// between the read that classified the write and the write.
func (g governanceWriter) storedClause(first int) (string, []any) {
	clause := fmt.Sprintf("($%d OR (type = $%d AND $%d) OR (type = ANY($%d::text[]) AND $%d)) AND NOT ($%d AND type = $%d)",
		first, first+1, first+2, first+3, first+4, first+5, first+1)
	return clause, []any{g.legacy, guardrailType, g.guardrails, policyRuleTypes, g.policy, g.tokenNoGuardrail}
}
