package run

import (
	"fmt"

	"github.com/EliteaAI/elitea-platform/services/elitea-subapp-host/internal/artifacts"
)

// The artifact upload path. The transport — the platform's artifact service,
// the bearer, the refusal texts — moved to internal/artifacts in ADR-0023
// stage H4c, because none of it is DeepWiki's; the names below stay so the
// composition here reads as it did.
//
// What did NOT move is the derivation: llm_settings is the legacy plugin's
// dict, and reading a key and a project out of it (including "no transport
// at all") is that plugin's contract, not the platform's.

// ArtifactClient puts one object into a bucket.
type ArtifactClient = artifacts.Client

// ArtifactClientFactory builds the client for one invocation.
type ArtifactClientFactory = artifacts.Factory

// ArtifactSettings is the transport derived from one request's llm_settings.
type ArtifactSettings = artifacts.Settings

// ExtractArtifactSettings is the legacy derivation: api_key or
// openai_api_key; the project from organization, openai_organization or
// project_id, in that order. api_base is not read: since #1359 U5 it only
// names the model route, and the artifact service's address is the host's
// own ELITEA_DEEPWIKI_ARTIFACT_SERVICE_URL.
func ExtractArtifactSettings(llmSettings map[string]any) ArtifactSettings {
	apiKey := firstTruthy(llmSettings["api_key"], llmSettings["openai_api_key"], "")
	project := firstTruthy(llmSettings["organization"], llmSettings["openai_organization"], llmSettings["project_id"], "")
	return ArtifactSettings{APIKey: fmt.Sprint(apiKey), ProjectID: fmt.Sprint(project)}
}

// hasTransport is the legacy "both halves" rule: a request carries a
// transport only with an api_base AND an api_key (a direct SPI call and the
// P0 fixtures carry neither).
func hasTransport(llmSettings map[string]any) bool {
	return (Truthy(llmSettings["api_base"]) || Truthy(llmSettings["openai_api_base"])) &&
		(Truthy(llmSettings["api_key"]) || Truthy(llmSettings["openai_api_key"]))
}

// ArtifactClientFrom is the default factory: nil without both halves of the
// transport, else a client of elitea.runtime.v1.ArtifactObjectService over
// the callback hop (the platform listener, the runtime CA, the callback
// client certificate), bound to the request's bearer and project.
func ArtifactClientFrom(callback artifacts.Callback) ArtifactClientFactory {
	return artifacts.FactoryFor(callback, ExtractArtifactSettings, hasTransport)
}
