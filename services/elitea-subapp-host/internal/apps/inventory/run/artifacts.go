package run

import (
	"fmt"
	"regexp"

	"github.com/EliteaAI/elitea-platform/services/elitea-subapp-host/internal/artifacts"
)

// The artifact upload path. The transport — the platform's artifact route, the
// bearer, the multipart shape, the refusal texts — is internal/artifacts,
// shared with DeepWiki, because none of it belongs to either application.
//
// What is NOT shared is the derivation below: reading a base URL, a key and a
// project out of the legacy `llm_settings` dict is the PLUGIN's contract. It is
// the same rule in both plugins, and it is written twice on purpose — a shared
// helper would make one plugin's contract change silently change the other's.

// ArtifactClient puts one object into a bucket.
type ArtifactClient = artifacts.Client

// ArtifactClientFactory builds the client for one invocation.
type ArtifactClientFactory = artifacts.Factory

// ArtifactSettings is the transport derived from one request's llm_settings.
type ArtifactSettings = artifacts.Settings

// DefaultAPIPath is the platform API prefix the artifact routes live under.
const DefaultAPIPath = artifacts.DefaultAPIPath

var llmSuffix = regexp.MustCompile(`/llm(/api)?(/v\d+)?/?$`)

// ExtractArtifactSettings is the legacy derivation: api_base or
// openai_api_base with `/llm[/api][/vN]` stripped; api_key or openai_api_key;
// the project from organization, openai_organization or project_id, in that
// order.
//
// TODO(#1359 U2/U5): the engine still reaches elitea-main over HTTP at this
// api_base: its /llm routes for every model call and the artifact routes
// for reads and uploads. Moving them is the LLM and artifacts units' work.
func ExtractArtifactSettings(llmSettings map[string]any) ArtifactSettings {
	apiBase := firstTruthy(llmSettings["api_base"], llmSettings["openai_api_base"], "")
	apiKey := firstTruthy(llmSettings["api_key"], llmSettings["openai_api_key"], "")
	project := firstTruthy(llmSettings["organization"], llmSettings["openai_organization"], llmSettings["project_id"], "")
	secret := firstTruthy(llmSettings["x_secret"], "secret")
	return ArtifactSettings{
		BaseURL:   llmSuffix.ReplaceAllString(fmt.Sprint(apiBase), ""),
		APIKey:    fmt.Sprint(apiKey),
		ProjectID: fmt.Sprint(project),
		APIPath:   DefaultAPIPath,
		XSecret:   fmt.Sprint(secret),
	}
}

func firstTruthy(values ...any) any {
	for _, v := range values {
		if Truthy(v) {
			return v
		}
	}
	if len(values) == 0 {
		return nil
	}
	return values[len(values)-1]
}

// ArtifactClientFrom is the default factory: nil without BOTH halves of the
// transport (a direct SPI call, a host test), else an HTTP client over the
// callback hop's TLS (its CA and, toward the platform listener, its client
// certificate).
func ArtifactClientFrom(callback artifacts.CallbackTLS) ArtifactClientFactory {
	return func(llmSettings map[string]any) (ArtifactClient, error) {
		if !Truthy(llmSettings["api_base"]) && !Truthy(llmSettings["openai_api_base"]) {
			return nil, nil
		}
		if !Truthy(llmSettings["api_key"]) && !Truthy(llmSettings["openai_api_key"]) {
			return nil, nil
		}
		return artifacts.NewHTTPClient(ExtractArtifactSettings(llmSettings), callback)
	}
}
