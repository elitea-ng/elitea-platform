package llmproxy

// policy_gate.go — enforcement of the governance DEFINITIONS authored through
// the admin surface and stored in gateway.governance_config (issue #218).
//
// This is the half of governance that did not exist. internal/governance
// enforces SPEND against a counter; this enforces what an operator AUTHORED:
// which providers and models a project may reach, how many requests and tokens
// a minute it may spend, which MCP servers it may call out to, how its usage is
// billed, and which provider a request is routed to.
//
// # Where each control runs, and why there
//
//	model allowlist   mapModel, after resolution — the one chokepoint every
//	                  dialect passes through with a resolved (provider, model).
//	routing rules     mapModel, immediately BEFORE the allowlist, so a rule can
//	                  never route a request around the allowlist.
//	rate limits       admissionVerdictFor, alongside the budget gate — so the
//	                  realtime route's periodic re-check gets them too.
//	MCP allowlist     the three dialects whose wire format can carry an MCP
//	                  server; it needs the raw body, which only they hold.
//	rate policy       the billing path (updateUsage), which is the only place a
//	                  cost is turned into a counter movement.
//
// # Nil is off, everywhere
//
// Every entry point returns "permit" when the policy source is nil. A Handler
// built without governance behaves exactly as it did before this file existed,
// which is what keeps the existing unit-test construction valid.

import (
	"context"
	"fmt"
	"math/rand/v2"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/maximhq/bifrost/core/schemas"

	"github.com/elitea-ng/elitea-platform/services/elitea-llm-gateway/internal/account"
	"github.com/elitea-ng/elitea-platform/services/elitea-llm-gateway/internal/policy"
)

// PolicySource supplies the current compiled governance definitions.
// *policy.Store satisfies it; tests inject a fixed snapshot.
type PolicySource interface {
	Current() *policy.Snapshot
}

// BudgetUsageReader reports the fraction of a project's budget consumed in the
// current period, for the CEL `budget_used` variable.
//
// It is a separate port from BudgetChecker because it is consulted at a
// different point in the request (before model dispatch, not at admission) and
// because it is OPTIONAL: a deployment whose routing rules never mention
// budget_used never calls it. *governance.GovernanceStore satisfies it.
type BudgetUsageReader interface {
	// BudgetFraction returns the consumed fraction and whether a ceiling exists
	// to divide by. It returns ok=false for an unlimited or absent budget,
	// which the CEL layer renders as 0.
	BudgetFraction(ctx context.Context, projectID int, scope, scopeID string, periodStartUnix int64) (float64, bool)
}

// WithGovernancePolicy arms enforcement of the authored governance definitions.
//
// src is required; limiter and usage are optional. A nil limiter disables rate
// limiting only (the posture of a gateway with no NATS); a nil usage reader
// resolves budget_used to 0, which a rule referencing it will see.
func WithGovernancePolicy(src PolicySource, limiter *policy.Limiter, usage BudgetUsageReader) HandlerOption {
	return func(h *Handler) {
		h.policy = src
		h.rateLimiter = limiter
		// usage rides the budget plane: it is the one collaborator here that
		// comes from the NATS budget path, so a late install has to publish it
		// with the gate (budget_plane.go). The assignment is unconditional —
		// the option runs at construction, where a nil usage means the same
		// "no reader" it always did.
		h.mutateBudget(func(bp *budgetPlane) { bp.usage = usage })
	}
}

// WithRoutingPick overrides the weighted-target draw. Production leaves it
// unset and gets a random source; a test sets it so a weighted rule has a
// determinate outcome.
func WithRoutingPick(pick func(total float64) float64) HandlerOption {
	return func(h *Handler) { h.routePick = pick }
}

// policySubject builds the matching subject for a request. provider and model
// are the POST-mapping values — a definition names what the request will
// actually dispatch to, never the label the caller happened to send.
func (h *Handler) policySubject(ctx context.Context, provider schemas.ModelProvider, model string) policy.Subject {
	return policy.Subject{
		ProjectID: parseProjectID(identityProjectFromCtx(ctx)),
		TenantID:  bifrostCtxString(ctx, schemas.BifrostContextKeyGovernanceCustomerID),
		Provider:  string(provider),
		Model:     model,
	}
}

// applyPolicy runs the routing rules and then the provider/model allowlist. It
// is called from mapModel with the resolved provider and model, and may rewrite
// both. It returns false when it has already written a refusal.
func (h *Handler) applyPolicy(
	w http.ResponseWriter,
	ctx *schemas.BifrostContext,
	provider *schemas.ModelProvider,
	model *string,
) bool {
	if h.policy == nil {
		return true
	}
	snap := h.policy.Current()
	if snap == nil {
		return true
	}
	h.applyRouting(ctx, snap, provider, model)
	return h.checkModelAllowlist(w, ctx, snap, *provider, *model)
}

// applyRouting evaluates the CEL routing rules and rewrites the request's
// provider and model when one fires.
//
// A rewrite CLEARS the pinned credential. The pin names the credential of the
// model row the caller asked for; after a rewrite the request is going to a
// different provider, and keeping the pin would make the account look for that
// credential under the new provider and fail every such request. Clearing it
// puts the project's whole credential set for the new provider back on offer,
// which is what an unpinned request has always done.
func (h *Handler) applyRouting(
	ctx *schemas.BifrostContext,
	snap *policy.Snapshot,
	provider *schemas.ModelProvider,
	model *string,
) {
	sub := h.policySubject(ctx, *provider, *model)
	in := policy.RoutingInputs{
		Provider:   sub.Provider,
		Model:      sub.Model,
		CustomerID: sub.TenantID,
		BudgetUsed: h.budgetFractionFunc(ctx, sub.ProjectID),
	}
	dec := snap.Route(sub, in, h.routingPick(), func(rule string, err error) {
		h.logger.WarnContext(ctx, "governance: routing rule failed to evaluate and was SKIPPED",
			"rule", rule, "project_id", sub.ProjectID, "model", sub.Model, "err", err)
	})
	if !dec.Matched {
		return
	}
	target := dec.Target
	if !schemas.IsKnownProvider(target.Provider) {
		h.logger.ErrorContext(ctx, "governance: routing rule names a provider this gateway cannot dispatch to; "+
			"the request keeps its original provider",
			"rule", dec.Rule, "target_provider", target.Provider, "target_model", target.Model)
		return
	}
	if string(*provider) == target.Provider && *model == target.Model {
		return
	}
	h.logger.InfoContext(ctx, "governance: routing rule rewrote the request target",
		"rule", dec.Rule,
		"project_id", sub.ProjectID,
		"from_provider", string(*provider), "from_model", *model,
		"to_provider", target.Provider, "to_model", target.Model)
	*provider = schemas.ModelProvider(target.Provider)
	*model = target.Model
	ctx.SetValue(account.ContextKeyLinkedCredential, account.LinkedCredential{})
	publishRequestModel(ctx, *model)
}

// routingPick returns the weighted-target draw. The default is a random source:
// weights only mean anything if the draw is random, and a fixed draw would send
// 100% of traffic to whichever target sorts first.
func (h *Handler) routingPick() func(total float64) float64 {
	if h.routePick != nil {
		return h.routePick
	}
	return func(total float64) float64 { return rand.Float64() * total }
}

// budgetFractionFunc returns the lazily-evaluated budget_used supplier. It is
// nil-safe in three directions — no reader, no project, no ceiling — and each
// resolves to 0 rather than to an error, because a routing rule must not fail
// a request.
func (h *Handler) budgetFractionFunc(ctx context.Context, projectID int) func() float64 {
	usage := h.budget().usage
	if usage == nil || projectID < 0 {
		return nil
	}
	return func() float64 {
		readCtx, cancel := context.WithTimeout(ctx, budgetGateTimeout)
		defer cancel()
		frac, ok := usage.BudgetFraction(
			readCtx, projectID, budgetScopeProject, strconv.Itoa(projectID), billingPeriodStart(time.Now()))
		if !ok {
			return 0
		}
		return frac
	}
}

// checkModelAllowlist refuses a request whose (provider, model) no authored
// model_config row permits.
//
// The refusal is 403, not 404. A 404 would say the model does not exist, which
// is untrue and sends the caller looking for a typo; 403 says the model exists
// and this project may not use it, which is the actual state and the one an
// operator can act on.
func (h *Handler) checkModelAllowlist(
	w http.ResponseWriter,
	ctx context.Context,
	snap *policy.Snapshot,
	provider schemas.ModelProvider,
	model string,
) bool {
	sub := h.policySubject(ctx, provider, model)
	dec := snap.CheckModel(sub)
	if !dec.Restricted || dec.Allowed {
		return true
	}
	h.logger.WarnContext(ctx, "governance: model is not permitted for this project by the authored allowlist",
		"project_id", sub.ProjectID, "provider", sub.Provider, "model", sub.Model,
		"rules", strings.Join(dec.Rules, ","))
	writeError(w, http.StatusForbidden, "permission_error",
		fmt.Sprintf("model %q on provider %q is not permitted for this project by the LLM governance policy",
			model, string(provider)),
		"model_not_permitted")
	return false
}

// checkMCPAllowlist refuses a request naming an MCP server the authored
// allowlist does not carry. raw is the request's undecoded body.
func (h *Handler) checkMCPAllowlist(
	w http.ResponseWriter,
	ctx context.Context,
	raw []byte,
	provider string,
	model string,
) bool {
	if h.policy == nil {
		return true
	}
	requested := policy.MCPServersFromRequest(raw)
	if len(requested) == 0 {
		return true
	}
	snap := h.policy.Current()
	if snap == nil {
		return true
	}
	sub := h.policySubject(ctx, schemas.ModelProvider(provider), model)
	dec := snap.CheckMCP(sub, requested)
	if !dec.Restricted || len(dec.Denied) == 0 {
		return true
	}
	h.logger.WarnContext(ctx, "governance: request names MCP servers the authored allowlist does not permit",
		"project_id", sub.ProjectID, "rule", dec.Rule,
		"denied", strings.Join(dec.Denied, ","),
		"allowed", strings.Join(dec.Allowlist, ","))
	writeError(w, http.StatusForbidden, "permission_error",
		fmt.Sprintf("MCP server(s) %s are not permitted for this project by the LLM governance policy",
			strings.Join(dec.Denied, ", ")),
		"mcp_server_not_permitted")
	return false
}

// rateVerdict applies the authored per-minute rate limits. It returns a
// budgetVerdict so the HTTP path and the realtime path refuse on identical
// terms, exactly as the budget and loop-breaker refusals already do.
func (h *Handler) rateVerdict(ctx context.Context, model string, mode admissionMode) budgetVerdict {
	if h.policy == nil || !h.rateLimiter.Enabled() {
		return budgetAllowed
	}
	snap := h.policy.Current()
	if snap == nil {
		return budgetAllowed
	}
	// The provider is not known at admission on every route, so the subject
	// carries the model only. A rate-limit row scoped to a provider therefore
	// does not match here — see the note on Scope.selects: a constraint the
	// subject cannot answer does not apply, rather than applying on a guess.
	sub := h.policySubject(ctx, "", model)
	def, ok := snap.RateLimit(sub)
	if !ok || !def.Limited() {
		return budgetAllowed
	}
	dec := h.rateLimiter.Admit(ctx, def, sub, mode == admissionArrival)
	if dec.Allowed {
		return budgetAllowed
	}
	h.logger.WarnContext(ctx, "governance: authored rate limit refused the request",
		"rule", dec.Rule, "bucket", dec.Bucket,
		"limit", dec.Limit, "observed", dec.Observed, "project_id", sub.ProjectID)
	return budgetVerdict{
		status:  http.StatusTooManyRequests,
		errType: "rate_limit_error",
		message: fmt.Sprintf("the LLM governance policy limits this project to %d %s per minute; retry after the window resets",
			dec.Limit, dec.Bucket),
		code:       "rate_limit_exceeded",
		retryAfter: dec.RetryAfter,
	}
}

// recordPolicyTokens adds a completed request's tokens to its rate-limit
// window. It is called from the billing path, which is the only place the
// authoritative token count is known.
func (h *Handler) recordPolicyTokens(ctx context.Context, provider, model string, tokens int64, admittedAt time.Time) {
	if h.policy == nil || !h.rateLimiter.Enabled() || tokens <= 0 {
		return
	}
	snap := h.policy.Current()
	if snap == nil {
		return
	}
	sub := h.policySubject(ctx, schemas.ModelProvider(provider), model)
	def, ok := snap.RateLimit(sub)
	if !ok || def.TokensPerMin <= 0 {
		return
	}
	h.rateLimiter.RecordTokens(ctx, def, sub, admittedAt, tokens)
}

// ratePolicyFor returns the authored billing treatment for a request. Callers
// use it to decide whether a cost reaches the counter at all.
func (h *Handler) ratePolicyFor(ctx context.Context, provider, model string) string {
	if h.policy == nil {
		return policy.RatePolicyBilled
	}
	snap := h.policy.Current()
	if snap == nil {
		return policy.RatePolicyBilled
	}
	return snap.CredentialPolicy(h.policySubject(ctx, schemas.ModelProvider(provider), model))
}
