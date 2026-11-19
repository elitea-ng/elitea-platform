package runtimecomposition

import (
	"errors"
	"fmt"
	"sync"

	configurationapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/configurations"
)

// pinnedAvailableCatalog is the available catalogue (pinned entries plus the
// native ones), loaded once: nativeConfigurationSection asks it per settings
// field and the catalogue is a 2 MiB parse.
var pinnedAvailableCatalog = sync.OnceValues(configurationapp.LoadPinnedCurrentAvailableCatalog)

// nativeConfigurationSection is the section of a credential type that the SDK
// configuration catalogue does not carry. ok=false means no catalogue knows the
// type, which stays a hard error for the caller.
func nativeConfigurationSection(configurationType string) (string, bool) {
	catalog, err := pinnedAvailableCatalog()
	if err != nil {
		return "", false
	}
	entry, found := catalog.EntryByType(configurationType)
	if !found {
		return "", false
	}
	return entry.Section, true
}

// configurationTypesAnnotation is the SDK annotation that marks a settings
// field as a reference to a saved configuration. The pinned snapshot carries it
// verbatim (its generator was deleted in #1312), and a type authored natively
// writes it in its own `properties` (docs/contributing/adding-types.md).
const configurationTypesAnnotation = "configuration_types"

var (
	ErrCurrentToolkitSettingsDefinitionsInvalid = errors.New(
		"current toolkit settings definitions are invalid",
	)
	// ErrCurrentToolkitConfigurationTypeUnknown reports a settings field that
	// names a configuration type the SDK configuration catalogue does not
	// declare. It fails the request closed: a "$ref" with no "$defs" entry
	// behind it is a dangling pointer, and the web client's schema parser
	// silently drops such a property instead of reporting it
	// (findConfigDefKey in
	// apps/elitea-web/src/features/toolkits/lib/helpers/toolkitSchema.helpers.ts
	// matches only keys that are present in "$defs").
	ErrCurrentToolkitConfigurationTypeUnknown = errors.New(
		"current toolkit settings reference an unknown configuration type",
	)
)

// CurrentToolkitSettingsDefinitionCatalog resolves the JSON Schema definitions
// that a built-in toolkit type's SETTINGS properties reference.
//
// It exists because neither pinned snapshot carries the answer on its own:
//
//   - current_toolkit_schema_snapshot.json says WHICH settings field is a
//     configuration reference, and which configuration types it accepts
//     (github -> github_configuration -> ["github"]).
//   - current_sdk_configuration_catalog_snapshot.json says which SECTION each
//     configuration type belongs to ("credentials" or "vectorstorage").
//
// The join of the two is the "$defs" block the web client needs. Both inputs
// are digest-pinned to the same admitted SDK revision, so the result is
// derived data, not a hand-authored guess.
//
// The definitions describe the REFERENCE, not the credential's own fields. The
// pinned catalogue holds only a digest of each configuration schema, so the
// inner fields (base_url, access_token, ...) are deliberately absent. The
// credential picker does not need them: it lists the project's saved
// configurations of the named types.
type CurrentToolkitSettingsDefinitionCatalog struct {
	toolkits       *CurrentToolkitSchemaSnapshot
	configurations *CurrentSDKConfigurationCatalog
}

func NewCurrentToolkitSettingsDefinitionCatalog(
	toolkits *CurrentToolkitSchemaSnapshot,
	configurations *CurrentSDKConfigurationCatalog,
) (*CurrentToolkitSettingsDefinitionCatalog, error) {
	if toolkits == nil || configurations == nil {
		return nil, errors.New(
			"current toolkit schema snapshot and SDK configuration catalog are required",
		)
	}
	return &CurrentToolkitSettingsDefinitionCatalog{
		toolkits:       toolkits,
		configurations: configurations,
	}, nil
}

// ToolkitSettingsDefinitions returns the "$defs" block for one built-in toolkit
// type, and the settings properties that reference it, keyed by property name.
//
// found=false means the type is not a built-in SDK toolkit. A built-in type
// that declares no configuration reference returns two empty maps and
// found=true: openapi's sibling types database, custom, datasource and
// application are elitea_core-native and legitimately have no configuration
// field at all.
//
// Both returned maps are freshly built on every call. The caller serves them
// straight into an HTTP response and must not share them with the package-level
// settings map.
func (c *CurrentToolkitSettingsDefinitionCatalog) ToolkitSettingsDefinitions(
	toolkitType string,
) (map[string]any, map[string]any, bool, error) {
	if c == nil || c.toolkits == nil || c.configurations == nil {
		return nil, nil, false, ErrCurrentToolkitSettingsDefinitionsInvalid
	}
	if !validCurrentToolkitSchemaIdentifier(toolkitType) {
		return nil, nil, false, ErrCurrentToolkitSettingsDefinitionsInvalid
	}
	entry, found := c.toolkits.entries[toolkitType]
	if !found {
		return nil, nil, false, nil
	}

	definitions := map[string]any{}
	properties := map[string]any{}
	for field, rawAnnotations := range entry.properties {
		annotations, ok := rawAnnotations.(map[string]any)
		if !ok {
			return nil, nil, false, ErrCurrentToolkitSettingsDefinitionsInvalid
		}
		configurationTypes, ok, err := currentToolkitConfigurationTypes(annotations)
		if err != nil {
			return nil, nil, false, err
		}
		if !ok {
			continue
		}
		property, err := c.buildConfigurationProperty(field, configurationTypes, definitions)
		if err != nil {
			return nil, nil, false, err
		}
		properties[field] = property
	}
	return definitions, properties, true, nil
}

// buildConfigurationProperty writes one "$defs" entry per accepted
// configuration type and returns the property that points at them.
//
// A single accepted type produces a direct "$ref". Several produce an "anyOf"
// of "$ref" branches — the same shape Pydantic emits for a union, and the
// second form the web client's findConfigDefKey resolves. The section is not
// merged across branches: each definition carries its own, and the client reads
// the section from the branch it matched.
func (c *CurrentToolkitSettingsDefinitionCatalog) buildConfigurationProperty(
	field string,
	configurationTypes []string,
	definitions map[string]any,
) (map[string]any, error) {
	references := make([]any, 0, len(configurationTypes))
	for _, configurationType := range configurationTypes {
		binding, found := c.configurations.Binding(configurationType)
		if !found {
			// A credential type the SDK catalogue does not carry (added after
			// the pin, or defined natively in Go) is known from the available
			// catalogue instead: it holds the section of every type.
			binding.ConfigurationType = configurationType
			binding.Section, found = nativeConfigurationSection(configurationType)
		}
		if !found || !validCurrentToolkitSchemaIdentifier(binding.Section) {
			return nil, fmt.Errorf(
				"%w: settings field %q names configuration type %q",
				ErrCurrentToolkitConfigurationTypeUnknown, field, configurationType,
			)
		}
		// "section" and "type" are both real SDK configuration metadata fields
		// (model_config.json_schema_extra.metadata in
		// elitea_sdk/configurations/*.py). No label is invented here: the
		// pinned catalogue carries none.
		definitions[configurationType] = map[string]any{
			"type": "object",
			"metadata": map[string]any{
				"section": binding.Section,
				"type":    binding.ConfigurationType,
			},
		}
		references = append(references, map[string]any{
			"$ref": "#/$defs/" + configurationType,
		})
	}
	if len(references) == 0 {
		return nil, ErrCurrentToolkitSettingsDefinitionsInvalid
	}

	// configuration_types is repeated on the property because the schedule
	// modal reads it from there, not from the definition
	// (IndexScheduleModal.tsx passes credentialsData.configuration_types
	// straight to the credential picker as configurationTypes).
	accepted := make([]any, 0, len(configurationTypes))
	for _, configurationType := range configurationTypes {
		accepted = append(accepted, configurationType)
	}
	property := map[string]any{configurationTypesAnnotation: accepted}
	if len(references) == 1 {
		reference, ok := references[0].(map[string]any)
		if !ok {
			return nil, ErrCurrentToolkitSettingsDefinitionsInvalid
		}
		property["$ref"] = reference["$ref"]
		return property, nil
	}
	property["anyOf"] = references
	return property, nil
}

// currentToolkitConfigurationTypes reads the configuration_types annotation.
// ok=false means the field is not a configuration reference — the common case,
// and the one that covers configuration_model ("embedding"), which names a
// model family rather than a saved configuration and has no catalogue entry.
func currentToolkitConfigurationTypes(annotations map[string]any) ([]string, bool, error) {
	raw, present := annotations[configurationTypesAnnotation]
	if !present {
		return nil, false, nil
	}
	values, ok := raw.([]any)
	if !ok {
		return nil, false, ErrCurrentToolkitSettingsDefinitionsInvalid
	}
	if len(values) == 0 {
		return nil, false, nil
	}
	configurationTypes := make([]string, 0, len(values))
	for _, value := range values {
		configurationType, ok := value.(string)
		if !ok || !validCurrentToolkitSchemaIdentifier(configurationType) {
			return nil, false, ErrCurrentToolkitSettingsDefinitionsInvalid
		}
		configurationTypes = append(configurationTypes, configurationType)
	}
	return configurationTypes, true, nil
}
