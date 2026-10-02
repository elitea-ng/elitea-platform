package configurations

import "testing"

func TestPinnedModelSchemaExposesOptionalInputCeiling(t *testing.T) {
	catalog, err := LoadPinnedCurrentAvailableCatalog()
	if err != nil {
		t.Fatal(err)
	}
	data, ok := catalog.DataSchemaByType("llm_model")
	if !ok {
		t.Fatal("missing model schema")
	}
	fields := data["properties"].(map[string]any)
	field, ok := fields["max_input_tokens"].(map[string]any)
	if !ok || field["default"] != nil {
		t.Fatal("input ceiling must be optional")
	}
	for _, key := range data["required"].([]any) {
		if key == "max_input_tokens" {
			t.Fatal("input ceiling must not be required")
		}
	}
	plain, err := LoadCurrentAvailableCatalog([]byte(pinnedCurrentAvailableSnapshot))
	if err != nil {
		t.Fatal(err)
	}
	legacy, _ := plain.DataSchemaByType("llm_model")
	if _, exists := legacy["properties"].(map[string]any)["max_input_tokens"]; exists {
		t.Fatal("legacy snapshot changed")
	}
}
