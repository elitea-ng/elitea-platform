package configurations

import (
	"context"
	"testing"
)

// A delegated Microsoft credential reaches the worker as an access token
// only (ADR-0057 decision 7): neither a claim nor a frozen input carries the
// client secret or a stored refresh token, and a SharePoint credential, whose
// worker config still reads them, is frozen as before.
func TestCurrentToolkitSettingsResolverWithholdsMicrosoftSecretsFromTheWorker(t *testing.T) {
	for _, mode := range []CurrentToolkitSettingsMode{CurrentToolkitSettingsClaimMode, CurrentToolkitSettingsReferenceMode} {
		for _, configType := range []string{"outlook", "teams", "ms_graph", "dataverse", "sharepoint"} {
			schemas := &currentToolkitSchemaCatalogStub{schemas: map[string]CurrentToolkitSchema{
				"root": {Properties: map[string]any{
					"credential": map[string]any{"configuration_types": []any{configType}},
				}},
			}}
			configurations := &currentConfigurationExpanderStub{fn: func(CurrentExpansionRequest) (map[string]any, error) {
				return map[string]any{
					"configuration_type": configType, "client_id": "client",
					"client_secret": "{{secret.CS}}", "refresh_token": "{{secret.RT}}",
				}, nil
			}}
			resolver := newCurrentToolkitSettingsResolverForTest(
				t, schemas, &currentNestedToolkitReaderStub{}, configurations,
				&currentModelVisibilityStub{}, &currentToolkitUnsecreterStub{},
			)
			result, err := resolver.Resolve(context.Background(), CurrentToolkitSettingsRequest{
				ToolkitType: "root",
				Settings:    map[string]any{"credential": map[string]any{"elitea_title": "t", "private": false}},
				ProjectID:   7, UserID: 42, Mode: mode,
			})
			if err != nil {
				t.Fatalf("%s %s: %v", mode, configType, err)
			}
			credential := result["credential"].(map[string]any)
			_, secret := credential["client_secret"]
			_, refresh := credential["refresh_token"]
			if credential["client_id"] != "client" {
				t.Fatalf("%s %s: credential = %#v", mode, configType, credential)
			}
			if wantKept := configType == "sharepoint"; secret != wantKept || refresh != wantKept {
				t.Fatalf("%s %s: secret kept=%v refresh kept=%v", mode, configType, secret, refresh)
			}
		}
	}
}
