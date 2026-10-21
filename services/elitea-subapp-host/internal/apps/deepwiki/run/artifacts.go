package run

import (
	"fmt"
	"regexp"

	"github.com/EliteaAI/elitea-platform/services/elitea-subapp-host/internal/artifacts"
)

// The artifact upload path. The transport — the platform's artifact route,
// the bearer, the multipart shape, the refusal texts — moved to
// internal/artifacts in ADR-0023 stage H4c, because none of it is
// DeepWiki's; the names below stay so the composition here reads as it did.
//
// What did NOT move is the derivation: llm_settings is the legacy plugin's
// dict, and reading a base URL, a key and a project out of it (including
// "no transport at all") is that plugin's contract, not the platform's.

// ArtifactClient puts one object into a bucket.
type ArtifactClient = artifacts.Client

// ArtifactClientFactory builds the client for one invocation.
type ArtifactClientFactory = artifacts.Factory

// ArtifactSettings is the transport derived from one request's llm_settings.
type ArtifactSettings = artifacts.Settings

// HTTPArtifactClient uploads through the platform's artifact API.
type HTTPArtifactClient = artifacts.HTTPClient

// DefaultAPIPath is the platform API prefix the artifact routes live under.
const DefaultAPIPath = artifacts.DefaultAPIPath

// NewHTTPArtifactClient builds the upload transport over the callback TLS.
func NewHTTPArtifactClient(settings ArtifactSettings, callback artifacts.CallbackTLS) (*HTTPArtifactClient, error) {
	return artifacts.NewHTTPClient(settings, callback)
}

var llmSuffix = regexp.MustCompile(`/llm(/api)?(/v\d+)?/?$`)

// ExtractArtifactSettings is the legacy derivation, verbatim: api_base or
// openai_api_base with `/llm[/api][/vN]` stripped; api_key or
// openai_api_key; the project from organization, openai_organization or
// project_id, in that order.
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

// ArtifactClientFrom is the default factory: nil without BOTH halves of the
// transport (a direct SPI call, the P0 fixtures), else an HTTP client.
// callback is the hop's TLS: its CA (the shell read the same
// ELITEA_DEEPWIKI_TLS_CA_FILE for its `verify=`) and, toward the platform
// listener, the client certificate it presents.
func ArtifactClientFrom(callback artifacts.CallbackTLS) ArtifactClientFactory {
	return func(llmSettings map[string]any) (ArtifactClient, error) {
		if !Truthy(llmSettings["api_base"]) && !Truthy(llmSettings["openai_api_base"]) {
			return nil, nil
		}
		if !Truthy(llmSettings["api_key"]) && !Truthy(llmSettings["openai_api_key"]) {
			return nil, nil
		}
		return NewHTTPArtifactClient(ExtractArtifactSettings(llmSettings), callback)
	}
}
