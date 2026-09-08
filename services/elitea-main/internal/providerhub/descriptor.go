package providerhub

// The descriptor document, as a Go model.
//
// The authoritative schema is `libs/provider/legacy/v0/
// ExternalServiceProviderDescriptor.json`, which pylon compiles into a pydantic
// model at plugin load with `remove_special_field_name_prefix=True`. That flag
// is why the schema declares `_description`, `_type` and `_required` while
// every descriptor actually published on the wire spells them `description`,
// `type` and `required`.
//
// Both spellings are accepted here. A provider that emits the schema literally
// is valid by the published schema, and a model that only read the unprefixed
// names would silently drop its descriptions and — worse — its parameter types,
// which decide what the create form renders. The prefixed name is used only
// when the unprefixed one is absent, so a document carrying both is read the
// way the wire reads it.
//
// The manifest stays `[]byte` everywhere else. It is content-addressed by
// sha256 over the published bytes, so nothing may re-serialise it. This model
// is a READER: it never round-trips.

import (
	"encoding/json"
	"fmt"
	"strings"
)

// maxDescriptorParseBytes bounds what one document may cost to decode. The
// registrar and the administration route both refuse a descriptor larger than
// one mebibyte before it is stored, so a stored document is already within this
// bound; the check is here so a row written before that limit existed, or by a
// future writer, cannot make a catalogue read unbounded.
const maxDescriptorParseBytes = 1 << 20

// Descriptor is one provider's published capability document.
type Descriptor struct {
	Name               string              `json:"name"`
	ServiceLocationURL string              `json:"service_location_url"`
	ProvidedToolkits   []ToolkitDescriptor `json:"provided_toolkits"`
}

// ToolkitDescriptor is one toolkit a provider offers.
type ToolkitDescriptor struct {
	Name            string                  `json:"name"`
	Description     string                  `json:"description"`
	UnderscoreDesc  string                  `json:"_description"`
	ToolkitConfig   ToolkitConfigDescriptor `json:"toolkit_config"`
	ProvidedTools   []ToolDescriptor        `json:"provided_tools"`
	ToolkitMetadata map[string]any          `json:"toolkit_metadata"`
}

// Describe answers the toolkit's description in either spelling.
func (t ToolkitDescriptor) Describe() string {
	if t.Description != "" {
		return t.Description
	}
	return t.UnderscoreDesc
}

// TypeOverride answers the toolkit type name the provider asked for, if any.
//
// pylon reads `toolkit_metadata.type_override` and uses it INSTEAD of the
// `<provider>_<toolkit>` composition. Nearly every descriptor published today
// sets it, so a port that only composed the two names would rename every
// existing toolkit type.
func (t ToolkitDescriptor) TypeOverride() string {
	override, _ := t.ToolkitMetadata["type_override"].(string)
	return strings.TrimSpace(override)
}

// ToolkitConfigDescriptor is a toolkit's settings declaration.
type ToolkitConfigDescriptor struct {
	Type           string                         `json:"type"`
	UnderscoreType string                         `json:"_type"`
	Description    string                         `json:"description"`
	UnderscoreDesc string                         `json:"_description"`
	FieldsOrder    []string                       `json:"fields_order"`
	Parameters     map[string]ParameterDescriptor `json:"parameters"`
}

// ParameterDescriptor is one settings field.
type ParameterDescriptor struct {
	Type            string         `json:"type"`
	UnderscoreType  string         `json:"_type"`
	Description     string         `json:"description"`
	UnderscoreDesc  string         `json:"_description"`
	Required        *bool          `json:"required"`
	UnderscoreReq   *bool          `json:"_required"`
	JSONSchemaExtra map[string]any `json:"json_schema_extra"`
	Default         any            `json:"default"`
}

// DeclaredType answers the parameter type in either spelling. An absent type is
// reported as the empty string; the projection maps that to an object, which is
// what pylon's own `else` arm does.
func (p ParameterDescriptor) DeclaredType() string {
	if p.Type != "" {
		return p.Type
	}
	return p.UnderscoreType
}

// Describe answers the parameter description in either spelling.
func (p ParameterDescriptor) Describe() string {
	if p.Description != "" {
		return p.Description
	}
	return p.UnderscoreDesc
}

// IsRequired answers whether the parameter is required. The schema's default is
// false, so an absent flag in BOTH spellings is not required.
func (p ParameterDescriptor) IsRequired() bool {
	if p.Required != nil {
		return *p.Required
	}
	if p.UnderscoreReq != nil {
		return *p.UnderscoreReq
	}
	return false
}

// ToolDescriptor is one tool a toolkit offers.
type ToolDescriptor struct {
	Name           string                   `json:"name"`
	Description    string                   `json:"description"`
	UnderscoreDesc string                   `json:"_description"`
	ArgsSchema     map[string]ArgDescriptor `json:"args_schema"`
}

// Describe answers the tool description in either spelling.
func (t ToolDescriptor) Describe() string {
	if t.Description != "" {
		return t.Description
	}
	return t.UnderscoreDesc
}

// ArgDescriptor is one tool argument.
//
// The numeric bounds carry pydantic's own spellings — gt/ge/lt/le — because
// that is what a provider publishes. The projection maps them to JSON Schema.
type ArgDescriptor struct {
	Type        string `json:"type"`
	Description string `json:"description"`
	Required    bool   `json:"required"`
	Default     any    `json:"default"`
	Enum        []any  `json:"enum"`
	GreaterThan any    `json:"gt"`
	GreaterEq   any    `json:"ge"`
	LessThan    any    `json:"lt"`
	LessEq      any    `json:"le"`
	// HasDefault records that the document carried a `default` key, so a
	// declared default of null, 0 or "" is kept rather than dropped. pylon
	// tests `"default" in schema`, not truthiness, for exactly this reason.
	HasDefault bool `json:"-"`
}

// UnmarshalJSON records whether `default` was present at all.
func (a *ArgDescriptor) UnmarshalJSON(data []byte) error {
	type plain ArgDescriptor
	var decoded plain
	if err := json.Unmarshal(data, &decoded); err != nil {
		return err
	}
	var keys map[string]json.RawMessage
	if err := json.Unmarshal(data, &keys); err != nil {
		return err
	}
	_, hasDefault := keys["default"]
	*a = ArgDescriptor(decoded)
	a.HasDefault = hasDefault
	return nil
}

// ParseDescriptor reads one stored manifest.
//
// An unreadable document is an error, never an empty descriptor. A manifest
// that cannot be parsed is a real fault — the bytes were accepted at
// registration and are content-addressed — and a caller that read it as "this
// provider offers no toolkits" would erase a whole provider from the catalogue
// with no record.
func ParseDescriptor(manifest []byte) (Descriptor, error) {
	if len(manifest) == 0 {
		return Descriptor{}, fmt.Errorf("providerhub: the stored manifest is empty")
	}
	if len(manifest) > maxDescriptorParseBytes {
		return Descriptor{}, fmt.Errorf(
			"providerhub: the stored manifest is %d bytes, over the %d-byte limit",
			len(manifest), maxDescriptorParseBytes)
	}
	var descriptor Descriptor
	if err := json.Unmarshal(manifest, &descriptor); err != nil {
		return Descriptor{}, fmt.Errorf("providerhub: parse the stored manifest: %w", err)
	}
	if strings.TrimSpace(descriptor.Name) == "" {
		return Descriptor{}, fmt.Errorf("providerhub: the stored manifest carries no name")
	}
	return descriptor, nil
}
