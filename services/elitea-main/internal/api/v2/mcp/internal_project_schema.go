package mcp

// Main injects the authenticated endpoint project in callInternalTool.
// Keep that routing field outside the model's editable argument schema.
func withEndpointProjectSchema(tool Tool) Tool {
	if tool.permission == "" {
		return tool
	}
	schema := make(map[string]any, len(tool.InputSchema))
	for key, value := range tool.InputSchema {
		schema[key] = value
	}
	if original, ok := schema["properties"].(map[string]any); ok {
		properties := make(map[string]any, len(original))
		for key, value := range original {
			if key != "project_id" {
				properties[key] = value
			}
		}
		schema["properties"] = properties
	}
	if original, ok := schema["required"].([]string); ok {
		required := make([]string, 0, len(original))
		for _, key := range original {
			if key != "project_id" {
				required = append(required, key)
			}
		}
		if len(required) == 0 {
			delete(schema, "required")
		} else {
			schema["required"] = required
		}
	}
	tool.InputSchema = schema
	return tool
}
