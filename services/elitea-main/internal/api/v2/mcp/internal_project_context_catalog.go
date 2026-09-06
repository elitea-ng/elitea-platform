package mcp

// The fixed project-context category mirrors the three current operations
// marked mcp_tool=True. Draft generation is a separate model-backed operation
// and stays closed until Main owns that use case rather than proxying a
// compatibility stub.
const internalProjectContextCategory = "elitea_core/project_context"

type internalProjectContextOperation string

const (
	internalGetProjectContext    internalProjectContextOperation = "get_project_context"
	internalUpdateProjectContext internalProjectContextOperation = "update_project_context"
	internalDeleteProjectContext internalProjectContextOperation = "delete_project_context"
)

type internalProjectContextToolDefinition struct {
	name        string
	description string
	permission  string
	operation   internalProjectContextOperation
	schema      map[string]any
}

var internalProjectContextToolDefinitions = []internalProjectContextToolDefinition{
	{
		name:        "get_prompt_lib_project-context",
		description: "Return the current project's stored context content, enabled state, activation description, and update time.",
		permission:  "models.project_context.view",
		operation:   internalGetProjectContext,
		schema: objectSchema(map[string]any{
			"project_id": intProperty("Current project ID. The server verifies this value."),
		}, "project_id"),
	},
	{
		name: "put_prompt_lib_project-context",
		description: "Create or replace the current project's context. Content defaults to empty and enabled defaults to true. " +
			"Omit activation_description to preserve it, or pass a blank string to remove it.",
		permission: "models.project_context.edit",
		operation:  internalUpdateProjectContext,
		schema: objectSchema(map[string]any{
			"project_id": intProperty("Current project ID. The server verifies this value."),
			"content":    boundedStringProperty("Project context content.", 0, 2500),
			"enabled":    map[string]any{"type": "boolean", "description": "Whether project context is available to runtime delivery."},
			"activation_description": nullableBoundedProjectContextString(
				"When a model should load the full project context. Null or blank removes it.", 300,
			),
		}, "project_id"),
	},
	{
		name:        "delete_prompt_lib_project-context",
		description: "Delete the current project's stored context configuration.",
		permission:  "models.project_context.edit",
		operation:   internalDeleteProjectContext,
		schema: objectSchema(map[string]any{
			"project_id": intProperty("Current project ID. The server verifies this value."),
		}, "project_id"),
	},
}

func nullableBoundedProjectContextString(description string, maximum int) map[string]any {
	return map[string]any{
		"description": description,
		"anyOf": []any{
			map[string]any{"type": "string", "maxLength": maximum},
			map[string]any{"type": "null"},
		},
	}
}

func internalProjectContextTools() []Tool {
	tools := make([]Tool, 0, len(internalProjectContextToolDefinitions))
	for _, definition := range internalProjectContextToolDefinitions {
		tools = append(tools, Tool{
			Name:                            definition.name,
			Description:                     definition.description,
			InputSchema:                     definition.schema,
			internalProjectContextOperation: definition.operation,
			permission:                      definition.permission,
		})
	}
	return tools
}
