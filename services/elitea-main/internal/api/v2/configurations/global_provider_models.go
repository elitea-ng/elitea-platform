// global_provider_models.go serves POST
// /api/v2/admin/gateway/providers/{configID}/models: the model ids a SAVED
// platform credential can see at its provider.
//
// # Why this route exists
//
// It is the read half of the `import_llm_models` successor. Legacy imported
// from LiteLLM's own model table; there is no such table under Bifrost, so the
// provider's own listing is the inventory — see provider_model_listing.go for
// the whole argument.
//
// The write half is NOT here, deliberately. This route answers with names; the
// admin panel then creates each adopted model through the existing platform
// model surface (`POST /admin/gateway/platform_models`, global_models.go),
// which is the one place that derives a model's `section`, validates its
// credential link against the platform's own providers, and runs provider
// admission for `status_ok`. A bulk writer here would be a second author of
// those rows, and the two would then have to agree about all of it forever —
// which is the argument global_providers.go's own header makes for delegating
// its writes.
//
// # The credential is never re-typed and never returned
//
// The row is read server-side and resolved through the SAME
// StoredConfigurationResolver the stored connection check consumes
// (stored_check.go, which this file does not modify). That is what makes the
// route possible at all: a platform credential's api_key is sealed in the
// public project's vault, so an operator holding `configuration.governance`
// has no copy of it to send. The resolved payload goes to the gateway and is
// dropped; no branch here logs it, returns it, or persists it.
//
// # What it must NOT do
//
//   - It must NOT write. Reading a provider's catalogue is not a decision to
//     adopt any of it, and a route that silently created rows would be the
//     legacy task's own failure mode: a scheduled import that published models
//     nobody chose, into every project on the platform.
//   - It must NOT dial the provider. The gateway owns the SSRF-safe egress
//     allowlist (#13); this handler reaches it through the same client the
//     stored check uses.
//   - It must NOT report an empty list for a failure. "This provider offers no
//     models" and "the listing could not be read" are different facts, and the
//     first is the one an operator would act on.
package configurations

import (
	"context"
	"errors"
	"log/slog"
	"math"
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/pkg/apierr"
)

// providerModelListingUnavailableMessage is the answer when this deployment
// cannot list models at all — no gateway client, or a gateway too old to serve
// the route. It never fabricates an empty catalogue.
const providerModelListingUnavailableMessage = "Reading a provider's models is not available right now."

// ListGlobalProviderModels serves POST /admin/gateway/providers/{configID}/models.
//
// The answer is `{"models":[…],"total":n,"truncated":bool,"type":"open_ai"}`
// on HTTP 200, and `{"error":"…"}` on every failure — the shape the three CRUD
// verbs on this surface already answer with, and the one the admin client
// reads its message out of. It deliberately does NOT borrow the check route's
// `{"success":false,"message":…}` contract: that shape exists because one
// browser control renders a check's 200 and its 400 identically, and nothing
// here is rendered that way.
func (h *Handler) ListGlobalProviderModels(w http.ResponseWriter, r *http.Request) {
	request, ok := h.pinPublicProject(w, r)
	if !ok {
		return
	}
	// The same fence the check and the revalidation take. WITHOUT IT THIS
	// ROUTE ADDRESSES THE WHOLE PUBLIC PROJECT BY ID: a toolkit credential's
	// id would make the platform resolve and unseal that row's secrets and
	// hand them to the gateway, under a permission granted for governance.
	switch err := h.admitGlobalProviderRow(request); {
	case errors.Is(err, errGlobalProviderRowUnreadable):
		// A failed READ is not a deleted credential — the distinction the two
		// neighbouring routes keep, for the reason stored_check.go states: a
		// saturated pool must not tell an operator their credentials are gone.
		apierr.WriteStatus(w, http.StatusInternalServerError,
			"the platform provider could not be read, so its models could not be listed")
		return
	case err != nil:
		apierr.WriteStatus(w, http.StatusNotFound, "configuration not found")
		return
	}

	h.writeStoredProviderModels(w, request)
}

// writeStoredProviderModels reads the stored row and writes the one answer.
//
// The row READ lives here and the DECISION lives in listStoredRowModels, the
// split stored_check.go makes for the same reason: the decision — what may be
// listed, what resolves, what the provider answered — is then testable against
// a fabricated row, and every one of its branches can be exercised without a
// database standing in the way.
func (h *Handler) writeStoredProviderModels(w http.ResponseWriter, r *http.Request) {
	projectID := chi.URLParam(r, "projectID")
	configID := chi.URLParam(r, "configID")
	schema, schemaOK := tenantSchema(w, projectID)
	if !schemaOK {
		return
	}
	ctx := r.Context()

	// No request body is read, and that is the point: there is nothing a
	// client could send that this listing needs. The api_key it would have to
	// carry is sealed in the vault and is not on this screen.
	row, found, err := h.loadStoredConfigurationRow(ctx, schema, configID)
	if err != nil {
		// A failed READ is not a missing row: reporting a saturated pool as a
		// deleted credential sends an operator to re-create one that is there.
		slog.ErrorContext(ctx, "list_provider_models: read the configuration row failed",
			"project_id", projectID, "configuration_id", configID, "err", err)
		apierr.WriteStatus(w, http.StatusInternalServerError,
			"the platform provider could not be read, so its models could not be listed")
		return
	}
	if !found {
		apierr.WriteStatus(w, http.StatusNotFound, "configuration not found")
		return
	}

	body, failure := h.listStoredRowModels(ctx, projectID, row)
	if failure != nil {
		apierr.WriteStatus(w, failure.status, failure.message)
		return
	}
	writeJSON(w, http.StatusOK, body)
}

// providerModelsFailure is one refusal: the status the route answers and the
// sentence the operator reads. The sentence is always safe — it never carries
// a provider body, an internal error or any part of the credential.
type providerModelsFailure struct {
	status  int
	message string
}

// listStoredRowModels is the whole decision for one stored row: what may be
// listed, what resolves, and what the provider answered.
//
// It writes nothing to the database, in any branch. Reading a catalogue is not
// admission and not adoption.
func (h *Handler) listStoredRowModels(
	ctx context.Context, projectID string, row storedConfigurationRow,
) (map[string]any, *providerModelsFailure) {
	if _, listable := checkableConnectionTypes[row.configType]; !listable {
		// The SAME set the connection check admits, and the same set the
		// gateway's listers cover. A type outside it is a missing feature, not
		// a broken credential, and it says so.
		return nil, &providerModelsFailure{
			status:  http.StatusBadRequest,
			message: "reading the available models is not supported yet for configuration type " + row.configType,
		}
	}

	lister := h.providerModelLister()
	if h.storedResolver == nil || lister == nil {
		slog.ErrorContext(ctx, "list_provider_models: the listing is not composed",
			"type", row.configType, "project_id", projectID,
			"resolver", h.storedResolver != nil, "lister", lister != nil)
		return nil, &providerModelsFailure{
			status: http.StatusServiceUnavailable, message: providerModelListingUnavailableMessage,
		}
	}

	resolution, resolutionOK := storedResolutionFor(projectID, row)
	if !resolutionOK {
		// tenantSchema already refused a non-decimal id, so this is the
		// out-of-range case only.
		return nil, &providerModelsFailure{
			status: http.StatusBadRequest, message: providerModelListingUnavailableMessage,
		}
	}
	resolved, err := h.storedResolver.ResolveStoredConfiguration(ctx, resolution)
	if err != nil || resolved == nil {
		// The row's references do not expand, or its secret does not redeem.
		// That is a real failure of THIS credential — the gateway would refuse
		// it too — and the cause never reaches the browser.
		slog.WarnContext(ctx, "list_provider_models: the stored configuration did not resolve",
			"type", row.configType, "project_id", projectID, "configuration_id", row.id, "err", err)
		return nil, &providerModelsFailure{
			status: http.StatusBadRequest,
			message: "This credential could not be resolved. Check that its secret and any " +
				"referenced configuration still exist.",
		}
	}
	// The guard runs on the RESOLVED payload, because expansion can merge a
	// referenced row's api_base into it: the value that reaches the provider is
	// the value the guard has to see.
	if err := validateNotSelfReferential(resolved, selfLLMOrigins()); err != nil {
		return nil, &providerModelsFailure{status: http.StatusBadRequest, message: err.Error()}
	}

	listing, err := lister.ListProviderModels(
		WithConnectionCheckProjectID(ctx, projectID), row.configType, resolved)
	if err != nil {
		// A transport failure must never be reported as an empty catalogue.
		slog.ErrorContext(ctx, "list_provider_models: gateway call failed",
			"type", row.configType, "project_id", projectID, "configuration_id", row.id, "err", err)
		return nil, &providerModelsFailure{
			status:  http.StatusBadGateway,
			message: "Could not read this provider's models right now. Please try again.",
		}
	}
	if !listing.Success {
		message := listing.Message
		if message == "" {
			message = "Could not read this provider's models."
		}
		return nil, &providerModelsFailure{status: http.StatusBadRequest, message: message}
	}

	models := listing.Models
	if models == nil {
		// Never null on the wire: a client cannot tell a null field from a
		// missing one, and both read as "this build does not answer that".
		models = []string{}
	}
	return map[string]any{
		"models": models,
		"total":  len(models),
		// Stated, so a short list is never read as the provider's whole
		// catalogue.
		"truncated": listing.Truncated,
		// Echoed so the caller can label what it is adopting from without a
		// second read of the listing.
		"type": row.configType,
	}, nil
}

// providerModelLister reports the listing capability of the composed gateway
// client, or nil when this deployment has none.
//
// It is READ OFF THE CONNECTION CHECKER rather than wired as its own option,
// and that is deliberate. The two calls are the same hop, to the same gateway,
// with the same mTLS settings and the same identity secret; a second option
// would be a second thing for a composition root to forget, and the failure
// would be silent — a screen that can test a credential but cannot read its
// models, with nothing saying why. Taking the capability off the client that
// is already composed makes the two impossible to configure apart.
//
// A nil checker type-asserts to false, so a deployment with no gateway gets a
// nil lister and the honest "not available" answer, never a nil-receiver call.
func (h *Handler) providerModelLister() ProviderModelLister {
	if h.connectionChecker == nil {
		return nil
	}
	lister, ok := h.connectionChecker.(ProviderModelLister)
	if !ok {
		return nil
	}
	return lister
}

// storedResolutionFor builds the resolver's input for one stored row.
//
// The project id is the one the SCHEMA was built from, never a value out of
// the row's own JSON: it decides which vault redeems the row's secrets, so a
// truncated or out-of-range id would redeem another project's vault and is
// refused rather than narrowed. That is stored_check.go's rule, applied here
// to the same resolver.
func storedResolutionFor(projectID string, row storedConfigurationRow) (StoredConfigurationResolution, bool) {
	owner, err := strconv.Atoi(projectID)
	if err != nil || owner <= 0 || owner > math.MaxInt32 {
		return StoredConfigurationResolution{}, false
	}
	resolution := StoredConfigurationResolution{ProjectID: int32(owner), Data: row.data}
	if row.authorID != nil && *row.authorID > 0 && *row.authorID <= math.MaxInt32 {
		author := int32(*row.authorID)
		resolution.AuthorID = &author
	}
	return resolution, true
}

// compile-time assertion that the composed gateway client really carries the
// listing capability. Without it, the type assertion in providerModelLister
// would silently answer "not available" for every deployment the day the
// method's signature drifted from the interface — the dead-wiring failure this
// repository keeps meeting, in the one shape a compiler can catch.
var _ ProviderModelLister = (*GatewayConnectionChecker)(nil)
