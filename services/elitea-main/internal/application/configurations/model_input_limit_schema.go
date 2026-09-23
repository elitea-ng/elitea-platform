package configurations

import "encoding/json"

// Add the replatform input ceiling without changing the legacy source snapshot.
// The optional field belongs to model configuration, not per-chat preferences.
func (catalog *CurrentAvailableCatalog) addModelInputLimitSchema() error {
	index, ok := catalog.entryIndexes["llm_model"]
	if !ok {
		return ErrInvalidCurrentAvailableSnapshot
	}
	var schema map[string]any
	if err := json.Unmarshal(catalog.entries[index].ConfigSchema, &schema); err != nil {
		return ErrInvalidCurrentAvailableSnapshot
	}
	properties, ok := schema["properties"].(map[string]any)
	if !ok {
		return ErrInvalidCurrentAvailableSnapshot
	}
	data, ok := properties["data"].(map[string]any)
	if !ok {
		return ErrInvalidCurrentAvailableSnapshot
	}
	fields, ok := data["properties"].(map[string]any)
	if !ok {
		return ErrInvalidCurrentAvailableSnapshot
	}
	fields["max_input_tokens"] = map[string]any{
		"title": "Maximum Input Tokens", "default": nil,
		"description": "Optional provider input-only limit within the total context window. Leave empty when the provider specifies no separate input limit.",
		"anyOf":       []any{map[string]any{"type": "integer", "minimum": 1, "maximum": 4294967295}, map[string]any{"type": "null"}},
	}
	encoded, err := json.Marshal(schema)
	if err != nil {
		return ErrInvalidCurrentAvailableSnapshot
	}
	catalog.entries[index].ConfigSchema = encoded
	return nil
}
