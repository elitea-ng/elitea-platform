package policy

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/EliteaAI/elitea-platform/libs/go/egresslib"
)

// Row is one raw `gateway.governance_config` row as the gateway reads it.
type Row struct {
	ID      string
	Type    string
	Section string
	Name    string
	Data    map[string]any
	Enabled bool
}

// Compile turns raw rows into an immutable Snapshot.
//
// It never returns an error. A malformed row is recorded in Rejected and the
// rest of the corpus still loads: one bad routing rule must not disable every
// budget definition alongside it. The caller logs the rejections and serves
// them on the status surface, so "my rule is not being enforced" has an answer
// that does not require reading a dead pod's logs.
//
// now is injected so a test can assert LoadedAt without a clock.
func Compile(rows []Row, now time.Time) *Snapshot {
	snap := &Snapshot{LoadedAt: now, RowCount: len(rows)}

	for _, row := range rows {
		if !row.Enabled {
			continue
		}
		name := row.Name
		if name == "" {
			name = row.ID
		}
		data := row.Data
		if data == nil {
			data = map[string]any{}
		}

		reject := func(reason string) {
			snap.Rejected = append(snap.Rejected, RejectedRow{ID: row.ID, Type: row.Type, Name: name, Reason: reason})
		}
		inert := func(reason string) {
			snap.Inert = append(snap.Inert, InertRow{ID: row.ID, Type: row.Type, Name: name, Reason: reason})
		}

		scope := parseScope(data)
		// A team scope makes any row unenforceable, whatever its type. Report it
		// once here rather than letting each accessor silently fail to match.
		if !scope.evaluable() {
			inert(inertTeamScope(scope.TeamIDs))
			continue
		}

		switch row.Type {
		case TypeBudget:
			def, err := parseBudget(name, scope, data)
			if err != nil {
				reject(err.Error())
				continue
			}
			if def.IsUnlimited && def.LimitUSD == nil {
				// Not an error: "unlimited" is a real, deliberate authoring
				// choice. It carries no ceiling to enforce, so say so rather
				// than listing it as an active budget.
				inert("the budget is marked unlimited, so it imposes no ceiling")
			}
			snap.budgets = append(snap.budgets, def)

		case TypeRateLimit:
			def, err := parseRateLimit(name, scope, data)
			if err != nil {
				reject(err.Error())
				continue
			}
			if !def.Limited() {
				inert("neither tokens_per_min nor requests_per_min is set, so the row limits nothing")
			}
			snap.rateLimits = append(snap.rateLimits, def)

		case TypeCredentialPolicy:
			policy := stringField(subMap(data, "credential"), "rate_policy")
			if policy == "" {
				policy = stringField(data, "rate_policy")
			}
			if policy == "" {
				reject("credential.rate_policy is required")
				continue
			}
			if !ValidRatePolicy(policy) {
				reject(fmt.Sprintf("credential.rate_policy %q is not one of %s, %s, %s",
					policy, RatePolicyBilled, RatePolicyZeroRateMetered, RatePolicyExcluded))
				continue
			}
			snap.credPolicies = append(snap.credPolicies, CredentialPolicyDef{Name: name, Scope: scope, RatePolicy: policy})

		case TypeModelConfig:
			if len(scope.Providers) == 0 && len(scope.Models) == 0 {
				// A row permitting everything to the projects it selects is a
				// deliberate exemption, and it is also the shape an operator
				// produces by saving the section with nothing filled in. Name
				// it so the second case is visible.
				inert("scope.providers and scope.models are both empty, so this row permits every provider and " +
					"model to the projects it selects — it exempts them from any other model_config row rather " +
					"than restricting them")
			}
			snap.modelConfigs = append(snap.modelConfigs, ModelConfigDef{Name: name, Scope: scope})

		case TypeMCPAllowlist:
			list := stringSlice(subMap(data, "mcp"), "allowlist")
			if len(list) == 0 {
				list = stringSlice(data, "allowlist")
			}
			if len(list) == 0 {
				inert("mcp.allowlist is empty, which the field defines as 'all servers permitted' — the " +
					"allowlist is OFF for the scope this row selects")
			}
			snap.mcpLists = append(snap.mcpLists, MCPAllowlistDef{Name: name, Scope: scope, Allowlist: list})

		case TypeEgressAllowlist:
			// A scope on this row cannot be honoured for half of what the row
			// governs — see EgressAllowlistDef. Refuse it rather than apply it
			// more widely than it was authored.
			if scope.specificity() > 0 {
				reject("an egress_allowlist row is global and carries no scope: the private-network half of the " +
					"decision is made without a project, so a scoped row would apply to every project anyway. " +
					"Remove the scope")
				continue
			}
			raw := stringSlice(subMap(data, "egress"), "allowlist")
			if len(raw) == 0 {
				raw = stringSlice(data, "allowlist")
			}
			if len(raw) == 0 {
				// NOT the same meaning as an empty mcp.allowlist. An empty MCP
				// list turns that control off; an empty egress list would be
				// read as "no entry", and an operator who cleared the field
				// would think they had removed a restriction when they had
				// removed a permission. Say so.
				inert("egress.allowlist is empty, so this row permits no destination and widens nothing. " +
					"Delete the row, or list the hosts it should permit")
				snap.egressLists = append(snap.egressLists, EgressAllowlistDef{Name: name})
				continue
			}
			def, err := parseEgressAllowlist(name, raw)
			if err != nil {
				reject(err.Error())
				continue
			}
			snap.egressLists = append(snap.egressLists, def)

		case TypeRoutingRule:
			// A routing row carries either one rule inline, or the array the
			// admin form's `routing.rules` field produces. Both shapes reach
			// this table, so both are read.
			payloads := routingPayloads(data)
			if len(payloads) == 0 {
				reject("the row carries no routing rule: expected a `cel` + `targets` pair, or a `routing.rules` array")
				continue
			}
			for i, p := range payloads {
				ruleName := name
				if len(payloads) > 1 {
					ruleName = fmt.Sprintf("%s[%d]", name, i)
				}
				rule, err := parseRoutingRule(ruleName, p)
				if err != nil {
					reject(fmt.Sprintf("rule %s: %s", ruleName, err.Error()))
					continue
				}
				// The scope on the ROW applies to every rule it carries; a rule
				// may narrow it further with its own scope fields.
				if rule.Scope.specificity() == 0 {
					rule.Scope = scope
				}
				if !rule.Scope.evaluable() {
					inert(inertTeamScope(rule.Scope.TeamIDs))
					continue
				}
				if bad := ReferencedUnevaluable(rule.CEL); len(bad) > 0 {
					reject(fmt.Sprintf("rule %s references CEL variables this gateway cannot supply (%s); see the "+
						"authoring surface for why", ruleName, strings.Join(bad, ", ")))
					continue
				}
				snap.routing = append(snap.routing, rule)
			}

		case TypeBudgetAlert:
			// Enforced, but not here: internal/failmode reads this row inside
			// the budget snapshot SQL on every /llm call. Skipping it silently
			// is correct; calling it unknown would not be.
			continue

		default:
			reject(fmt.Sprintf("unknown governance type %q", row.Type))
		}
	}

	sortRouting(snap.routing)
	return snap
}

// routingPayloads normalises the two shapes a routing row can carry into a list
// of per-rule payloads.
func routingPayloads(data map[string]any) []map[string]any {
	// Shape 1: the row IS the rule.
	if _, ok := data["cel"]; ok {
		return []map[string]any{data}
	}
	// Shape 2: the admin form's `routing.rules` array.
	raw, ok := subMap(data, "routing")["rules"].([]any)
	if !ok {
		raw, _ = data["rules"].([]any)
	}
	out := make([]map[string]any, 0, len(raw))
	for _, r := range raw {
		if m, ok := r.(map[string]any); ok {
			out = append(out, m)
		}
	}
	return out
}

// parseEgressAllowlist validates every entry against the ONE grammar
// (libs/go/egresslib), the same parser that reads GATEWAY_EGRESS_ALLOWLIST and
// the same one elitea-main validates a write with.
//
// One bad entry rejects the WHOLE row rather than being dropped from it. An
// allowlist that silently lost a member is an allowlist that refuses a
// destination the operator believes they permitted, and the resulting symptom —
// one provider failing with EGRESS_HOST_NOT_ALLOWED while the row sits in the
// admin list looking correct — is exactly what this surface exists to prevent.
func parseEgressAllowlist(name string, raw []string) (EgressAllowlistDef, error) {
	list, err := egresslib.Parse(raw)
	if err != nil {
		return EgressAllowlistDef{}, fmt.Errorf("egress.allowlist: %w", err)
	}
	return EgressAllowlistDef{Name: name, Entries: list.Entries()}, nil
}

func parseBudget(name string, scope Scope, data map[string]any) (BudgetDef, error) {
	b := subMap(data, "budget")
	if len(b) == 0 {
		b = data
	}
	def := BudgetDef{
		Name:         name,
		Scope:        scope,
		IsUnlimited:  boolField(b, "is_unlimited"),
		Period:       stringField(b, "period"),
		SoftAlertPct: intField(b, "soft_alert_pct"),
		NATSFailMode: stringField(b, "nats_fail_mode"),
	}
	if v, ok := floatField(b, "limit_usd"); ok {
		if v < 0 {
			return BudgetDef{}, fmt.Errorf("budget.limit_usd must not be negative, got %g", v)
		}
		def.LimitUSD = &v
	}
	if def.Period == "" {
		def.Period = "monthly"
	}
	switch def.Period {
	case "daily", "weekly", "monthly", "yearly":
	default:
		return BudgetDef{}, fmt.Errorf("budget.period %q is not one of daily, weekly, monthly, yearly", def.Period)
	}
	if def.SoftAlertPct == 0 {
		def.SoftAlertPct = 80
	}
	if def.SoftAlertPct < 1 || def.SoftAlertPct > 100 {
		return BudgetDef{}, fmt.Errorf("budget.soft_alert_pct must be between 1 and 100, got %d", def.SoftAlertPct)
	}
	switch def.NATSFailMode {
	case "", "tiered_hybrid", "fail_open", "fail_closed":
	default:
		return BudgetDef{}, fmt.Errorf("budget.nats_fail_mode %q is not one of tiered_hybrid, fail_open, fail_closed", def.NATSFailMode)
	}
	if !def.IsUnlimited && def.LimitUSD == nil {
		// Mirrors gateway.project_budget's derivation: a row with no ceiling is
		// unlimited to the enforcement path whatever the flag says. Deriving it
		// here keeps the two definitions of "unlimited" identical.
		def.IsUnlimited = true
	}
	return def, nil
}

func parseRateLimit(name string, scope Scope, data map[string]any) (RateLimitDef, error) {
	rl := subMap(data, "rate_limit")
	if len(rl) == 0 {
		rl = data
	}
	def := RateLimitDef{Name: name, Scope: scope}
	if v, ok := floatField(rl, "tokens_per_min"); ok {
		if v < 0 {
			return RateLimitDef{}, fmt.Errorf("rate_limit.tokens_per_min must not be negative, got %g", v)
		}
		def.TokensPerMin = int64(v)
	}
	if v, ok := floatField(rl, "requests_per_min"); ok {
		if v < 0 {
			return RateLimitDef{}, fmt.Errorf("rate_limit.requests_per_min must not be negative, got %g", v)
		}
		def.RequestsPerMin = int64(v)
	}
	return def, nil
}

// parseScope reads the `scope` object. It also accepts the fields at the top
// level, because a hand-written row and an older authoring surface both produce
// that shape.
func parseScope(data map[string]any) Scope {
	s := subMap(data, "scope")
	if len(s) == 0 {
		s = data
	}
	return Scope{
		Providers:  stringSlice(s, "providers"),
		Models:     stringSlice(s, "models"),
		ProjectIDs: intSlice(s, "project_ids"),
		TeamIDs:    intSlice(s, "team_ids"),
	}
}

func subMap(data map[string]any, key string) map[string]any {
	if m, ok := data[key].(map[string]any); ok {
		return m
	}
	return map[string]any{}
}

func stringField(data map[string]any, key string) string {
	if v, ok := data[key].(string); ok {
		return v
	}
	return ""
}

func boolField(data map[string]any, key string) bool {
	if v, ok := data[key].(bool); ok {
		return v
	}
	return false
}

// floatField coerces the numeric shapes a JSONB round-trip can produce. pgx
// decodes JSONB numbers as float64, but json.Number appears when a decoder was
// configured with UseNumber, and a string is what an HTML number input can send
// through an authoring surface that does not coerce. Returning ok=false for an
// absent or unusable value is what keeps "no ceiling" distinct from "zero".
func floatField(data map[string]any, key string) (float64, bool) {
	switch n := data[key].(type) {
	case float64:
		return n, true
	case float32:
		return float64(n), true
	case int:
		return float64(n), true
	case int64:
		return float64(n), true
	case json.Number:
		f, err := n.Float64()
		return f, err == nil
	case string:
		f, err := strconv.ParseFloat(strings.TrimSpace(n), 64)
		return f, err == nil
	}
	return 0, false
}

func intField(data map[string]any, key string) int {
	if f, ok := floatField(data, key); ok {
		return int(f)
	}
	return 0
}

func stringSlice(data map[string]any, key string) []string {
	raw, ok := data[key].([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(raw))
	for _, v := range raw {
		if s, ok := v.(string); ok && strings.TrimSpace(s) != "" {
			out = append(out, strings.TrimSpace(s))
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func intSlice(data map[string]any, key string) []int {
	raw, ok := data[key].([]any)
	if !ok {
		return nil
	}
	out := make([]int, 0, len(raw))
	for _, v := range raw {
		switch n := v.(type) {
		case float64:
			out = append(out, int(n))
		case int:
			out = append(out, n)
		case json.Number:
			if i, err := n.Int64(); err == nil {
				out = append(out, int(i))
			}
		case string:
			if i, err := strconv.Atoi(strings.TrimSpace(n)); err == nil {
				out = append(out, i)
			}
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}
