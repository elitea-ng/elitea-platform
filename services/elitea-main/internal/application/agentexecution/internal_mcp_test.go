package agentexecution

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/guardrails"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/mcpregistry"
	"github.com/stretchr/testify/require"
)

func TestCurrentInternalMCPSelectionBecomesExecutableReferences(t *testing.T) {
	for _, tc := range []struct {
		name, version, conversation string
		want                        int
	}{
		{"off", `{}`, `[]`, 0},
		{"application flag", `{"meta":{"internal_tools":["internal_mcp","ask_user"]}}`, `[]`, 9},
		{"conversation flag", `{}`, `["internal_mcp"]`, 9},
		{"skill builder", `{}`, `["skill_builder"]`, 1},
		{"context builder", `{"meta":{"internal_tools":["project_context_builder"]}}`, `[]`, 1},
		{"deduplicated flags", `{"meta":{"internal_tools":["internal_mcp","skill_builder"]}}`, `["internal_mcp","project_context_builder"]`, 9},
	} {
		t.Run(tc.name, func(t *testing.T) {
			freezer, err := NewCurrentApplicationToolSnapshotService(&currentAgentSettingsResolverStub{}, &currentAgentNameResolverStub{}, currentAgentModelCatalogForTest(true), &currentAgentGuardrailStub{}, 1)
			require.NoError(t, err)
			version, err := decodeCurrentApplicationVersion([]byte(tc.version))
			require.NoError(t, err)
			version["llm_settings"] = map[string]any{"model_name": "model"}
			version["tools"] = []any{}
			raw, err := json.Marshal(version)
			require.NoError(t, err)
			frozen, err := freezer.FreezeCurrentApplicationVersion(t.Context(), CurrentApplicationVersionFreezeRequest{ProjectID: 7, ActorUserID: 11, VersionDetails: raw, InternalTools: json.RawMessage(tc.conversation)})
			require.NoError(t, err)
			decoded, err := decodeCurrentApplicationVersion(frozen)
			require.NoError(t, err)
			tools := decoded["tools"].([]any)
			require.Len(t, tools, tc.want)
			seen := map[string]bool{}
			for _, raw := range tools {
				tool := raw.(map[string]any)
				kind := tool["type"].(string)
				entry, ok := mcpregistry.InternalBuilderForType(kind)
				require.True(t, ok)
				require.False(t, seen[kind])
				seen[kind] = true
				require.Nil(t, tool["id"])
				require.Equal(t, entry.Name, tool["toolkit_name"])
				require.Equal(t, map[string]any{"server_name": kind}, tool["settings"])
			}
			require.NotContains(t, string(frozen), "Authorization")
			require.NotContains(t, string(frozen), "personal_token")
			require.NotContains(t, string(frozen), "https://")
			if tc.want == 9 {
				require.True(t, seen["mcp_elitea_internal_skills"])
				require.True(t, seen["mcp_elitea_internal_project_context"])
			}
		})
	}
}

func TestCurrentAdhocInternalMCPSelectionSurvivesSnapshot(t *testing.T) {
	target := CurrentAdhocTarget{LLMSettings: json.RawMessage(`{"model_name":"model"}`), Tools: json.RawMessage(`[]`), ConversationMeta: json.RawMessage(`{"internal_tools":["internal_mcp"]}`)}
	snapshot, err := currentAdhocSnapshot(json.RawMessage(`{}`), target)
	require.NoError(t, err)
	version, err := decodeCurrentApplicationVersion(snapshot)
	require.NoError(t, err)
	tools, err := freezeCurrentInternalMCP(version, nil, []any{}, guardrails.Policy{})
	require.NoError(t, err)
	require.Len(t, tools, 9)
	runtimeFlags, err := currentAdhocRuntimeInternalTools(target.ConversationMeta)
	require.NoError(t, err)
	require.Equal(t, `[]`, string(runtimeFlags))
}

func TestCurrentInternalMCPReferencesDeduplicateAfterFreeze(t *testing.T) {
	version := map[string]any{}
	first, err := freezeCurrentInternalMCP(version, json.RawMessage(`["internal_mcp"]`), nil, guardrails.Policy{})
	require.NoError(t, err)
	second, err := freezeCurrentInternalMCP(version, json.RawMessage(`["internal_mcp"]`), first, guardrails.Policy{})
	require.NoError(t, err)
	require.Len(t, second, len(first))
	require.True(t, strings.HasPrefix(second[0].(map[string]any)["type"].(string), "mcp_"))
}
