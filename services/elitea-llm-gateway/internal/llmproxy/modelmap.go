// modelmap.go — the single point where the model id a CALLER sends becomes the
// model name the PROVIDER receives (issue #317).
//
// GET /llm/v1/models advertises each configured model under its elitea_title, a
// user-authored label. The provider only knows the row's data.name. The two are
// independent by construction, so a caller that picks a model out of the
// advertised list used to send the provider a name it does not recognise. This
// file maps the one onto the other, on every dialect, before dispatch.
//
// The legacy LiteLLM proxy had the same two-name model: it registered a
// deployment with an addressable `model_name` and a separate provider
// `litellm_params.model`, and translated the first into the second on the way
// out. Nothing replaced that translation when LiteLLM was removed.

package llmproxy

import (
	"context"
	"expvar"
	"net/http"

	"github.com/maximhq/bifrost/core/schemas"

	"github.com/EliteaAI/elitea-platform/services/elitea-llm-gateway/internal/requestlog"

	"github.com/EliteaAI/elitea-platform/services/elitea-llm-gateway/internal/account"
)

// modelLookup is the outcome of mapping a caller's model id for one project.
type modelLookup int

const (
	// modelSetNoProject means the request carries no project identity. There is
	// no model set to check the caller's model against, and there are no
	// credentials either: account.GetKeysForProvider returns zero keys for an
	// empty project. The request is refused with 404 model_not_found.
	//
	// This is a CONDITION OF THE REQUEST, not a fault of the gateway (issue
	// #469). It is kept apart from the two fault conditions below for that
	// reason: a database fault must not be reported as a caller error, and a
	// caller error must not make an operator look for a broken database.
	modelSetNoProject modelLookup = iota
	// modelSetNoDatabase means the resolver holds no database handle, so it can
	// never read any project's model set. The request is refused with 502.
	//
	// This is a COMPOSITION FAULT, not a deployment posture. A gateway that
	// starts without a database gets NO resolver at all (see main.go), and
	// mapModel then maps nothing and forwards every model unchanged. A resolver
	// that exists with no database is a wiring error, and forwarding on it
	// would make the degraded path the permanent path.
	modelSetNoDatabase
	// modelSetLookupFailed means the query failed and nothing is cached. The
	// request is refused with 502.
	//
	// A query failure with a cache entry does NOT reach here: List serves the
	// last good list and reports the set as known (see models.go). That stale
	// list is the bounded permissive path, and it is the only one. Reaching
	// this outcome means the gateway has never read this project's model set,
	// so there is no bound to apply.
	modelSetLookupFailed
	// modelNotAdvertised means the model set was read and holds no such model.
	modelNotAdvertised
	// modelResolved means the id maps to a provider model name.
	modelResolved
)

// The model map's refusal counters (issue #469). Each counter names ONE of the
// three conditions in which the gateway cannot read a project's model set.
//
// A refusal that only writes a log line is hard to alarm on. These counters
// give an operator a number to alarm on, so a gateway that refuses every
// request cannot stay unreported. The gateway serves them over HTTP on
// /metrics; cmd/elitea-llm-gateway collects them through ModelMapMetricNames.
const (
	// MetricModelMapRefusedNoProject counts requests refused because the
	// request carried no project identity.
	MetricModelMapRefusedNoProject = "gateway_model_map_refused_no_project_total"
	// MetricModelMapRefusedNoDatabase counts requests refused because the model
	// resolver holds no database handle.
	MetricModelMapRefusedNoDatabase = "gateway_model_map_refused_no_database_total"
	// MetricModelMapRefusedLookupFailed counts requests refused because the
	// model query failed and no cached list exists.
	MetricModelMapRefusedLookupFailed = "gateway_model_map_refused_lookup_failed_total"
)

var (
	modelMapRefusedNoProject    = expvar.NewInt(MetricModelMapRefusedNoProject)
	modelMapRefusedNoDatabase   = expvar.NewInt(MetricModelMapRefusedNoDatabase)
	modelMapRefusedLookupFailed = expvar.NewInt(MetricModelMapRefusedLookupFailed)
)

// ModelMapMetricNames returns the names of the counters above, in a fixed
// order. The composition root reads this list to build the /metrics allowlist,
// so a counter that this package publishes reaches the scrape surface through
// ONE named path instead of a name copied into a second file.
func ModelMapMetricNames() []string {
	return []string{
		MetricModelMapRefusedNoProject,
		MetricModelMapRefusedNoDatabase,
		MetricModelMapRefusedLookupFailed,
	}
}

// resolve maps a caller-visible model id onto the provider model name to
// dispatch for projectID. ids are the candidate spellings of one request model,
// most specific first (see requestModelCandidates); the first candidate that
// names an advertised model wins.
//
// A candidate matches on the advertised id (elitea_title) first, and on the
// row's own provider model name (data.name) second. Both name the SAME row, so
// accepting the second keeps a caller that already sends wire names working
// while the advertised list stays callable. It never widens the set: a name
// that no configured row carries still resolves to modelNotAdvertised.
// It returns the whole resolved row, not the wire name alone: the row also
// carries the provider its linked credential serves, which for a bare model
// name is the only provider there is (issue #451).
//
// The three conditions in which the model set cannot be read are separated
// here, and each gets its own outcome (issue #469). The two guards below run in
// the order List applies them, so the outcome always names the condition List
// acted on. TestModelMap_ResolveClassifiesEachUnknownCondition holds the two in
// step.
func (m *ModelResolver) resolve(ctx context.Context, projectID string, ids []string) (modelObject, modelLookup) {
	if projectID == "" {
		return modelObject{}, modelSetNoProject
	}
	if m.db == nil {
		return modelObject{}, modelSetNoDatabase
	}
	models, known := m.list(ctx, projectID)
	if !known {
		return modelObject{}, modelSetLookupFailed
	}
	for _, id := range ids {
		if id == "" {
			continue
		}
		for _, mo := range models {
			if mo.ID == id {
				return mo, modelResolved
			}
		}
		for _, mo := range models {
			if mo.providerModel == id {
				return mo, modelResolved
			}
		}
	}
	return modelObject{}, modelNotAdvertised
}

// requestModelCandidates lists the spellings that can name an advertised model,
// most specific first.
//
// bifrost splits a "provider/model" request string before a handler sees it
// (schemas.ParseModelString), so a request for "openai/gpt-4o" arrives as the
// pair ("openai", "gpt-4o"). An advertised id can itself carry that prefix —
// preflight's StaticLegacyModels advertises "openai/gpt-4o" — so the rejoined
// form must be tried before the bare one, or such an id would 404 against the
// very list that advertises it.
func requestModelCandidates(provider schemas.ModelProvider, model string) []string {
	if provider == "" || model == "" {
		return []string{model}
	}
	return []string{string(provider) + "/" + model, model}
}

// mapModel rewrites provider and model in place, from what the caller sent to
// what the provider accepts. It reports whether the request may continue; when
// it returns false it has already written the response.
//
// EVERY /llm dialect that carries a model MUST call it after it decodes the
// request and BEFORE the budget gate. Ordering matters twice: the provider must
// never see an unmapped title, and the cost tables are keyed by the provider's
// own model name, so a budget check or a usage record made before the mapping
// would price the wrong model.
//
// The outcomes are:
//   - resolved — provider and model become the mapped pair.
//   - not advertised — 404, so an unknown model fails at the gateway with an
//     OpenAI-shaped error instead of at the provider with an opaque one.
//   - no project — 404. The caller sent no project identity, so the caller has
//     access to no model and to no credential. The 404 message says exactly
//     that.
//   - no database, or lookup failed — 502. The gateway cannot read the model
//     set, so it cannot prove the caller's model is one the project configured.
//
// Issue #469 changed those three conditions from "forward the caller's model
// unchanged" to a refusal. The reasons, in order:
//
//  1. An unmapped model name goes to the provider exactly as the caller wrote
//     it. The caller, not the project configuration, then chooses what the
//     provider receives. The budget gate also prices a model name that no
//     configuration row carries.
//  2. The "a database fault must not stop all inference" argument is already
//     answered one layer down: a query failure with a cached list serves the
//     last good list, and the request maps and dispatches as normal (models.go,
//     List). That stale list is the bounded permissive path. It is bounded
//     because every name in it came from a real configuration row.
//  3. Nothing is left to bound in the three conditions above. Reaching them
//     means the gateway holds no list for this project at all, so "permit only
//     a cached name" would permit nothing.
//
// A gateway that must run with no model map at all still can: build the
// Handler with no model resolver. That posture forwards every model unchanged,
// and it is what main.go uses when it has no database pool.
//
// Callers pass the two fields of a decoded request. Every call site holds a
// non-nil request: bifrost's ToBifrost*Request converters return nil only for a
// nil receiver, and each handler builds the receiver as a local value (or, on
// the multipart routes, checks the builder's error first). Keep that true —
// taking the address of a field of a nil request panics at the call site.
func (h *Handler) mapModel(
	w http.ResponseWriter,
	ctx *schemas.BifrostContext,
	provider *schemas.ModelProvider,
	model *string,
) bool {
	// Attach the resolved pair to the request log on the way out, whichever
	// branch below returns.
	//
	// HERE rather than in each handler: every dialect that dispatches passes
	// through this function, so one deferred call covers chat, completions,
	// embeddings, responses, messages, images, audio and realtime. The
	// alternative is a list of seventeen call sites that has to stay complete,
	// which is the shape of defect this codebase keeps finding.
	//
	// Deferred so it records what was RESOLVED rather than what was requested:
	// the mapping rewrites both arguments, and the resolved name is the one the
	// provider actually saw.
	defer func() {
		requestlog.FromContext(ctx).SetModel(string(*provider), *model)
	}()
	if h.models == nil {
		publishRequestModel(ctx, *model)
		// No model map is not an exemption from governance: a deployment that
		// forwards every model unchanged still has authored allowlists and
		// routing rules, and skipping them here would make "no model resolver"
		// a way to bypass the policy.
		return h.applyPolicy(w, ctx, provider, model)
	}
	projectID := identityProjectFromCtx(ctx)
	mo, outcome := h.models.resolve(ctx, projectID, requestModelCandidates(*provider, *model))
	switch outcome {
	case modelResolved:
		// The mapped name can carry its own provider prefix (e.g.
		// "openai/gpt-4o"). Re-split it, keeping the request's provider as the
		// default so an unprefixed wire name does not lose it.
		p, mdl := schemas.ParseModelString(mo.providerModel, *provider)
		// ISSUE #451: the row's linked credential decides the provider, and it
		// OVERRIDES the prefix. The link is the explicit, structured statement
		// of which provider serves this model; a prefix is a substring of a
		// name. The deleted LiteLLM mapper made the same choice: it took
		// custom_llm_provider from the credential type and never from the name.
		//
		// This is also the only path that works for a real row. Measured on the
		// staging dump of 2026-07-09: 48 of 48 chat rows and 8 of 8 embedding
		// rows hold a BARE name, so ParseModelString returns the empty default
		// for every one of them, and bifrost/core rejects an empty provider
		// (core@v1.7.3 utils.go:152-155). Without the link every model row in
		// that database is undispatchable.
		//
		// The provider is assigned as a schemas.ModelProvider value and is never
		// spliced into the model string. That matters: ParseModelString accepts
		// a prefix only when IsKnownProvider admits it, so a name-based fix
		// would fail silently for any provider spelling core does not know.
		if mo.credentialProvider != "" {
			p = mo.credentialProvider
		}
		*provider, *model = p, mdl
		// Pin the credential for the account package. Absent means "the model
		// named no credential", which keeps the whole provider set on offer.
		if link, ok := mo.linkedCredential(); ok {
			ctx.SetValue(account.ContextKeyLinkedCredential, link)
		}
		publishRequestModel(ctx, *model)
		// The authored governance plane runs LAST, on the resolved target: a
		// routing rule may rewrite it, and the model allowlist then judges what
		// the request will actually dispatch to (policy_gate.go).
		if !h.applyPolicy(w, ctx, provider, model) {
			return false
		}
		applyDialRequestShape(ctx)
		return true
	case modelNotAdvertised:
		h.logger.WarnContext(ctx, "model is not configured for this project",
			"project_id", projectID, "model", *model)
		writeModelNotFound(w, *model)
		return false
	case modelSetNoProject:
		// A request condition, not a fault. Log at Warn and answer the caller.
		modelMapRefusedNoProject.Add(1)
		h.logger.WarnContext(ctx, "model map: the request carries no project; no model is reachable",
			"model", *model, "metric", MetricModelMapRefusedNoProject)
		writeModelNotFound(w, *model)
		return false
	case modelSetNoDatabase:
		// A wiring fault. Log at Error: no request on this process can map.
		modelMapRefusedNoDatabase.Add(1)
		h.logger.ErrorContext(ctx, "model map: the model resolver has no database handle; refusing the request",
			"project_id", projectID, "model", *model, "metric", MetricModelMapRefusedNoDatabase)
		writeModelCatalogueUnavailable(w)
		return false
	default: // modelSetLookupFailed
		// A database fault. models.go already logged the query error.
		modelMapRefusedLookupFailed.Add(1)
		h.logger.ErrorContext(ctx, "model map: the model set could not be read and nothing is cached; refusing the request",
			"project_id", projectID, "model", *model, "metric", MetricModelMapRefusedLookupFailed)
		writeModelCatalogueUnavailable(w)
		return false
	}
}

// applyDialRequestShape converts a chat completion into a Responses request
// when the dispatched model uses the AI DIAL `openai` protocol (legacy issue
// #6707). DIAL serves the OpenAI family on /openai/v1/responses only: its
// /openai/v1/chat/completions route answers 404. bifrost/core performs the
// conversion, and converts the answer (and every stream chunk) back to the
// chat shape, when the context carries BifrostContextKeyChangeRequestType.
// core acts on this value on the chat paths only, so a request of another type is
// not changed by it.
//
// It reads the protocol from the pin AFTER the governance plane ran. A routing
// rule that rewrites the target clears the pin (policy_gate.go), so the
// protocol of the model the caller asked for never changes the shape of a
// request that goes somewhere else.
func applyDialRequestShape(ctx *schemas.BifrostContext) {
	link, ok := ctx.Value(account.ContextKeyLinkedCredential).(account.LinkedCredential)
	if !ok || !link.DialProtocol.ConvertsChatToResponses() {
		return
	}
	ctx.SetValue(schemas.BifrostContextKeyChangeRequestType, schemas.ResponsesRequest)
}

// markDispatchKind records the operation this request dispatches. The account
// builds the AI DIAL deployment route from it (legacy issue #6707): DIAL
// serves a chat completion and an embedding on different deployment paths,
// and core resolves the key before it calls the provider.
func markDispatchKind(ctx *schemas.BifrostContext, kind account.DispatchKind) {
	if ctx != nil {
		ctx.SetValue(account.ContextKeyDispatchKind, kind)
	}
}

// publishRequestModel records the model this request dispatches, so the account
// can build the Azure api-version alias for it (issue #455). bifrost accepts a
// per-key api-version only inside Key.Aliases, and it resolves that map by the
// requested model name — but the schemas.Account interface gives the account a
// context and a provider only, so the model must travel on the context.
//
// It is deliberately best effort. A context that is not a *BifrostContext, or a
// handler that never maps a model, simply leaves the value unset, and the key is
// then built with no alias exactly as before.
//
// Issue #469 removed the third call site. An unreadable model set now refuses
// the request, so that path dispatches no model and has none to record.
func publishRequestModel(ctx context.Context, model string) {
	if model == "" {
		return
	}
	if bc, ok := ctx.(*schemas.BifrostContext); ok && bc != nil {
		bc.SetValue(account.ContextKeyRequestModel, model)
	}
}

// writeModelNotFound answers the caller with the OpenAI-shaped 404 for a model
// the project cannot use. The message covers both reasons on purpose: the
// model is not configured, or the caller has no access to it. A caller must
// not be able to tell one from the other.
func writeModelNotFound(w http.ResponseWriter, model string) {
	writeError(w, http.StatusNotFound, "invalid_request_error",
		"the model `"+model+"` does not exist or you do not have access to it",
		"model_not_found")
}

// writeModelCatalogueUnavailable answers the caller when the gateway cannot
// read the model set. 502 keeps the status the deleted elitea-main handler
// used for the same condition (errRoutingUnavailable). The message names no
// project and no model row, so it discloses nothing about the configuration.
func writeModelCatalogueUnavailable(w http.ResponseWriter) {
	writeError(w, http.StatusBadGateway, "api_error",
		"the model catalogue is unavailable; try again shortly",
		"model_catalogue_unavailable")
}
