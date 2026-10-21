// provider_model_listing.go is elitea-main's half of the successor to
// legacy's `import_llm_models`.
//
// # What legacy did, and what replaced it
//
// The legacy admin task read LiteLLM's own model table (`model_info`) and
// wrote a Configuration row for every entry it found that this platform did
// not already manage
// (legacy/plugins/runtime_interface_litellm/methods/admin_tasks.py).
//
// Bifrost keeps no such table, so there is no registry to reconcile against
// and no honest way to resurrect that task as written. What is still there is
// the authority the registry was only ever a cache of: the PROVIDER's own
// model listing, reachable with the credential the operator already stored.
//
// The gateway speaks those dialects already — its connection checkers call
// GET /models, GET /openai/deployments, GET /api/tags, Bedrock's
// ListFoundationModels and Vertex's Model Garden listing, and discard the
// body. `POST /llm/v1/list_provider_models` makes the same call and keeps the
// ids. This file is the client for it.
//
// # Why the round trip stays in the gateway
//
// For the reason check_connection.go states at length: the gateway owns the
// SSRF-safe egress allowlist for a tenant-authored `api_base` (#13), and
// elitea-main must not reach the same provider a second, unguarded way. The
// listing route reuses the checkers' own `dialTargets`, allowlist gate and
// address-validating dialer, so this is not a second path to a provider — it
// is the same path with the answer kept.
package configurations

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/llmproxy"
)

// maxProviderModelIDs bounds how many ids this platform will accept from ONE
// listing.
//
// The gateway caps its own answer at the same number. Restating the bound here
// is not distrust of that cap for its own sake: this process decodes whatever
// arrives on the socket, and a bound that lives only at the other end of a
// wire is not a bound on what this process allocates.
const maxProviderModelIDs = 500

// maxProviderModelIDLength bounds one id, for the same reason the gateway
// bounds it: these strings are rendered in an admin screen and become the
// name of a configuration row.
const maxProviderModelIDLength = 200

// maxProviderModelListingBytes bounds how much of the gateway's answer is read
// at all.
const maxProviderModelListingBytes = 1 << 20 // 1 MiB

// ProviderModelListing is one provider's answer to "which models can this
// credential see".
//
// It carries NAMES ONLY. Nothing else of the provider's response reaches this
// process, and nothing about the credential is echoed back in any field.
type ProviderModelListing struct {
	// Success is true only when a provider actually answered a listing.
	Success bool
	// Message is always safe to return to the browser: it comes from the
	// gateway's fixed reason vocabulary, never from a provider's response
	// body.
	Message string
	// Models are the provider's model ids, in the provider's own order.
	Models []string
	// Truncated reports that the listing was cut at the cap. Without it a
	// short list reads as the provider's whole catalogue, and an operator
	// concludes a model is not offered when it simply was not reached.
	Truncated bool
}

// ProviderModelLister reads the model ids a credential can see at its
// provider.
//
// Implementations MUST NOT dial a provider directly: the round trip belongs to
// the gateway, which owns the egress allowlist. Implementations MUST NOT
// report Success without a provider having answered.
type ProviderModelLister interface {
	ListProviderModels(ctx context.Context, configType string, data map[string]any) (ProviderModelListing, error)
}

// providerModelsResponseBody is the gateway's reply (mirrors
// elitea-llm-gateway/internal/llmproxy.listProviderModelsResponse).
type providerModelsResponseBody struct {
	Success   bool     `json:"success"`
	Reason    string   `json:"reason"`
	Detail    string   `json:"detail"`
	Models    []string `json:"models"`
	Truncated bool     `json:"truncated"`
}

// ListProviderModels implements ProviderModelLister on the SAME client the
// stored connection check already uses.
//
// It is a method on GatewayConnectionChecker rather than a second client for
// one reason that matters: the two calls are the same hop, with the same mTLS
// transport, the same identity signature and the same operator configuration.
// A second client would be a second place for that configuration to be wrong,
// and its failure mode would be an admin screen that can test a credential but
// not read its models, with nothing on either screen saying why.
//
// A transport-level failure is returned as an error — the caller must NOT map
// that to an empty catalogue, which would read as "this provider offers no
// models".
func (c *GatewayConnectionChecker) ListProviderModels(
	ctx context.Context, configType string, data map[string]any,
) (ProviderModelListing, error) {
	if c == nil {
		// A typed nil boxed into the ProviderModelLister interface makes the
		// caller's `lister == nil` test FALSE — the trap recorded on
		// WithProviderAdmission. The guard belongs here as well as at the
		// composition root, because only this side can tell a nil receiver
		// from a working client, and the cost must be the answer rather than
		// the process.
		return ProviderModelListing{}, errors.New("list provider models: the gateway client is not composed")
	}
	body := checkConnectionRequestBody{
		Type:       configType,
		APIBase:    strVal(data, "api_base"),
		APIKey:     firstStrVal(data, "api_key", "api_token"),
		APIVersion: strVal(data, "api_version"),

		AWSAccessKeyID:     strVal(data, "aws_access_key_id"),
		AWSSecretAccessKey: strVal(data, "aws_secret_access_key"),
		AWSSessionToken:    strVal(data, "aws_session_token"),
		AWSRegionName:      strVal(data, "aws_region_name"),

		VertexProject:  strVal(data, "vertex_project"),
		VertexLocation: strVal(data, "vertex_location"),
		// Forwarded as the caller stored it, not coerced: the service-account
		// document is an escaped JSON string on one screen and a nested object
		// on another, and the gateway accepts both.
		VertexCredentials: data["vertex_credentials"],

		AuthMode:      strVal(data, "auth_mode"),
		TokenAudience: strVal(data, "token_audience"),
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return ProviderModelListing{}, fmt.Errorf("list provider models: encode request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.baseURL+"/llm/v1/list_provider_models", strings.NewReader(string(raw)))
	if err != nil {
		return ProviderModelListing{}, fmt.Errorf("list provider models: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	// No execution id: a connection check is authored by an operator in the
	// admin UI, not made from a runtime execution. The empty value signs v1,
	// byte for byte what this call signed before.
	llmproxy.SignIdentityHeaders(req.Header, c.identitySecret, connectionCheckProjectIDFrom(ctx), "", "", "")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return ProviderModelListing{}, fmt.Errorf("list provider models: call gateway: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		// The route answers 200 for every provider verdict, so a non-200 is
		// this hop failing — an unmounted route on an older gateway, a refused
		// signature, a proxy error. It is never a verdict about the credential.
		return ProviderModelListing{}, fmt.Errorf("list provider models: gateway responded with status %d", resp.StatusCode)
	}

	var out providerModelsResponseBody
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxProviderModelListingBytes)).Decode(&out); err != nil {
		return ProviderModelListing{}, fmt.Errorf("list provider models: decode gateway response: %w", err)
	}
	if !out.Success {
		return ProviderModelListing{
			Message: connectionCheckMessageFor(out.Reason, out.Detail),
		}, nil
	}

	models, truncated := boundProviderModelIDs(out.Models)
	return ProviderModelListing{
		Success:   true,
		Models:    models,
		Truncated: out.Truncated || truncated,
	}, nil
}

// boundProviderModelIDs applies this process's own bounds to the ids the
// gateway sent, and drops duplicates.
//
// The provider's ORDER is kept: a provider lists its catalogue in an order
// that is frequently meaningful, and re-sorting it here would be this platform
// inventing a ranking.
func boundProviderModelIDs(ids []string) (bounded []string, truncated bool) {
	bounded = make([]string, 0, len(ids))
	seen := make(map[string]struct{}, len(ids))
	for _, raw := range ids {
		id := strings.TrimSpace(raw)
		if id == "" || len(id) > maxProviderModelIDLength {
			continue
		}
		if _, duplicate := seen[id]; duplicate {
			continue
		}
		if len(bounded) >= maxProviderModelIDs {
			return bounded, true
		}
		seen[id] = struct{}{}
		bounded = append(bounded, id)
	}
	return bounded, false
}
