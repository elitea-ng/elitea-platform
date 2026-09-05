package llmproxy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"unicode"
	"unicode/utf8"
)

// listprovidermodels.go implements POST /llm/v1/list_provider_models: the
// successor to legacy's `import_llm_models` admin task.
//
// # What replaced what
//
// Under LiteLLM, the platform had a REGISTRY to import from: LiteLLM held its
// own model table, and the legacy task read `model_info` out of it and wrote a
// Configuration row for every unmanaged entry it found
// (legacy/plugins/runtime_interface_litellm/methods/admin_tasks.py).
//
// Bifrost has no such table. There is no second inventory to reconcile
// against, and inventing one would be inventing a component. What DOES still
// exist is the only authority that ever mattered: the provider's own model
// listing, reachable with the credential the operator already stored. This
// gateway has spoken those dialects since #319 — the connection checkers call
// exactly these endpoints (GET /models, GET /openai/deployments, GET
// /api/tags, ListFoundationModels, the Model Garden listing) and throw the
// answer away, because all a check needs is the status code.
//
// This route keeps the answer. It is the same request, to the same host, over
// the same guarded client; the only difference is that the response body is
// parsed and the model IDS are returned.
//
// # The gate is the checkers' gate, not a second one
//
// Every destination comes from `checkConnectionProviders[type].dialTargets`
// — the SAME function, not a copy — and every host it returns goes through
// the operator's egress allowlist before anything is dialled, exactly as
// CheckConnection does it. The dial itself uses
// `newCheckConnectionProbeClient`, so the address-level SSRF guard (resolve,
// validate, dial the validated IP, never follow a redirect) is the one guard
// rather than a second implementation of it. A type with a lister but no
// checker entry has no gated destination and is refused; the two maps are
// pinned equal by TestListProviderModels_ListersMatchTheCheckers.
//
// # What crosses the wire
//
// Model IDS ONLY, bounded three ways: at most listProviderModelsCap of them,
// each at most listProviderModelsMaxIDLength bytes, and none carrying a
// control character. The rest of the provider's body — pricing, quotas, error
// text, anything a future provider adds — is dropped by the decode, and the
// failure vocabulary is the checkers' fixed reason set. Nothing here forwards
// a provider's prose to the caller, and a credential is never echoed in any
// branch.
//
// The wire contract is INTERNAL: elitea-main is the only caller (its
// `/admin/gateway/providers/{configID}/models` route resolves the stored
// credential and forwards it here).

// listProviderModelsMaxBody bounds the request body. It is the check request's
// bound, because it IS the check request's body.
const listProviderModelsMaxBody = 8 << 10 // 8 KiB

// listProviderModelsCap bounds how many ids are returned. A provider listing
// is a few hundred entries; this is a bound on what an operator is asked to
// read, and on what one admin click can put in a browser.
const listProviderModelsCap = 500

// listProviderModelsMaxResponseBytes bounds how much of the provider's
// response is read at all. Without it a hostile or broken endpoint on the
// allowlist could stream indefinitely into this pod's memory.
const listProviderModelsMaxResponseBytes = 4 << 20 // 4 MiB

// listProviderModelsMaxIDLength bounds one id. Real ids are well under 100
// bytes; the bound exists so a provider cannot hand back a megabyte "name"
// that this gateway then relays into an admin screen.
const listProviderModelsMaxIDLength = 200

// errProviderListingUnreadable marks a response that ARRIVED and was accepted
// (2xx) but does not decode into the listing shape this build knows.
//
// It is a distinct condition from a refused credential and from an
// unreachable provider, and it must not be reported as either: a 2xx with an
// unreadable body means the endpoint is not the one this dialect expects, and
// telling an operator their key was rejected would send them to rotate a key
// that is fine.
var errProviderListingUnreadable = errors.New("the provider's model listing could not be read")

// listProviderModelsResponse is the gateway → elitea-main wire contract.
//
// Success is true only when a provider actually answered a listing. Models is
// always present (never null) so a caller cannot read "no field" as "not
// implemented"; Truncated says the cap was reached, so an empty tail is never
// mistaken for the provider having nothing more.
type listProviderModelsResponse struct {
	Success   bool     `json:"success"`
	Reason    string   `json:"reason,omitempty"`
	Detail    string   `json:"detail,omitempty"`
	Models    []string `json:"models"`
	Truncated bool     `json:"truncated,omitempty"`
}

// providerModelLister performs one provider's real, read-only model listing
// and returns the ids it named.
type providerModelLister func(ctx context.Context, client *http.Client, req checkConnectionRequest) ([]string, error)

// providerModelListers covers exactly the types checkConnectionProviders
// covers. The pairing is deliberate and is asserted by a test: the checker
// entry owns the destination (dialTargets) and the private-network carve-out,
// so a lister without one would be a dial with no gate, and a checker without
// a lister is a type this route honestly reports as unsupported.
var providerModelListers = map[string]providerModelLister{
	"open_ai":        listOpenAICompatibleModels,
	"azure_open_ai":  listAzureDeployments,
	"open_ai_azure":  listAzureDeployments,
	"ai_dial":        listAzureDeployments,
	"ollama":         listOllamaTags,
	"vllm":           listOpenAICompatibleModels,
	"amazon_bedrock": listBedrockFoundationModels,
	"vertex_ai":      listVertexPublisherModels,
}

// ListProviderModels answers with the model ids the supplied credential can
// see at its provider. It writes nothing, invokes no model, and bills nothing:
// every call it makes is the provider's own listing endpoint.
func (h *Handler) ListProviderModels(w http.ResponseWriter, r *http.Request) {
	if !verifySignature(r.Header, h.identitySecret) {
		writeError(w, http.StatusForbidden, "permission_error", "invalid identity signature", "")
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, listProviderModelsMaxBody)
	var req checkConnectionRequest
	if !decodeJSON(w, r, &req) {
		return
	}

	lister, listable := providerModelListers[req.Type]
	provider, checkable := checkConnectionProviders[req.Type]
	// BOTH are required. The checker entry is where the destination and the
	// private-network rule come from, so a lister reached without one would
	// be an ungated dial — refused here rather than trusted to the map.
	if !listable || !checkable {
		writeJSON(w, http.StatusOK, listProviderModelsResponse{
			Reason: checkConnectionReasonUnsupported,
			Models: []string{},
		})
		return
	}

	// Resolve every host BEFORE anything is dialled, from the checker's own
	// function, so a payload that cannot name a legitimate destination is
	// refused with the offending FIELD named and no value echoed.
	targets, err := provider.dialTargets(req)
	if err != nil {
		var fieldErr *checkConnectionFieldError
		if errors.As(err, &fieldErr) {
			writeJSON(w, http.StatusOK, listProviderModelsResponse{
				Reason: fieldErr.reason, Detail: fieldErr.detail, Models: []string{},
			})
			return
		}
		h.logger.WarnContext(r.Context(), "list_provider_models: could not resolve a target",
			"type", req.Type, "err", err)
		writeJSON(w, http.StatusOK, listProviderModelsResponse{
			Reason: checkConnectionReasonUnreachable,
			Detail: "could not reach the provider",
			Models: []string{},
		})
		return
	}

	// Fail closed, exactly as the checker does: no policy wired, or no gated
	// destination, refuses rather than dialling.
	if h.egressPolicy == nil || len(targets) == 0 {
		h.logger.WarnContext(r.Context(), "list_provider_models: no egress policy or no gated target",
			"type", req.Type)
		writeJSON(w, http.StatusOK, listProviderModelsResponse{
			Reason: checkConnectionReasonEgress, Models: []string{},
		})
		return
	}
	for _, target := range targets {
		if !h.egressPolicy.EgressAllows(target) {
			h.logger.WarnContext(r.Context(), "list_provider_models: host is not on the egress allowlist",
				"type", req.Type)
			writeJSON(w, http.StatusOK, listProviderModelsResponse{
				Reason: checkConnectionReasonEgress, Models: []string{},
			})
			return
		}
	}

	allowPrivate := checkConnectionAllowsPrivateNetwork(req) && h.egressPolicy.EgressAllowlistConfigured()
	client := newCheckConnectionProbeClient(allowPrivate)

	ctx, cancel := context.WithTimeout(r.Context(), checkConnectionProbeTimeout)
	defer cancel()

	ids, err := lister(ctx, client, req)
	if err != nil {
		reason, detail := classifyListProviderModelsError(err)
		h.logger.WarnContext(r.Context(), "list_provider_models: provider listing failed",
			"type", req.Type, "reason", reason, "err", err)
		writeJSON(w, http.StatusOK, listProviderModelsResponse{
			Reason: reason, Detail: detail, Models: []string{},
		})
		return
	}

	models, truncated := boundProviderModelIDs(ids)
	writeJSON(w, http.StatusOK, listProviderModelsResponse{
		Success:   true,
		Reason:    checkConnectionReasonOK,
		Models:    models,
		Truncated: truncated,
	})
}

// classifyListProviderModelsError maps a listing failure onto the checkers'
// fixed reason vocabulary.
//
// An unreadable 2xx body is reported as an UPSTREAM error, never as a rejected
// credential: the credential was accepted, and the endpoint answered something
// this dialect does not describe.
func classifyListProviderModelsError(err error) (reason, detail string) {
	if errors.Is(err, errProviderListingUnreadable) {
		return checkConnectionReasonUpstream, errProviderListingUnreadable.Error()
	}
	return classifyCheckConnectionProbeError(err)
}

// boundProviderModelIDs applies the three bounds this route promises, and
// drops duplicates.
//
// Order is the PROVIDER's, not sorted: a provider lists its own catalogue in
// an order that is frequently meaningful (newest first, families together),
// and re-sorting it here would be this gateway inventing a ranking.
//
// A rejected id is dropped silently rather than reported. It is not an error
// about the credential, and a per-id complaint would be the provider's body
// text arriving under another name.
func boundProviderModelIDs(ids []string) (bounded []string, truncated bool) {
	bounded = make([]string, 0, len(ids))
	seen := make(map[string]struct{}, len(ids))
	for _, raw := range ids {
		id := strings.TrimSpace(raw)
		if !validProviderModelID(id) {
			continue
		}
		if _, duplicate := seen[id]; duplicate {
			continue
		}
		if len(bounded) >= listProviderModelsCap {
			// Reported, not silently dropped: an operator who cannot find a
			// model in the list has to be able to tell "the provider does not
			// offer it" from "the list stopped early".
			return bounded, true
		}
		seen[id] = struct{}{}
		bounded = append(bounded, id)
	}
	return bounded, false
}

// validProviderModelID reports whether one id may be relayed.
//
// A control character is refused because these strings are rendered in an
// admin screen and become a configuration row's name; valid UTF-8 is required
// for the same reason.
func validProviderModelID(id string) bool {
	if id == "" || len(id) > listProviderModelsMaxIDLength || !utf8.ValidString(id) {
		return false
	}
	for _, r := range id {
		if unicode.IsControl(r) {
			return false
		}
	}
	return true
}

// listProviderModelsGET performs one read-only listing call and decodes it.
func listProviderModelsGET(
	ctx context.Context, client *http.Client, rawURL string, headers map[string]string, dst any,
) error {
	u, err := url.Parse(rawURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return &checkConnectionProbeError{
			err: fmt.Errorf("list_provider_models: api_base does not yield a valid http(s) url"),
		}
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return &checkConnectionProbeError{err: err}
	}
	for key, value := range headers {
		if value != "" {
			httpReq.Header.Set(key, value)
		}
	}
	return listProviderModelsFetch(client, httpReq, dst)
}

// listProviderModelsFetch sends an already-built request and decodes a BOUNDED
// prefix of the answer.
//
// It is split from listProviderModelsGET for the reason checkConnectionProbeDo
// is: the Bedrock call must be SIGNED before it is sent, and a header added
// after signing invalidates the signature.
func listProviderModelsFetch(client *http.Client, httpReq *http.Request, dst any) error {
	resp, err := client.Do(httpReq)
	if err != nil {
		return &checkConnectionProbeError{err: err}
	}
	defer func() {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
		_ = resp.Body.Close()
	}()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return &checkConnectionProbeError{
			status: resp.StatusCode,
			err:    fmt.Errorf("list_provider_models: provider returned status %d", resp.StatusCode),
		}
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, listProviderModelsMaxResponseBytes)).Decode(dst); err != nil {
		// The decode error quotes the body, so it is wrapped behind a sentinel
		// and only the sentinel's own wording is ever returned to the caller.
		return fmt.Errorf("%w: %w", errProviderListingUnreadable, err)
	}
	return nil
}

// --- openai-compatible ----------------------------------------------------

// openAIModelListing is the envelope GET /models answers with, and the one
// Azure's deployments listing answers with too.
//
// `id` is what a caller addresses; `model` is read only as a fallback, for the
// Azure shape where a deployment names the model it serves.
type openAIModelListing struct {
	Data []struct {
		ID    string `json:"id"`
		Model string `json:"model"`
	} `json:"data"`
}

func (l openAIModelListing) ids() []string {
	ids := make([]string, 0, len(l.Data))
	for _, entry := range l.Data {
		if entry.ID != "" {
			ids = append(ids, entry.ID)
			continue
		}
		ids = append(ids, entry.Model)
	}
	return ids
}

// listOpenAICompatibleModels reads GET {api_base}/models — the same call
// probeOpenAICompatibleModels makes, with the body kept.
func listOpenAICompatibleModels(ctx context.Context, client *http.Client, req checkConnectionRequest) ([]string, error) {
	headers := map[string]string{}
	if req.APIKey != "" {
		headers["Authorization"] = "Bearer " + req.APIKey
	}
	var listing openAIModelListing
	if err := listProviderModelsGET(ctx, client,
		checkConnectionJoinURL(req.APIBase, "/models"), headers, &listing); err != nil {
		return nil, err
	}
	return listing.ids(), nil
}

// listAzureDeployments reads GET {api_base}/openai/deployments — the same call
// probeAzureDeployments makes.
//
// An Azure DEPLOYMENT id is what a caller addresses, not the underlying model
// name, which is why the envelope's `id` is preferred over its `model`.
func listAzureDeployments(ctx context.Context, client *http.Client, req checkConnectionRequest) ([]string, error) {
	apiVersion := req.APIVersion
	if apiVersion == "" {
		apiVersion = defaultAzureAPIVersion
	}
	target := checkConnectionJoinURL(req.APIBase, "/openai/deployments") +
		"?api-version=" + url.QueryEscape(apiVersion)
	headers := map[string]string{}
	if req.APIKey != "" {
		headers["api-key"] = req.APIKey
	}
	var listing openAIModelListing
	if err := listProviderModelsGET(ctx, client, target, headers, &listing); err != nil {
		return nil, err
	}
	return listing.ids(), nil
}

// --- ollama ---------------------------------------------------------------

// ollamaTagListing is GET /api/tags' envelope. `name` carries the tag
// (`llama3:latest`), which is what a caller addresses.
type ollamaTagListing struct {
	Models []struct {
		Name  string `json:"name"`
		Model string `json:"model"`
	} `json:"models"`
}

func listOllamaTags(ctx context.Context, client *http.Client, req checkConnectionRequest) ([]string, error) {
	var listing ollamaTagListing
	if err := listProviderModelsGET(ctx, client,
		checkConnectionJoinURL(req.APIBase, "/api/tags"), nil, &listing); err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(listing.Models))
	for _, entry := range listing.Models {
		if entry.Name != "" {
			ids = append(ids, entry.Name)
			continue
		}
		ids = append(ids, entry.Model)
	}
	return ids, nil
}

// --- amazon_bedrock -------------------------------------------------------

// bedrockFoundationModelListing is ListFoundationModels' envelope.
type bedrockFoundationModelListing struct {
	ModelSummaries []struct {
		ModelID string `json:"modelId"`
	} `json:"modelSummaries"`
}

// listBedrockFoundationModels signs and sends the same control-plane
// ListFoundationModels call probeBedrockFoundationModels makes — through the
// same signer, over credentials built ONLY from the supplied fields, never
// through the pod's own IAM role.
func listBedrockFoundationModels(ctx context.Context, client *http.Client, req checkConnectionRequest) ([]string, error) {
	httpReq, err := signedBedrockControlPlaneRequest(ctx, req, "/foundation-models")
	if err != nil {
		return nil, err
	}
	var listing bedrockFoundationModelListing
	if err := listProviderModelsFetch(client, httpReq, &listing); err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(listing.ModelSummaries))
	for _, entry := range listing.ModelSummaries {
		ids = append(ids, entry.ModelID)
	}
	return ids, nil
}

// --- vertex_ai ------------------------------------------------------------

// vertexPublisherModelListing is the Model Garden listing's envelope. `name`
// is a resource path (`publishers/google/models/gemini-1.5-pro`); the id a
// caller addresses is its last segment.
type vertexPublisherModelListing struct {
	PublisherModels []struct {
		Name string `json:"name"`
	} `json:"publisherModels"`
	NextPageToken string `json:"nextPageToken"`
}

// vertexListPublishers are the Model Garden publishers this route walks.
//
// They are bifrost's own list, in bifrost's own order (providers/vertex), and
// that is what makes the answer useful: on Vertex the Claude and Mistral models
// are published under their vendors' names, not Google's, so a listing of
// `publishers/google` alone would offer an operator none of the models this
// gateway can actually dispatch for them.
//
// GOOGLE MUST STAY FIRST. The other two are regionally optional — a location
// that does not carry them answers 403 or 404 — so a failure from them is
// skipped rather than reported. Any failure that IS about the credential
// (a refused token, an unreachable host) therefore has to surface from the
// publisher that is always there, and it does, because it runs first.
var vertexListPublishers = []string{"google", "anthropic", "mistralai"}

// vertexListPageSize is the page size bifrost uses for the same call
// (providers/vertex, MaxPageSize). Asking for more is not obviously accepted:
// Google's own guidance is to coerce an oversized page down, but this route
// has no way to prove a given location does, and a refused page would report a
// working credential as an upstream error.
const vertexListPageSize = 100

// vertexListMaxPages bounds the pages read PER PUBLISHER. Following
// `nextPageToken` without a bound makes the number of round trips a function
// of the provider's answer; three pages is 300 models per publisher, well past
// the cap this route returns.
const vertexListMaxPages = 3

// listVertexPublisherModels mints a token from the service-account document
// and walks the location's Model Garden — the same two-step exchange
// probeVertexPublisherModels performs, both hops through the guarded client
// and both hosts already gated by the checker's own dialTargets.
func listVertexPublisherModels(ctx context.Context, client *http.Client, req checkConnectionRequest) ([]string, error) {
	_, location, err := vertexCheckCredential(req)
	if err != nil {
		return nil, err
	}
	token, err := mintVertexAccessToken(ctx, client, req)
	if err != nil {
		return nil, err
	}

	host := vertexCheckAPIHost(location)
	headers := map[string]string{"Authorization": "Bearer " + token}
	ids := make([]string, 0, listProviderModelsCap)

	for _, publisher := range vertexListPublishers {
		pageToken := ""
		for page := 0; page < vertexListMaxPages; page++ {
			target := fmt.Sprintf("https://%s/v1beta1/publishers/%s/models?pageSize=%d",
				host, publisher, vertexListPageSize)
			if pageToken != "" {
				target += "&pageToken=" + url.QueryEscape(pageToken)
			}

			var listing vertexPublisherModelListing
			if err := listProviderModelsGET(ctx, client, target, headers, &listing); err != nil {
				if publisher != vertexListPublishers[0] {
					// Regional availability, not a verdict about the
					// credential — and never reported as one. Google already
					// answered, so anything actually wrong with the credential
					// has already surfaced.
					break
				}
				return nil, err
			}
			for _, entry := range listing.PublisherModels {
				ids = append(ids, vertexPublisherModelID(entry.Name))
			}
			if listing.NextPageToken == "" || len(ids) >= listProviderModelsCap {
				break
			}
			pageToken = listing.NextPageToken
		}
		if len(ids) >= listProviderModelsCap {
			break
		}
	}
	return ids, nil
}

// vertexPublisherModelID reduces a publisher-model resource path to the id a
// caller addresses. A name with no slash is already that id.
func vertexPublisherModelID(name string) string {
	trimmed := strings.TrimRight(strings.TrimSpace(name), "/")
	if index := strings.LastIndex(trimmed, "/"); index >= 0 {
		return trimmed[index+1:]
	}
	return trimmed
}
