package runtimecomposition

import (
	"context"
	"testing"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/mcpregistry"
	"github.com/stretchr/testify/require"
)

func TestCurrentAgentInternalBuildersMaterializeWithoutRegistryRows(t *testing.T) {
	store := &currentPrebuiltMCPStoreStub{err: mcpregistry.ErrPrebuiltNotFound}
	issuer := &currentActorTokenIssuerStub{token: "ephemeral-executing-actor"}
	resolver, err := newCurrentAgentRuntimePrebuiltMCP(store, "https://main.example.test", issuer)
	require.NoError(t, err)
	for _, entry := range mcpregistry.InternalBuilders() {
		kind := mcpregistry.InternalBuilderTypePrefix + entry.Key
		resolved, found, err := resolver.ResolveCurrentAgentPrebuiltMCP(context.Background(), 7, 42, kind, map[string]any{
			"server_name": kind, "url": "https://attacker.invalid", "project_id": "999", "personal_token": "forged", "headers": map[string]any{"Authorization": "forged"},
		}, identityPrebuiltMaterializer)
		require.NoError(t, err)
		require.True(t, found)
		require.Equal(t, "https://main.example.test/app/7/mcp/"+entry.Category, resolved["url"])
		require.Equal(t, "Bearer ephemeral-executing-actor", resolved["headers"].(map[string]any)["Authorization"])
		require.Equal(t, int64(42), issuer.userID)
	}
	require.Equal(t, len(mcpregistry.InternalBuilders()), issuer.calls)
	_, found, err := resolver.ResolveCurrentAgentPrebuiltMCP(context.Background(), 7, 42, "mcp_elitea_internal_unknown", map[string]any{}, identityPrebuiltMaterializer)
	require.NoError(t, err)
	require.False(t, found)
}

func TestCurrentAgentInternalBuildersRequireClaimActorAuthority(t *testing.T) {
	resolver, err := newCurrentAgentPrebuiltMCP(&currentPrebuiltMCPStoreStub{err: mcpregistry.ErrPrebuiltNotFound})
	require.NoError(t, err)
	_, found, err := resolver.ResolveCurrentAgentPrebuiltMCP(context.Background(), 7, 42, "mcp_elitea_internal_skills", map[string]any{}, identityPrebuiltMaterializer)
	require.Error(t, err)
	require.False(t, found)
}
