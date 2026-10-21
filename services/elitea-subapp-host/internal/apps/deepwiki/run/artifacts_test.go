package run_test

import (
	"testing"

	"github.com/EliteaAI/elitea-platform/services/elitea-subapp-host/internal/apps/deepwiki/run"
)

// The transport an invocation carries is its bearer and its project, read in
// every legacy spelling; api_base only names the model route since #1359 U5
// (the artifact service's address is the host's own), so it is not read. The
// client itself is internal/artifacts', tested there against a TLS
// ArtifactObjectService.
func TestTheArtifactSettingsAreTheBearerAndTheProject(t *testing.T) {
	for _, llmSettings := range []map[string]any{
		{"api_base": "https://elitea-platform.invalid/llm/v1", "api_key": "minted", "organization": "90200"},
		{"openai_api_base": "http://x/llm/v1", "openai_api_key": "minted", "project_id": 90200.0},
		{"api_key": "minted", "openai_organization": "90200", "project_id": "1"},
	} {
		if got := run.ExtractArtifactSettings(llmSettings); got.APIKey != "minted" || got.ProjectID != "90200" {
			t.Errorf("%v → %+v", llmSettings, got)
		}
	}
}
