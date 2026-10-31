package configurations

import (
	"encoding/json"
	"strings"
	"unicode/utf8"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/toolcategory"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/platformconfig"
)

// AIDisclosureConfigurationType is a project's own AI disclosure marker
// (ADR-0043 decision 3, #1500): one tenant configuration row with type
// `ai_disclosure` in section `project_settings`, holding `marker_text`, the
// text the project's outbound tools add in place of the platform's, and
// `opt_out_toolkit_types`, the toolkit types the project turns the marker
// off for. A project can change the text but cannot remove the marker: an
// empty text is the platform's text, and an opt-out takes effect only for a
// type that the platform's `ai_disclosure` section lets projects opt out of.
// It takes the `models.project_settings.edit` gate of the other project
// settings.
const (
	AIDisclosureConfigurationType = "ai_disclosure"
	AIDisclosureSection           = "project_settings"
)

// addAIDisclosureProjectSetting registers the type beside the pinned legacy
// entries, on the envelope of another project-settings entry, as
// addWorkerEgressProjectSetting does.
func (catalog *CurrentAvailableCatalog) addAIDisclosureProjectSetting() error {
	if _, exists := catalog.entryIndexes[AIDisclosureConfigurationType]; exists {
		return nil
	}
	template, ok := catalog.entryIndexes["project_icon"]
	if !ok {
		return ErrInvalidCurrentAvailableSnapshot
	}
	var schema map[string]any
	if err := json.Unmarshal(catalog.entries[template].ConfigSchema, &schema); err != nil {
		return ErrInvalidCurrentAvailableSnapshot
	}
	properties, ok := schema["properties"].(map[string]any)
	if !ok {
		return ErrInvalidCurrentAvailableSnapshot
	}
	properties["data"] = map[string]any{
		"title": "AIDisclosure",
		"type":  "object",
		"metadata": map[string]any{
			"label": "AI disclosure", "section": AIDisclosureSection, "type": AIDisclosureConfigurationType,
		},
		"properties": map[string]any{
			"marker_text": map[string]any{
				"title": "Marker text",
				"description": "The text this project's outbound tools add to the content they send, in place " +
					"of the platform's text. Empty is the platform's text. You cannot remove the marker.",
				"default": nil,
				"anyOf": []any{
					map[string]any{"type": "string", "maxLength": platformconfig.MaxAIDisclosureMarkerTextBytes},
					map[string]any{"type": "null"},
				},
			},
			"opt_out_toolkit_types": map[string]any{
				"title": "Opt-out toolkit types",
				"description": "Toolkit types that do not add the marker for this project. Only the types " +
					"that the platform administrator allows take effect.",
				"default": nil,
				"anyOf": []any{
					map[string]any{"type": "array", "items": map[string]any{
						"type": "string", "enum": toolcategory.DisclosureToolkitTypes(), "maxLength": 128,
					}, "maxItems": len(toolcategory.DisclosureToolkitTypes())},
					map[string]any{"type": "null"},
				},
			},
		},
	}
	schema["title"] = "AI disclosure"
	encoded, err := json.Marshal(schema)
	if err != nil {
		return ErrInvalidCurrentAvailableSnapshot
	}
	catalog.entryIndexes[AIDisclosureConfigurationType] = len(catalog.entries)
	catalog.entries = append(catalog.entries, CurrentAvailableConfigurationType{
		Type: AIDisclosureConfigurationType, Section: AIDisclosureSection, ConfigSchema: encoded,
	})
	return nil
}

// normalizeAIDisclosureSetting keeps `marker_text` (trimmed, at most 200
// bytes, no control character) and `opt_out_toolkit_types` (known types
// only, by their contract names, each once).
func normalizeAIDisclosureSetting(data map[string]any) (map[string]any, error) {
	if data == nil {
		return nil, currentLocalConfigurationFieldError("data")
	}
	normalized := map[string]any{}
	if raw, present := data["marker_text"]; present && raw != nil {
		text, ok := raw.(string)
		text = strings.TrimSpace(text)
		if !ok || len(text) > platformconfig.MaxAIDisclosureMarkerTextBytes || !utf8.ValidString(text) || hasControl(text) {
			return nil, currentLocalConfigurationFieldError("data.marker_text")
		}
		normalized["marker_text"] = text
	}
	if raw, present := data["opt_out_toolkit_types"]; present && raw != nil {
		items, ok := raw.([]any)
		if !ok {
			return nil, currentLocalConfigurationFieldError("data.opt_out_toolkit_types")
		}
		out := make([]any, 0, len(items))
		seen := map[string]bool{}
		for _, item := range items {
			entry, ok := item.(string)
			name, known := toolcategory.DisclosureToolkitType(entry)
			if !ok || !known {
				return nil, currentLocalConfigurationFieldError("data.opt_out_toolkit_types")
			}
			if !seen[name] {
				seen[name] = true
				out = append(out, name)
			}
		}
		normalized["opt_out_toolkit_types"] = out
	}
	return normalized, nil
}

func hasControl(text string) bool {
	for _, r := range text {
		if r < 0x20 || r == 0x7f {
			return true
		}
	}
	return false
}
