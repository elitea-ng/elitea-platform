package toolkits_test

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/toolkits"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/runtimecomposition"
)

// The tests below assert on the RESOLVED definition, never on the presence of a
// "$defs" key. A served "$defs" that no property points at, or a "$ref" whose
// target is missing, both leave the web client exactly where #330 found it: no
// property of kind `configuration`, so no credential picker. Every assertion
// therefore runs the reference to its target and reads the section off it.

// resolveConfigurationSection reproduces the web client's resolution step for
// one property, so a payload that passes here is one the browser can use.
//
// It mirrors findConfigDefKey in
// apps/elitea-web/src/features/toolkits/lib/helpers/toolkitSchema.helpers.ts:
// strip the "#/$defs/" prefix from the property's "$ref", or from the first
// "anyOf" branch whose stripped key is present in "$defs", then read
// metadata.section off the definition that key names. A key that is absent from
// "$defs" resolves to nothing there, and to ok=false here.
func resolveConfigurationSection(
	t *testing.T,
	typeSchema map[string]any,
	property string,
) (string, bool) {
	t.Helper()

	definitions, _ := typeSchema["$defs"].(map[string]any)
	properties, ok := typeSchema["properties"].(map[string]any)
	if !ok {
		t.Fatalf("type schema has no properties object: %#v", typeSchema)
	}
	schema, ok := properties[property].(map[string]any)
	if !ok {
		t.Fatalf("no %q property; have %v", property, keysOf(properties))
	}

	key, ok := configurationDefinitionKey(schema, definitions)
	if !ok {
		return "", false
	}
	definition, ok := definitions[key].(map[string]any)
	if !ok {
		return "", false
	}
	metadata, ok := definition["metadata"].(map[string]any)
	if !ok {
		return "", false
	}
	section, ok := metadata["section"].(string)
	return section, ok
}

func configurationDefinitionKey(
	schema map[string]any,
	definitions map[string]any,
) (string, bool) {
	if reference, ok := schema["$ref"].(string); ok {
		key := strings.TrimPrefix(reference, "#/$defs/")
		_, present := definitions[key]
		return key, present
	}
	branches, ok := schema["anyOf"].([]any)
	if !ok {
		return "", false
	}
	for _, raw := range branches {
		branch, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		reference, ok := branch["$ref"].(string)
		if !ok {
			continue
		}
		key := strings.TrimPrefix(reference, "#/$defs/")
		if _, present := definitions[key]; present {
			return key, true
		}
	}
	return "", false
}

func hasDefinitionReference(schema map[string]any) bool {
	if _, ok := schema["$ref"].(string); ok {
		return true
	}
	branches, ok := schema["anyOf"].([]any)
	if !ok {
		return false
	}
	for _, raw := range branches {
		branch, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		if _, ok := branch["$ref"].(string); ok {
			return true
		}
	}
	return false
}

// This is the defect #330 records, stated as a test: the credential picker is
// gated on a property whose $defs entry carries metadata.section "credentials".
// Before this change github served no "$defs" at all, so this could not pass.
func TestToolkitTypeCatalogueServesResolvableCredentialDefinitions(t *testing.T) {
	t.Parallel()

	body := getToolkitTypeCatalogue(t, pinnedCatalogueOptions(t)...)

	for _, expected := range []struct {
		toolkitType string
		property    string
	}{
		{"github", "github_configuration"},
		{"jira", "jira_configuration"},
		{"openapi", "openapi_configuration"},
	} {
		typeSchema, ok := body[expected.toolkitType].(map[string]any)
		if !ok {
			t.Fatalf("catalogue has no %q type; have %v", expected.toolkitType, keysOf(body))
		}
		section, ok := resolveConfigurationSection(t, typeSchema, expected.property)
		if !ok {
			t.Errorf("%s.%s does not resolve to a definition",
				expected.toolkitType, expected.property)
			continue
		}
		if section != "credentials" {
			t.Errorf("%s.%s section=%q, want %q",
				expected.toolkitType, expected.property, section, "credentials")
		}
	}
}

// The retired pgvector reference is served by no type: artifact, which had
// one and no credential at all, now carries no configuration reference and
// no vector-storage definition.
func TestToolkitTypeCatalogueServesNoVectorStorageDefinition(t *testing.T) {
	t.Parallel()

	body := getToolkitTypeCatalogue(t, pinnedCatalogueOptions(t)...)
	artifact, ok := body["artifact"].(map[string]any)
	if !ok {
		t.Fatalf("catalogue has no artifact type; have %v", keysOf(body))
	}
	properties := artifact["properties"].(map[string]any)
	if _, present := properties["pgvector_configuration"]; present {
		t.Error("artifact still serves the retired pgvector_configuration")
	}
	for property := range properties {
		if section, ok := resolveConfigurationSection(t, artifact, property); ok && section == "vectorstorage" {
			t.Errorf("artifact.%s still resolves to a vectorstorage definition", property)
		}
	}
}

// resolveCredentialsData in
// apps/elitea-web/src/features/toolkits/indexes/ui/IndexDetails/IndexActions.tsx
// scans the converted properties for the first one whose section contains
// "credentials", and the index schedule modal renders its credential select
// only when that scan returns a property. This reproduces the scan over the
// served payload: an empty result is the null that keeps the schedule select
// off the screen.
func TestToolkitTypeCatalogueLetsTheScheduleCredentialScanFindAProperty(t *testing.T) {
	t.Parallel()

	body := getToolkitTypeCatalogue(t, pinnedCatalogueOptions(t)...)
	github := body["github"].(map[string]any)
	properties := github["properties"].(map[string]any)

	found := make([]string, 0, 1)
	for property := range properties {
		section, ok := resolveConfigurationSection(t, github, property)
		if ok && strings.Contains(section, "credentials") {
			found = append(found, property)
		}
	}
	sort.Strings(found)

	if len(found) != 1 || found[0] != "github_configuration" {
		t.Errorf("credential scan over github found %v, want [github_configuration]", found)
	}
}

// The modal passes credentialsData.configuration_types straight to the picker
// as the list of configuration types to offer. An empty list offers nothing, so
// the property carries the types even though the definition names one too.
func TestToolkitTypeCatalogueServesTheAcceptedConfigurationTypes(t *testing.T) {
	t.Parallel()

	body := getToolkitTypeCatalogue(t, pinnedCatalogueOptions(t)...)
	github := body["github"].(map[string]any)
	properties := github["properties"].(map[string]any)
	configuration := properties["github_configuration"].(map[string]any)

	types, ok := configuration["configuration_types"].([]any)
	if !ok {
		t.Fatalf("github_configuration carries no configuration_types: %#v", configuration)
	}
	if len(types) != 1 || types[0] != "github" {
		t.Errorf("github_configuration configuration_types=%#v, want [github]", types)
	}
}

// A "$ref" whose target is missing is worse than no reference: the client drops
// the property silently. This walks every served type and runs every reference
// it finds to its target.
func TestToolkitTypeCatalogueServesNoDanglingSchemaReference(t *testing.T) {
	t.Parallel()

	body := getToolkitTypeCatalogue(t, pinnedCatalogueOptions(t)...)
	referenced := 0
	for toolkitType, raw := range body {
		typeSchema, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		definitions, _ := typeSchema["$defs"].(map[string]any)
		properties, ok := typeSchema["properties"].(map[string]any)
		if !ok {
			continue
		}
		for property, rawSchema := range properties {
			schema, ok := rawSchema.(map[string]any)
			if !ok {
				continue
			}
			if !hasDefinitionReference(schema) {
				continue
			}
			if _, resolved := configurationDefinitionKey(schema, definitions); !resolved {
				t.Errorf("%s.%s has a reference with no $defs target; $defs has %v",
					toolkitType, property, keysOf(definitions))
				continue
			}
			referenced++
		}
	}
	if referenced == 0 {
		t.Fatal("no served property references a definition at all")
	}
}

// database, custom, datasource and application are elitea_core-native, not SDK
// toolkits. They have no configuration reference, and inventing a "$defs" for
// them would put a credential picker on a form that has no credential.
func TestToolkitTypeCatalogueServesNoDefinitionsForTypesTheSDKDoesNotDefine(t *testing.T) {
	t.Parallel()

	body := getToolkitTypeCatalogue(t, pinnedCatalogueOptions(t)...)
	for _, toolkitType := range []string{"database", "custom", "datasource", "application"} {
		typeSchema, ok := body[toolkitType].(map[string]any)
		if !ok {
			t.Fatalf("catalogue has no %q type; have %v", toolkitType, keysOf(body))
		}
		if definitions, present := typeSchema["$defs"]; present {
			t.Errorf("%s serves $defs=%#v, want none", toolkitType, definitions)
		}
	}
}

// The settings and argument schemas are separate resources on the same
// response. Adding the definitions must not cost either of them.
func TestToolkitTypeCatalogueKeepsSettingsAndArgumentSchemasBesideTheDefinitions(t *testing.T) {
	t.Parallel()

	body := getToolkitTypeCatalogue(t, pinnedCatalogueOptions(t)...)
	github := body["github"].(map[string]any)
	properties := github["properties"].(map[string]any)

	for _, setting := range []string{"repository", "access_token"} {
		if _, ok := properties[setting]; !ok {
			t.Errorf("github settings lost %q; have %v", setting, keysOf(properties))
		}
	}
	if _, ok := argumentSchema(t, body, "github", "get_issue")["type"]; !ok {
		t.Error("github lost its get_issue argument schema")
	}
}

// A definition source that cannot answer is a broken binary, exactly as the
// argument-schema source is. Answering 200 with a definition-less catalogue
// would put the unreachable picker back, silently.
func TestToolkitTypeCatalogueFailsLoudlyWhenTheDefinitionSourceErrors(t *testing.T) {
	t.Parallel()

	handler := toolkits.NewHandlerWithRepo(
		&mockRepo{},
		toolkits.WithSettingsDefinitions(failingSettingsDefinitions{}),
	)
	response := httptest.NewRecorder()
	handler.ListTypeSchemas(response, httptest.NewRequest(http.MethodGet, "/toolkits/prompt_lib/1", nil))

	if response.Code != http.StatusInternalServerError {
		t.Fatalf("status=%d, want 500: %s", response.Code, response.Body.String())
	}
}

// An unassigned source must not panic the endpoint. It serves the pre-#330
// payload instead, which is a degraded picker, not a broken response.
func TestToolkitTypeCatalogueServesWithoutADefinitionSource(t *testing.T) {
	t.Parallel()

	body := getToolkitTypeCatalogue(t)
	github, ok := body["github"].(map[string]any)
	if !ok {
		t.Fatalf("catalogue has no github type; have %v", keysOf(body))
	}
	if _, present := github["$defs"]; present {
		t.Error("github serves $defs with no definition source assigned")
	}
}

func pinnedCatalogueOptions(t *testing.T) []toolkits.Option {
	t.Helper()
	return []toolkits.Option{
		toolkits.WithArgumentSchemas(pinnedSnapshot(t)),
		toolkits.WithSettingsDefinitions(pinnedSettingsDefinitions(t)),
	}
}

func pinnedSettingsDefinitions(t *testing.T) *runtimecomposition.CurrentToolkitSettingsDefinitionCatalog {
	t.Helper()
	configurations, err := runtimecomposition.LoadPinnedCurrentSDKConfigurationCatalog()
	if err != nil {
		t.Fatalf("load pinned SDK configuration catalog: %v", err)
	}
	definitions, err := runtimecomposition.NewCurrentToolkitSettingsDefinitionCatalog(
		pinnedSnapshot(t),
		configurations,
	)
	if err != nil {
		t.Fatalf("compose toolkit settings definitions: %v", err)
	}
	return definitions
}

type failingSettingsDefinitions struct{}

func (failingSettingsDefinitions) ToolkitSettingsDefinitions(
	string,
) (map[string]any, map[string]any, bool, error) {
	return nil, nil, false, errors.New("configuration catalog unavailable")
}
