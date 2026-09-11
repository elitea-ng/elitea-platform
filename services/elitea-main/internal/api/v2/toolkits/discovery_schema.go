package toolkits

// agentToolkitSettingsSchema describes saved settings, rather than UI controls.
// The SDK catalogue owns toolkit settings. Credential references are owned by
// Main's resolver and the current platform credential picker.
func (h *Handler) agentToolkitSettingsSchema(name string, served map[string]any) (map[string]any, error) {
	settings, _, found, err := h.toolkitCatalogueEntry(name)
	if err != nil {
		return nil, err
	}
	if !found {
		settings = served
	}
	result := make(map[string]any, len(settings)+1)
	for key, value := range settings {
		result[key] = value
	}
	if metadata, present := served["metadata"]; present {
		result["metadata"] = metadata
	}
	original, _ := settings["properties"].(map[string]any)
	properties := make(map[string]any, len(original))
	for key, value := range original {
		property, ok := value.(map[string]any)
		if !ok {
			properties[key] = value
			continue
		}
		if _, credential := property["configuration_types"]; credential {
			properties[key] = map[string]any{
				"type":        "object",
				"description": "Saved credential reference. Use elitea_title from the configuration result. private=true selects the personal project; false selects the current project.",
				"properties": map[string]any{
					"elitea_title": map[string]any{"type": "string", "minLength": 1, "maxLength": 128},
					"private":      map[string]any{"type": "boolean"},
				},
				"required":             []string{"elitea_title", "private"},
				"additionalProperties": false,
				"configuration_types":  property["configuration_types"],
			}
			continue
		}
		properties[key] = value
	}
	result["properties"] = properties
	return result, nil
}
