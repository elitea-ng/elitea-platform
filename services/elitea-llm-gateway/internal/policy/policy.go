// Package policy is the gateway's governance-DEFINITION plane: it reads the
// rows elitea-main's admin surface writes into `gateway.governance_config` and
// turns them into decisions on the /llm request path.
//
// # Why this is not internal/governance
//
// internal/governance is the budget COUNTER engine — NATS JetStream running
// totals, the tiered-hybrid fail-mode FSM, and the Postgres snapshot of
// `gateway.project_budget`. It answers "has this project spent past its
// ceiling?" and it has never read `gateway.governance_config`; the table's own
// migration header and the admin schema each claimed otherwise, and both were
// wrong (issue #218, corrected in #466).
//
// This package answers the other half: "what did an operator AUTHOR, and does
// it apply to this request?" The two share no state and no lifecycle. The only
// place they meet is BudgetDefaults, which hands the counter engine a fallback
// ceiling for a project that has no `gateway.project_budget` row.
//
// # The authoring contract
//
// Every row is `(type, section, name, data JSONB, enabled)`. `type` selects
// which definition the row carries; `data` holds the field paths the admin
// schema declares (`budget.limit_usd`, `scope.providers`, `routing.rules`, …).
// elitea-main validates on write and is the source of truth for what a valid
// row is (design-governance-config-authoring §3.1, §4). This package parses
// defensively anyway: a row that reaches the gateway malformed is REJECTED and
// named in Snapshot.Rejected rather than silently skipped, because a definition
// that vanishes without trace is the exact failure this whole feature exists to
// end.
//
// # Scope is a selector, with one deliberate exception
//
// `scope` says WHICH requests a definition applies to. An empty list on a
// dimension means "all"; a non-empty list means the request's value must be in
// it. `model_config` is the exception: its `scope.providers` / `scope.models`
// are the ALLOWLIST it declares, so those two dimensions select nothing and
// only `scope.project_ids` decides whether the row applies. Without that split
// the row would be circular — it would only permit what it already matched.
package policy

import (
	"fmt"
	"math"
	"sort"
	"strings"
	"time"
)

// Row types written by the admin surface. These strings are the contract with
// elitea-main's CRUD (services/elitea-main/internal/api/gateway/governance.go)
// and with the `type` column's documented value set in migration 0067.
const (
	TypeBudget           = "budget"
	TypeRateLimit        = "rate_limit"
	TypeModelConfig      = "model_config"
	TypeRoutingRule      = "routing_rule"
	TypeMCPAllowlist     = "mcp_allowlist"
	TypeCredentialPolicy = "credential_policy"

	// TypeEgressAllowlist is the operator's runtime egress policy: the hosts a
	// tenant-authored credential `api_base` may name (gap G6, shared migration
	// 0112).
	//
	// It exists because GATEWAY_EGRESS_ALLOWLIST was the ONLY way to state that
	// policy, so an on-premise model endpoint could not be reached without a
	// chart edit and a pod restart, and the environment variable answered two
	// different questions with one value. The rows here are UNIONED with the
	// environment floor; nothing authored can withdraw a host the chart named.
	TypeEgressAllowlist = "egress_allowlist"

	// TypeBudgetAlert is the platform soft-alert row written by
	// PUT /admin/gateway/budget-alerts (issue #322). This package does NOT
	// enforce it and must not treat it as unknown: it is read directly in SQL,
	// by the budget snapshot query in internal/failmode, on every /llm call.
	//
	// It is named here for exactly that reason. Without the name, the one row
	// that WAS already being read would be logged as an unrecognised
	// definition on every refresh — an operator chasing a real rejection would
	// find a permanent false one sitting next to it.
	TypeBudgetAlert = "budget_alert"
)

// Rate policies for a credential's usage (admin schema `credential.rate_policy`).
const (
	// RatePolicyBilled is normal cost accounting: the request's cost hits the
	// budget counter and the usage ledger.
	RatePolicyBilled = "billed"
	// RatePolicyZeroRateMetered records the usage but prices it at zero, so it
	// appears in the ledger and moves no counter.
	RatePolicyZeroRateMetered = "zero-rate-metered"
	// RatePolicyExcluded records nothing at all: no ledger row, no counter move.
	RatePolicyExcluded = "excluded"
)

// ValidRatePolicy reports whether s is one of the three authored policies.
func ValidRatePolicy(s string) bool {
	switch s {
	case RatePolicyBilled, RatePolicyZeroRateMetered, RatePolicyExcluded:
		return true
	}
	return false
}

// Subject is the request a definition is matched against. It carries only what
// the gateway can establish from the signed edge identity plus the resolved
// model — never anything the caller could assert for itself.
//
// There is no TeamIDs field, and that is not an omission. This platform has no
// teams: no `teams` table exists in any migration, and nothing populates a team
// on a request. A `scope.team_ids` that is non-empty therefore cannot be
// evaluated, so such a row is marked INERT rather than being silently treated
// as matching or as not matching. elitea-main rejects new ones on write.
type Subject struct {
	// ProjectID is the numeric Elitea project from X-Elitea-Project-Id. It is
	// negative when the request carries no resolvable project.
	ProjectID int
	// TenantID is the forwarded X-Elitea-Tenant-Id. It is the CEL
	// `customer_id`. Empty when the edge resolved no tenant.
	TenantID string
	// Provider and Model are the values AFTER model mapping — the provider the
	// request will actually dispatch to and the name that provider accepts.
	Provider string
	Model    string
}

// Scope is a definition's selector. Every dimension is "empty means all".
type Scope struct {
	Providers  []string
	Models     []string
	ProjectIDs []int
	TeamIDs    []int
}

// evaluable reports whether this scope can be decided at all. A scope naming
// teams cannot: see Subject.
func (s Scope) evaluable() bool { return len(s.TeamIDs) == 0 }

// specificity counts the constrained dimensions. It orders competing
// definitions of the same type: the narrowest wins, so a project-scoped budget
// beats a global one. Ties are broken by name so the choice is deterministic
// across replicas — two gateways must never enforce different rows.
func (s Scope) specificity() int {
	n := 0
	for _, constrained := range []bool{
		len(s.Providers) > 0,
		len(s.Models) > 0,
		len(s.ProjectIDs) > 0,
		len(s.TeamIDs) > 0,
	} {
		if constrained {
			n++
		}
	}
	return n
}

// selectsProject reports whether the scope's PRINCIPAL dimensions admit sub.
// This is the whole test for model_config, whose provider/model lists are its
// allowlist rather than its selector.
func (s Scope) selectsProject(sub Subject) bool {
	if !s.evaluable() {
		return false
	}
	if len(s.ProjectIDs) > 0 && !containsInt(s.ProjectIDs, sub.ProjectID) {
		return false
	}
	return true
}

// selects reports whether the scope admits sub on every dimension.
//
// A dimension the scope constrains but the subject cannot supply does NOT
// match. That direction is deliberate for the restrictive types (rate limits,
// MCP allowlists): a definition that cannot be evaluated must not be applied on
// a guess. For the permissive read — model_config — the caller uses
// selectsProject instead, so this asymmetry never widens an allowlist.
func (s Scope) selects(sub Subject) bool {
	if !s.selectsProject(sub) {
		return false
	}
	if len(s.Providers) > 0 && !containsFold(s.Providers, sub.Provider) {
		return false
	}
	if len(s.Models) > 0 && !containsFold(s.Models, sub.Model) {
		return false
	}
	return true
}

// BudgetDef is an authored budget ceiling (admin schema `budget.*`). Limits are
// authored in USD; the gateway scales them to nano-USD at the counter boundary
// and never before (design §5.1 — the three denominations are not
// interchangeable, and conflating them is the documented 1000x pricing bug).
type BudgetDef struct {
	Name        string
	Scope       Scope
	IsUnlimited bool
	// LimitUSD is nil when unlimited or when the operator saved a ceiling-less
	// row ("governed, no ceiling yet"). Nil and 0 are different states and are
	// kept apart, exactly as gateway.project_budget keeps them apart.
	LimitUSD     *float64
	Period       string
	SoftAlertPct int
	NATSFailMode string
}

// RateLimitDef is an authored per-minute ceiling (admin schema `rate_limit.*`).
// Zero on either field means that bucket is unlimited — the schema's "leave
// empty for no limit".
type RateLimitDef struct {
	Name           string
	Scope          Scope
	TokensPerMin   int64
	RequestsPerMin int64
}

// Limited reports whether this definition constrains anything at all. A row
// with both buckets empty is authored but inert, and callers skip the counter
// round-trip for it.
func (d RateLimitDef) Limited() bool { return d.TokensPerMin > 0 || d.RequestsPerMin > 0 }

// CredentialPolicyDef is an authored billing treatment (admin schema
// `credential.rate_policy`).
type CredentialPolicyDef struct {
	Name       string
	Scope      Scope
	RatePolicy string
}

// ModelConfigDef is an authored provider/model permission. Its Scope's
// Providers and Models are the ALLOWLIST, not the selector — see the package
// doc. Only ProjectIDs decides whether the row applies.
type ModelConfigDef struct {
	Name  string
	Scope Scope
}

// MCPAllowlistDef is an authored set of permitted MCP server ids (admin schema
// `mcp.allowlist`). An empty list disables the allowlist, matching the field's
// own description: "Empty disables the allowlist (all servers permitted)".
type MCPAllowlistDef struct {
	Name      string
	Scope     Scope
	Allowlist []string
}

// EgressAllowlistDef is an authored set of permitted egress destinations
// (admin schema `egress.allowlist`).
//
// It carries NO Scope, and that is a decision rather than an omission. The
// allowlist governs two things: which api_base a credential may name, and
// whether bifrost's SSRF-safe dialer is relaxed for the self-hosted provider
// classes. The second is decided in GetConfigForProvider, which bifrost calls
// with no context and no project — so a project scope could be honoured for
// half the definition and silently ignored for the other half. Compile REJECTS
// a scoped row rather than applying it more widely than it was authored.
type EgressAllowlistDef struct {
	Name string
	// Entries are the normalised host patterns. Parsing happens at compile, so
	// a malformed entry is named in Snapshot.Rejected rather than reaching the
	// credential path.
	Entries []string
}

// RoutingTarget is one weighted destination of a routing rule.
type RoutingTarget struct {
	Provider string
	Model    string
	Weight   float64
}

// RejectedRow names a row the gateway could not use, and why. It exists so a
// definition never disappears in silence: the reasons are logged at load and
// served on the gateway's own diagnostics surface, so an operator who authored
// something the gateway will not honour can find out without reading logs from
// a pod that has since been replaced.
type RejectedRow struct {
	ID     string `json:"id"`
	Type   string `json:"type"`
	Name   string `json:"name"`
	Reason string `json:"reason"`
}

// InertRow names a row that PARSED but can never match anything, with the
// condition that makes it inert. It is reported separately from RejectedRow
// because the row is not malformed — it is well-formed and unenforceable, which
// is a different conversation to have with the operator.
type InertRow struct {
	ID     string `json:"id"`
	Type   string `json:"type"`
	Name   string `json:"name"`
	Reason string `json:"reason"`
}

// Snapshot is an immutable, fully-parsed view of gateway.governance_config.
// Build it with Compile; publish it with Store.
type Snapshot struct {
	budgets      []BudgetDef
	rateLimits   []RateLimitDef
	credPolicies []CredentialPolicyDef
	modelConfigs []ModelConfigDef
	mcpLists     []MCPAllowlistDef
	egressLists  []EgressAllowlistDef
	routing      []RoutingRuleDef

	// LoadedAt is when the rows were read. A caller that must not act on a
	// stale definition set compares it against its own freshness bound.
	LoadedAt time.Time
	// RowCount is how many enabled rows the read returned, before parsing.
	RowCount int
	// Rejected and Inert are the operator-facing accounting of everything the
	// gateway will not enforce.
	Rejected []RejectedRow
	Inert    []InertRow
}

// Empty is the snapshot a gateway holds before its first successful read, and
// the one it holds when governance is not wired at all. Every accessor on it
// reports "no definition", which is enforcement-neutral: nothing is restricted
// and nothing is permitted that was not already.
var Empty = &Snapshot{}

// Budget returns the narrowest authored budget applying to sub.
func (s *Snapshot) Budget(sub Subject) (BudgetDef, bool) {
	if s == nil {
		return BudgetDef{}, false
	}
	best, found := BudgetDef{}, false
	bestSpec := -1
	for _, d := range s.budgets {
		if !d.Scope.selects(sub) {
			continue
		}
		if spec := d.Scope.specificity(); spec > bestSpec {
			best, bestSpec, found = d, spec, true
		}
	}
	return best, found
}

// DefaultBudget returns the narrowest authored budget applying to a project,
// for use as a FALLBACK when the project has no gateway.project_budget row.
//
// The subject carries the project only. A budget row scoped to a provider or a
// model therefore does not match here — the admission check that consults this
// has not resolved a provider, and applying a provider-scoped ceiling to a
// request whose provider is unknown would enforce a limit the operator did not
// author.
func (s *Snapshot) DefaultBudget(projectID int) (BudgetDef, bool) {
	def, ok := s.Budget(Subject{ProjectID: projectID})
	if !ok || def.IsUnlimited || def.LimitUSD == nil {
		return BudgetDef{}, false
	}
	return def, true
}

// LimitNanoUSD converts the authored USD ceiling to the nano-USD denomination
// the counter uses (design §5.1). It returns ok=false for an unlimited row, a
// negative value, or a value so large the conversion would overflow int64 —
// about 9.2 billion USD, which is not a ceiling anybody authored on purpose and
// which would wrap to a negative limit that admits everything.
func (d BudgetDef) LimitNanoUSD() (int64, bool) {
	if d.IsUnlimited || d.LimitUSD == nil {
		return 0, false
	}
	usd := *d.LimitUSD
	if usd < 0 || math.IsNaN(usd) || math.IsInf(usd, 0) {
		return 0, false
	}
	nano := usd * float64(nanoUSD)
	if nano > math.MaxInt64 {
		return 0, false
	}
	return int64(math.Round(nano)), true
}

// nanoUSD is the counter denomination. It mirrors failmode.NanoUSD; this
// package does not import failmode, so the value is restated with the reason
// rather than shared through a dependency that would only carry a constant.
const nanoUSD = 1_000_000_000

// RateLimit returns the narrowest authored rate limit applying to sub.
func (s *Snapshot) RateLimit(sub Subject) (RateLimitDef, bool) {
	if s == nil {
		return RateLimitDef{}, false
	}
	best, found := RateLimitDef{}, false
	bestSpec := -1
	for _, d := range s.rateLimits {
		if !d.Scope.selects(sub) {
			continue
		}
		if spec := d.Scope.specificity(); spec > bestSpec {
			best, bestSpec, found = d, spec, true
		}
	}
	return best, found
}

// CredentialPolicy returns the narrowest authored rate policy applying to sub,
// defaulting to RatePolicyBilled when nothing applies. Billed is the only safe
// default: the other two suppress accounting, and a definition that fails to
// load must never be the reason spend goes unrecorded.
func (s *Snapshot) CredentialPolicy(sub Subject) string {
	if s == nil {
		return RatePolicyBilled
	}
	best, bestSpec := RatePolicyBilled, -1
	for _, d := range s.credPolicies {
		if !d.Scope.selects(sub) {
			continue
		}
		if spec := d.Scope.specificity(); spec > bestSpec {
			best, bestSpec = d.RatePolicy, spec
		}
	}
	return best
}

// ModelDecision is the outcome of the provider/model allowlist check.
type ModelDecision struct {
	// Restricted is true when at least one model_config row selects this
	// project. When false the allowlist is not in force and Allowed is empty.
	Restricted bool
	// Allowed reports whether the subject's (provider, model) is permitted.
	// Meaningless when Restricted is false.
	Allowed bool
	// Rules names the rows that produced the decision, for the refusal log.
	Rules []string
}

// CheckModel applies the provider/model allowlist to sub.
//
// The union of every selecting row is the permitted set, not the intersection.
// Two rows that each grant a provider grant both, which is what an operator
// authoring one row per provider expects; an intersection would make the second
// row silently revoke the first.
//
// A row constraining NEITHER providers nor models grants everything to the
// projects it selects. That is not a no-op — it is how an operator exempts a
// project from a global allowlist authored in another row.
func (s *Snapshot) CheckModel(sub Subject) ModelDecision {
	if s == nil || len(s.modelConfigs) == 0 {
		return ModelDecision{}
	}
	dec := ModelDecision{}
	for _, d := range s.modelConfigs {
		if !d.Scope.selectsProject(sub) {
			continue
		}
		dec.Restricted = true
		dec.Rules = append(dec.Rules, d.Name)
		providerOK := len(d.Scope.Providers) == 0 || containsFold(d.Scope.Providers, sub.Provider)
		modelOK := len(d.Scope.Models) == 0 || containsFold(d.Scope.Models, sub.Model)
		if providerOK && modelOK {
			dec.Allowed = true
		}
	}
	return dec
}

// MCPDecision is the outcome of the MCP allowlist check.
type MCPDecision struct {
	// Restricted is true when an allowlist with at least one entry applies.
	Restricted bool
	// Denied names the requested servers the allowlist does not carry.
	Denied []string
	// Rule names the row that produced the decision.
	Rule string
	// Allowlist is the permitted set, for the refusal message.
	Allowlist []string
}

// CheckMCP applies the MCP server allowlist to the servers a request asks for.
//
// An empty allowlist permits everything, per the field's own description, so an
// operator who clears the chips turns the control OFF rather than locking every
// server out. Only the narrowest matching row applies: unlike model_config,
// unioning here would let a broad row re-permit what a narrow one was authored
// to forbid.
func (s *Snapshot) CheckMCP(sub Subject, requested []string) MCPDecision {
	if s == nil || len(requested) == 0 {
		return MCPDecision{}
	}
	var (
		best     MCPAllowlistDef
		bestSpec = -1
	)
	for _, d := range s.mcpLists {
		if !d.Scope.selects(sub) {
			continue
		}
		if spec := d.Scope.specificity(); spec > bestSpec {
			best, bestSpec = d, spec
		}
	}
	if bestSpec < 0 || len(best.Allowlist) == 0 {
		return MCPDecision{}
	}
	dec := MCPDecision{Restricted: true, Rule: best.Name, Allowlist: best.Allowlist}
	for _, want := range requested {
		if want == "" {
			continue
		}
		if !containsFold(best.Allowlist, want) {
			dec.Denied = append(dec.Denied, want)
		}
	}
	return dec
}

// EgressAllowlist returns the union of every authored egress entry, sorted and
// deduplicated.
//
// The union, not the narrowest row: every row is an operator statement that a
// destination is legitimate, and a second row must not silently revoke the
// first. This is the same rule model_config follows and the opposite of the one
// mcp_allowlist follows — the difference is that this list is merged with an
// environment floor rather than selected among competing scopes.
//
// It satisfies account.EgressSource. The account merges the result with
// GATEWAY_EGRESS_ALLOWLIST and re-parses; entries are already validated here,
// so that merge only ever widens.
func (s *Snapshot) EgressAllowlist() []string {
	if s == nil || len(s.egressLists) == 0 {
		return nil
	}
	seen := map[string]struct{}{}
	out := make([]string, 0, len(s.egressLists))
	for _, d := range s.egressLists {
		for _, e := range d.Entries {
			if _, dup := seen[e]; dup {
				continue
			}
			seen[e] = struct{}{}
			out = append(out, e)
		}
	}
	sort.Strings(out)
	return out
}

// Diagnostics is the operator-facing report of what this snapshot holds and
// what it refused. It backs the gateway's /governance/status surface so the
// admin page can show an operator that the rule they saved is actually loaded.
type Diagnostics struct {
	LoadedAt         time.Time     `json:"loaded_at"`
	Rows             int           `json:"rows"`
	Budgets          int           `json:"budgets"`
	RateLimits       int           `json:"rate_limits"`
	ModelConfigs     int           `json:"model_configs"`
	MCPAllowlists    int           `json:"mcp_allowlists"`
	CredentialPolicy int           `json:"credential_policies"`
	RoutingRules     int           `json:"routing_rules"`
	EgressAllowlists int           `json:"egress_allowlists"`
	Rejected         []RejectedRow `json:"rejected"`
	Inert            []InertRow    `json:"inert"`
}

// Diagnostics renders the snapshot's operator-facing report.
func (s *Snapshot) Diagnostics() Diagnostics {
	if s == nil {
		return Diagnostics{Rejected: []RejectedRow{}, Inert: []InertRow{}}
	}
	d := Diagnostics{
		LoadedAt:         s.LoadedAt,
		Rows:             s.RowCount,
		Budgets:          len(s.budgets),
		RateLimits:       len(s.rateLimits),
		ModelConfigs:     len(s.modelConfigs),
		MCPAllowlists:    len(s.mcpLists),
		CredentialPolicy: len(s.credPolicies),
		RoutingRules:     len(s.routing),
		EgressAllowlists: len(s.egressLists),
		Rejected:         s.Rejected,
		Inert:            s.Inert,
	}
	if d.Rejected == nil {
		d.Rejected = []RejectedRow{}
	}
	if d.Inert == nil {
		d.Inert = []InertRow{}
	}
	return d
}

// sortRouting orders rules by descending priority, then by name, so every
// replica evaluates them in the same order.
func sortRouting(rules []RoutingRuleDef) {
	sort.SliceStable(rules, func(i, j int) bool {
		if rules[i].Priority != rules[j].Priority {
			return rules[i].Priority > rules[j].Priority
		}
		return rules[i].Name < rules[j].Name
	})
}

func containsInt(haystack []int, needle int) bool {
	for _, v := range haystack {
		if v == needle {
			return true
		}
	}
	return false
}

// containsFold compares case-insensitively. Provider and model names arrive
// from three places that disagree about case — the admin form, the model
// catalogue row, and bifrost's own provider constants — and a governance rule
// that fails to match because an operator typed "OpenAI" is a silent
// enforcement hole, not a typo the operator can see.
func containsFold(haystack []string, needle string) bool {
	for _, v := range haystack {
		if strings.EqualFold(strings.TrimSpace(v), strings.TrimSpace(needle)) {
			return true
		}
	}
	return false
}

// inertTeamScope is the reason string for a row scoped to teams.
func inertTeamScope(ids []int) string {
	return fmt.Sprintf("scope.team_ids names %d team(s), but this platform has no teams: no teams table exists "+
		"and no request carries a team, so the row can never match. Remove the team scope to make it enforceable.", len(ids))
}
