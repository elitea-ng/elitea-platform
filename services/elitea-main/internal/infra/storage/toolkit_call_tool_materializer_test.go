package storage

import (
	"context"
	"encoding/json"
	executiondomain "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/execution"
	"testing"
)

func TestToolkitCallToolMaterializesSettingsAndPreservesOtherInputs(t *testing.T) {
	vault := &currentMaterializationUnsecreterStub{values: map[int32]map[string]string{7: {"TOKEN": "redeemed"}}}
	materializer := newCurrentConfigurationsMaterializerForTest(t, vault)
	authorization := ContentAuthorization{ResourceProjectID: "7", ActorID: "42", CapabilityID: executiondomain.ToolkitCallToolCapability, SemanticRole: executiondomain.ToolkitCallToolSettingsRole}
	source := []byte(`{"id":19,"type":"github","toolkit_name":"saved","settings":{"token":"{{secret.TOKEN}}","large":9007199254740993}}`)
	result, err := materializer.MaterializeContent(context.Background(), authorization, source, 256*1024)
	if err != nil {
		t.Fatal(err)
	}
	object, err := decodeCurrentMaterializationObject(result)
	if err != nil {
		t.Fatal(err)
	}
	settings := object["settings"].(map[string]any)
	if settings["token"] != "redeemed" || settings["large"] != json.Number("9007199254740993") {
		t.Fatal("settings redemption changed the contract")
	}
	for _, role := range []string{executiondomain.ToolkitCallToolArgumentsRole, executiondomain.ToolkitCallToolRuntimeContextRole} {
		authorization.SemanticRole = role
		raw := []byte(` {"text":"{{secret.TOKEN}}","large":9007199254740993} `)
		result, err = materializer.MaterializeContent(context.Background(), authorization, raw, 256*1024)
		if err != nil || string(result) != string(raw) {
			t.Fatalf("input %s changed: %v", role, err)
		}
	}
	if len(vault.projects) != 1 {
		t.Fatal("argument or policy content reached secret redemption")
	}
	authorization.SemanticRole = "unknown"
	if _, err = materializer.MaterializeContent(context.Background(), authorization, source, 256*1024); err == nil {
		t.Fatal("unbound role accepted")
	}
}
