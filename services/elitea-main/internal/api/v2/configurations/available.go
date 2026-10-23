package configurations

import (
	"encoding/json"

	configurationapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/configurations"
)

// CurrentAvailableConfigurationTypeDTO preserves the current
// /configurations/available item shape. The function-name fields are strings
// in the current Flask response (not callable values); nil is intentionally
// encoded as JSON null for compatibility.
type CurrentAvailableConfigurationTypeDTO struct {
	Type                 string          `json:"type"`
	Section              string          `json:"section"`
	ConfigSchema         json.RawMessage `json:"config_schema"`
	HasTestConnection    bool            `json:"has_test_connection"`
	CheckConnectionLabel *string         `json:"check_connection_label"`
	ValidationFunc       *string         `json:"validation_func"`
	CheckConnectionFunc  *string         `json:"check_connection_func"`
}

func newCurrentAvailableConfigurationTypeDTO(
	entry configurationapp.CurrentAvailableConfigurationType,
) CurrentAvailableConfigurationTypeDTO {
	return CurrentAvailableConfigurationTypeDTO{
		Type:                 entry.Type,
		Section:              entry.Section,
		ConfigSchema:         entry.ConfigSchema,
		HasTestConnection:    entry.HasTestConnection,
		CheckConnectionLabel: entry.CheckConnectionLabel,
		ValidationFunc:       entry.ValidationFunc,
		CheckConnectionFunc:  entry.CheckConnectionFunc,
	}
}

func newCurrentAvailableConfigurationTypesDTO(
	entries []configurationapp.CurrentAvailableConfigurationType,
) []CurrentAvailableConfigurationTypeDTO {
	result := make([]CurrentAvailableConfigurationTypeDTO, len(entries))
	for index, entry := range entries {
		result[index] = newCurrentAvailableConfigurationTypeDTO(entry)
	}
	return result
}
