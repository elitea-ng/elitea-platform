package configurations

import (
	"encoding/json"
	"testing"

	configurationapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/configurations"
)

func TestCurrentAvailableDTOEncodesCurrentFunctionFieldsAndNulls(t *testing.T) {
	catalog, err := configurationapp.LoadPinnedCurrentAvailableCatalog()
	if err != nil {
		t.Fatalf("LoadPinnedCurrentAvailableCatalog() error = %v", err)
	}

	entries, err := catalog.CompleteEntries("credentials", "llm")
	if err != nil {
		t.Fatalf("CompleteEntries() error = %v", err)
	}
	dtos := newCurrentAvailableConfigurationTypesDTO(entries)
	raw, err := json.Marshal(dtos)
	if err != nil {
		t.Fatalf("json.Marshal() error = %v", err)
	}

	var decoded []map[string]any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("json.Unmarshal() error = %v", err)
	}
	github := findCurrentAvailableDTO(t, decoded, "github")
	if github["validation_func"] != "applications_configuration_validator" ||
		github["check_connection_func"] != "applications_configuration_check_connection" {
		t.Fatalf("github function fields = %#v", github)
	}
	llm := findCurrentAvailableDTO(t, decoded, "llm_model")
	if value, exists := llm["validation_func"]; !exists || value != nil {
		t.Fatalf("llm_model validation_func = %#v, exists = %v", value, exists)
	}
	if value, exists := llm["check_connection_label"]; !exists || value != nil {
		t.Fatalf("llm_model check_connection_label = %#v, exists = %v", value, exists)
	}
}

func findCurrentAvailableDTO(t *testing.T, entries []map[string]any, typeName string) map[string]any {
	t.Helper()
	for _, entry := range entries {
		if entry["type"] == typeName {
			return entry
		}
	}
	t.Fatalf("entry %q not found", typeName)
	return nil
}
