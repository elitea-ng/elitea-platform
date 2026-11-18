package configurations

import (
	"encoding/json"
	"fmt"

	configurationdomain "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/configurations"
)

// Native credential types: the supported way to add a credential type that the
// pinned SDK snapshot (SDK revision a78d3654, 2026-07-22) does not have
// (D1 of epic #2254, #2255).
//
// The snapshot is no longer regenerated (its generator was deleted with the
// Python worker in #1312), so a type the SDK gained after the pin is DEFINED
// HERE, in Go, and registered beside the pinned entries by
// LoadPinnedCurrentAvailableCatalog. The definition is checked against a
// captured upstream fixture (internal/sdkfixture, testdata/sdk-fixtures) so it
// cannot drift from the SDK's model unnoticed. docs/contributing/adding-types.md
// is the walkthrough.
//
// A native type is validated by Main's own schema rules (required fields,
// secret fields, the normalizer chain), not by the SDK validation boundary, so
// its entry carries no validation function. A connection check is separate: set
// CheckConnection and add the probe to toolkitCheckProbes
// (api/v2/configurations/toolkit_check.go), or, for a delegated sign-in type,
// answer through check_connection_oauth.go.

// NativeConfigurationType is one credential type defined natively.
type NativeConfigurationType struct {
	// Type is the configuration type id, lowercase with underscores (the id a
	// toolkit's `configuration_types` annotation names).
	Type string
	// Section is "credentials" for a credential type.
	Section string
	// Title is the data model's title, the SDK class name (TavilyConfiguration).
	Title string
	// Label is what the credential picker shows.
	Label           string
	Categories      []string
	ExtraCategories []string
	// Properties is the JSON Schema of the type's fields, as the SDK's pydantic
	// model_json_schema() writes them: a secret field carries `"secret": true`,
	// `"format": "password"` and `"writeOnly": true`.
	Properties map[string]any
	// Required names the required fields.
	Required []string
	// AuthSubsections groups the fields the form shows under "auth": each
	// subsection is a name and the fields in it. Empty means no `sections`.
	AuthSubsections []NativeAuthSubsection
	// CheckConnection reports that a probe exists for the type.
	CheckConnection bool
	// Hidden registers the type but keeps it out of the create pickers
	// (`metadata.hidden`, which the web credential type selector honours), for
	// a type whose flow is not ready. Existing rows of the type still open and
	// edit; the type stays in the available catalogue.
	Hidden bool
	// OwnNormalizer says the type's create rules are written by its own
	// story: it adds its own owner in NewCurrentConfigurationDataNormalizer
	// and the default (CurrentNativeDataNormalizer) steps aside. Without it
	// the default owns the type. Exactly one owner, or Main does not boot.
	OwnNormalizer bool
}

// NativeAuthSubsection is one group of fields of the form's auth section.
type NativeAuthSubsection struct {
	Name   string
	Fields []string
}

// nativeConfigurationTypes is the registry. Add the next post-pin type here.
// The Microsoft delegated types (outlook, teams, ms_graph, dataverse) are in
// native_types_microsoft.go (ADR-0057, #2266). Add the next one beside them.
var nativeConfigurationTypes = microsoftNativeTypes()

// NativeConfigurationTypes lists the natively defined types (a copy), for the
// guards that check them against the other catalogues.
func NativeConfigurationTypes() []NativeConfigurationType {
	return append([]NativeConfigurationType(nil), nativeConfigurationTypes...)
}

// nativeEnvelopeType is the pinned entry whose ConfigurationCreateBase envelope
// (elitea_title, label, shared, type) every native credential type reuses.
const nativeEnvelopeType = "github"

// addNativeConfigurationTypes registers the native types beside the pinned
// entries. A type already defined by the pinned snapshot (or twice natively)
// is refused: one type, one definition.
func (catalog *CurrentAvailableCatalog) addNativeConfigurationTypes(
	natives []NativeConfigurationType,
) error {
	if len(natives) == 0 {
		return nil
	}
	template, ok := catalog.entryIndexes[nativeEnvelopeType]
	if !ok {
		return ErrInvalidCurrentAvailableSnapshot
	}
	for _, native := range natives {
		if _, exists := catalog.entryIndexes[native.Type]; exists {
			return fmt.Errorf("%w: native type %q is already defined", ErrInvalidCurrentAvailableSnapshot, native.Type)
		}
		if len(native.Properties) == 0 || native.Title == "" || native.Label == "" {
			return fmt.Errorf("%w: native type %q is incomplete", ErrInvalidCurrentAvailableSnapshot, native.Type)
		}
		encoded, err := nativeConfigSchema(catalog.entries[template].ConfigSchema, native)
		if err != nil {
			return err
		}
		if _, err := configurationdomain.NewRegistrySnapshot([]configurationdomain.RegistryEntryDefinition{{
			Type: native.Type, Section: native.Section, JSONSchema: encoded,
			ConnectionCheckSupported: native.CheckConnection,
		}}); err != nil {
			return fmt.Errorf("%w: native type %q: %v", ErrInvalidCurrentAvailableSnapshot, native.Type, err)
		}
		entry := CurrentAvailableConfigurationType{
			Type: native.Type, Section: native.Section, ConfigSchema: encoded,
			HasTestConnection: native.CheckConnection,
		}
		if native.CheckConnection {
			function := currentCheckConnectionFunction
			entry.CheckConnectionFunc = &function
		}
		catalog.entryIndexes[native.Type] = len(catalog.entries)
		catalog.entries = append(catalog.entries, entry)
	}
	return nil
}

// currentCheckConnectionFunction is the registry's check-connection function
// name, the same on every entry that supports a check.
const currentCheckConnectionFunction = "applications_configuration_check_connection"

func nativeConfigSchema(envelope json.RawMessage, native NativeConfigurationType) (json.RawMessage, error) {
	var schema map[string]any
	if err := json.Unmarshal(envelope, &schema); err != nil {
		return nil, ErrInvalidCurrentAvailableSnapshot
	}
	properties, ok := schema["properties"].(map[string]any)
	if !ok {
		return nil, ErrInvalidCurrentAvailableSnapshot
	}
	metadata := map[string]any{
		"categories":                 nonNilStrings(native.Categories),
		"check_connection_supported": native.CheckConnection,
		"extra_categories":           nonNilStrings(native.ExtraCategories),
		"icon_url":                   nil,
		"label":                      native.Label,
		"section":                    native.Section,
		"type":                       native.Type,
	}
	if native.Hidden {
		metadata["hidden"] = true
	}
	if len(native.AuthSubsections) > 0 {
		subsections := make([]any, 0, len(native.AuthSubsections))
		for _, subsection := range native.AuthSubsections {
			subsections = append(subsections, map[string]any{
				"fields": nonNilStrings(subsection.Fields), "name": subsection.Name,
			})
		}
		metadata["sections"] = map[string]any{
			"auth": map[string]any{"required": true, "subsections": subsections},
		}
	}
	data := map[string]any{
		"metadata":   metadata,
		"properties": native.Properties,
		"title":      native.Title,
		"type":       "object",
	}
	if len(native.Required) > 0 {
		data["required"] = nonNilStrings(native.Required)
	}
	properties["data"] = data
	schema["title"] = native.Label
	encoded, err := json.Marshal(schema)
	if err != nil {
		return nil, ErrInvalidCurrentAvailableSnapshot
	}
	return encoded, nil
}

func nonNilStrings(values []string) []string {
	if values == nil {
		return []string{}
	}
	return values
}
