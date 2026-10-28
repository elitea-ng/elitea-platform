package configurations

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
)

// jiraToolkitWithReference resolves one jira toolkit whose stored reference
// is ref, through the REAL legacy expansion (CurrentExpansionService over a
// finder stub), and answers the frozen settings' JSON and the finder's
// lookups.
func jiraToolkitWithReference(t *testing.T, ref map[string]any, mode CurrentToolkitSettingsMode, refuse bool) ([]byte, []currentExpansionLookup, error) {
	t.Helper()
	finder := &currentExpansionFinderStub{rows: map[currentExpansionLookup]CurrentExpansionConfiguration{
		{projectID: 7, title: "jira-prod"}: {
			UUID: "33333333-3333-3333-3333-333333333333", ProjectID: 7, Type: "jira",
			Data: map[string]any{"base_url": "https://jira.example.test", "api_key": "{{secret.JIRA}}"},
		},
	}}
	expander := newCurrentExpansionTestService(t, finder, &currentExpansionUnsecreterStub{})
	schemas := &currentToolkitSchemaCatalogStub{schemas: map[string]CurrentToolkitSchema{
		"jira": {Properties: map[string]any{
			"jira_configuration": map[string]any{"configuration_types": []any{"jira"}},
			"limit":              map[string]any{},
		}},
	}}
	deps := currentToolkitSettingsTestDependencies()
	resolver := newCurrentToolkitSettingsResolverForTest(t, schemas, deps.toolkits, expander, deps.models, deps.unsecreter)
	settings, err := resolver.Resolve(context.Background(), CurrentToolkitSettingsRequest{
		ToolkitType: "jira", ProjectID: 7, UserID: 42, Mode: mode, RefuseCredentialReference: refuse,
		Settings: map[string]any{"jira_configuration": ref, "limit": json.Number("5")},
	})
	if err != nil {
		return nil, finder.calls, err
	}
	encoded, err := json.Marshal(settings)
	if err != nil {
		t.Fatal(err)
	}
	return encoded, finder.calls, nil
}

// Golden (#1119): a stored reference that also names a governed
// credential_reference_id resolves to byte-identical frozen settings, by the
// same title lookups, as the title-only reference. Legacy readers ignore
// the field, in both modes.
func TestLegacyToolkitSettingsIgnoreTheCredentialReferenceID(t *testing.T) {
	for _, mode := range []CurrentToolkitSettingsMode{CurrentToolkitSettingsReferenceMode, CurrentToolkitSettingsClaimMode} {
		t.Run(string(mode), func(t *testing.T) {
			legacy, legacyLookups, err := jiraToolkitWithReference(t, map[string]any{"elitea_title": "jira-prod", "private": false}, mode, false)
			if err != nil {
				t.Fatal(err)
			}
			withID, lookups, err := jiraToolkitWithReference(t, map[string]any{
				"elitea_title": "jira-prod", "private": false, CredentialReferenceIDField: "0192f0aa-0000-7000-8000-0000000000c1",
			}, mode, false)
			if err != nil {
				t.Fatal(err)
			}
			if string(withID) != string(legacy) {
				t.Fatalf("frozen settings differ:\n with id: %s\n legacy:  %s", withID, legacy)
			}
			if len(lookups) != len(legacyLookups) || lookups[0] != legacyLookups[0] {
				t.Fatalf("lookups differ: %v vs %v", lookups, legacyLookups)
			}
			const golden = `{"jira_configuration":{"__elitea_frozen_configuration_v1":true,"api_key":"{{secret.JIRA}}","base_url":"https://jira.example.test","configuration_project_id":7,"configuration_type":"jira","configuration_uuid":"33333333-3333-3333-3333-333333333333","elitea_title":"jira-prod","private":false},"limit":5}`
			if mode == CurrentToolkitSettingsReferenceMode && string(legacy) != golden {
				t.Fatalf("legacy frozen settings changed:\n got  %s\n want %s", legacy, golden)
			}
		})
	}
}

// The toolkit write path keeps refusing the field (only the governed admin
// API writes it), and a non-string id is refused everywhere, as any third
// key was before.
func TestToolkitWritePathRefusesTheCredentialReferenceID(t *testing.T) {
	for name, c := range map[string]struct {
		id     any
		refuse bool
	}{
		"write path": {"0192f0aa-0000-7000-8000-0000000000c1", true},
		"not a text": {json.Number("5"), false},
	} {
		t.Run(name, func(t *testing.T) {
			_, lookups, err := jiraToolkitWithReference(t, map[string]any{
				"elitea_title": "jira-prod", "private": false, CredentialReferenceIDField: c.id,
			}, CurrentToolkitSettingsReferenceMode, c.refuse)
			var validation *CurrentToolkitSettingsValidationError
			if !errors.As(err, &validation) || len(validation.Violations) != 1 ||
				validation.Violations[0].Code != CurrentToolkitConfigurationReferenceInvalidCode || len(lookups) != 0 {
				t.Fatalf("err = %v lookups %v", err, lookups)
			}
		})
	}
}
