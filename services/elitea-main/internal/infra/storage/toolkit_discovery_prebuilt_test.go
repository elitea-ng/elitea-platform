package storage

import (
	"context"
	"testing"

	executiondomain "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/execution"
	"github.com/stretchr/testify/require"
)

func TestDiscoveryUsesAdmittedPrebuiltTypeAndTrustedEndpoint(t *testing.T) {
	vault := &currentMaterializationUnsecreterStub{values: map[int32]map[string]string{7: {"CALLER_SECRET": "must-not-redeem"}}}
	resolver := &currentAgentPrebuiltMCPResolverStub{found: true, keep: []string{"server_name", "selected_tools"}, result: map[string]any{
		"server_name": "records", "url": "https://trusted.example.test/mcp", "selected_tools": []any{"query"},
	}}
	materializer, err := NewCurrentAgentConfigurationsMaterializer(vault, resolver)
	require.NoError(t, err)
	result, err := materializer.MaterializeContent(context.Background(), ContentAuthorization{
		ResourceProjectID: "7", ActorID: "41", ToolkitType: "mcp_records", CapabilityID: executiondomain.ToolkitAvailableToolsCapability, SemanticRole: executiondomain.ToolkitAvailableToolsSettingsRole,
	}, []byte(`{"server_name":"records","url":"https://caller.example.test/mcp","headers":{"Authorization":"{{secret.CALLER_SECRET}}"},"selected_tools":["query"]}`), 256*1024)
	require.NoError(t, err)
	require.Equal(t, "mcp_records", resolver.toolType)
	require.Equal(t, 1, resolver.calls)
	require.Empty(t, vault.projects)
	require.Contains(t, string(result), "https://trusted.example.test/mcp")
	require.NotContains(t, string(result), "caller.example.test")
}

func TestDiscoveryPreservesFrozenConfigurationOwner(t *testing.T) {
	vault := &currentMaterializationUnsecreterStub{values: map[int32]map[string]string{7: {"TOKEN": "wrong-project"}, 1: {"TOKEN": "owner-token"}}}
	materializer := newCurrentConfigurationsMaterializerForTest(t, vault)
	result, err := materializer.MaterializeContent(context.Background(), ContentAuthorization{
		ResourceProjectID: "7", ActorID: "41", ToolkitType: "openapi", CapabilityID: executiondomain.ToolkitAvailableToolsCapability, SemanticRole: executiondomain.ToolkitAvailableToolsSettingsRole,
	}, []byte(`{"openapi_configuration":{"elitea_title":"api","private":false,"token":"{{secret.TOKEN}}","configuration_uuid":"config","configuration_project_id":1,"configuration_type":"openapi","__elitea_frozen_configuration_v1":true}}`), 256*1024)
	require.NoError(t, err)
	require.Contains(t, string(result), "owner-token")
	require.NotContains(t, string(result), "wrong-project")
	require.Equal(t, []int32{1, 7}, vault.projects)
}
