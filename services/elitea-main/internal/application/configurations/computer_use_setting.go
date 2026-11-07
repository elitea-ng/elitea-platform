package configurations

import "encoding/json"

// ComputerUseConfigurationType is a project's desktop computer use setting
// (ADR-0047 decision 9, #1585): one tenant configuration row with type
// `computer_use` in section `project_settings`. `upload_screenshots`
// (default false) uploads the screenshots a desktop turn's computer tools
// take as run artifacts. Off, they stay on the device: the turn commit
// records only that each was taken, its size and its digest. It takes the
// `models.project_settings.edit` gate of the other project settings.
const (
	ComputerUseConfigurationType = "computer_use"
	ComputerUseSection           = "project_settings"
)

// addComputerUseProjectSetting registers the type beside the pinned legacy
// entries, on the envelope of another project-settings entry, as
// addBrowserProjectSetting does.
func (catalog *CurrentAvailableCatalog) addComputerUseProjectSetting() error {
	if _, exists := catalog.entryIndexes[ComputerUseConfigurationType]; exists {
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
		"title": "Computer use",
		"type":  "object",
		"metadata": map[string]any{
			"label": "Desktop computer use", "section": ComputerUseSection, "type": ComputerUseConfigurationType,
		},
		"properties": map[string]any{
			"upload_screenshots": map[string]any{
				"title": "Upload screenshots",
				"description": "Upload the screenshots the desktop agent takes of other applications as run " +
					"artifacts. Off, they stay on the person's device.",
				"type":    "boolean",
				"default": false,
			},
		},
	}
	schema["title"] = "Desktop computer use"
	encoded, err := json.Marshal(schema)
	if err != nil {
		return ErrInvalidCurrentAvailableSnapshot
	}
	catalog.entryIndexes[ComputerUseConfigurationType] = len(catalog.entries)
	catalog.entries = append(catalog.entries, CurrentAvailableConfigurationType{
		Type: ComputerUseConfigurationType, Section: ComputerUseSection, ConfigSchema: encoded,
	})
	return nil
}

// normalizeComputerUseSetting keeps `upload_screenshots` (default false).
// Other keys are dropped.
func normalizeComputerUseSetting(data map[string]any) (map[string]any, error) {
	if data == nil {
		return nil, currentLocalConfigurationFieldError("data")
	}
	normalized := map[string]any{"upload_screenshots": false}
	if raw, present := data["upload_screenshots"]; present && raw != nil {
		upload, ok := raw.(bool)
		if !ok {
			return nil, currentLocalConfigurationFieldError("data.upload_screenshots")
		}
		normalized["upload_screenshots"] = upload
	}
	return normalized, nil
}
