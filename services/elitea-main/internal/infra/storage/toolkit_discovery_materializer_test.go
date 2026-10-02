package storage

import (
	"context"
	"encoding/json"
	domain "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/execution"
	"testing"
)

func TestDiscoveryMaterializerRedeemsOnlyBoundSettings(t *testing.T) {
	vault := &currentMaterializationUnsecreterStub{values: map[int32]map[string]string{7: {"TOKEN": "redeemed"}}}
	materializer := newCurrentConfigurationsMaterializerForTest(t, vault)
	authorization := ContentAuthorization{ResourceProjectID: "7", ToolkitType: "github", ActorID: "42", CapabilityID: domain.ToolkitAvailableToolsCapability, SemanticRole: domain.ToolkitAvailableToolsSettingsRole}
	source := []byte(`{"token":"{{secret.TOKEN}}","large":9007199254740993}`)
	result, err := materializer.MaterializeContent(context.Background(), authorization, source, 256*1024)
	if err != nil {
		t.Fatal(err)
	}
	object, err := decodeCurrentMaterializationObject(result)
	if err != nil || object["token"] != "redeemed" || object["large"] != json.Number("9007199254740993") {
		t.Fatal("discovery settings changed")
	}
	authorization.SemanticRole = "toolkit.available_tools.runtime_context"
	result, err = materializer.MaterializeContent(context.Background(), authorization, source, 256*1024)
	if err != nil || string(result) != string(source) || len(vault.projects) != 1 {
		t.Fatal("policy content was materialized")
	}
	authorization.SemanticRole = "toolkit.call_tool.settings"
	if _, err = materializer.MaterializeContent(context.Background(), authorization, source, 256*1024); err == nil {
		t.Fatal("foreign role materialized")
	}
}
