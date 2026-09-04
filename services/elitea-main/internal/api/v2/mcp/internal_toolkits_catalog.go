package mcp

// The internal toolkit builder is a fixed, Main-owned MCP category.
//
// The current Python platform marks six operations with mcp_tool=True. Five
// are published here. The sixth, get_elitea_core_toolkit_available_tools, is
// deliberately absent until Main has a real per-instance discovery provider:
// its current REST repository query reads application-tool relations and is
// not the SDK/remote discovery the Python operation performs.
const internalToolkitsCategory = "elitea_core/toolkits"

type internalToolkitOperation string

const (
	internalListToolkitTypes   internalToolkitOperation = "list_toolkit_types"
	internalListToolkits       internalToolkitOperation = "list_toolkits"
	internalCreateToolkit      internalToolkitOperation = "create_toolkit"
	internalUpdateToolkit      internalToolkitOperation = "update_toolkit"
	internalUpdateToolRelation internalToolkitOperation = "update_tool_relation"
)

type internalToolkitToolDefinition struct {
	name        string
	description string
	permission  string
	operation   internalToolkitOperation
	schema      map[string]any
}

var internalToolkitToolDefinitions = []internalToolkitToolDefinition{
	{
		name:        "get_elitea_core_toolkits",
		description: "List the toolkit type schemas available for creating toolkit instances in this project.",
		permission:  "models.applications.toolkits.details",
		operation:   internalListToolkitTypes,
		schema: objectSchema(map[string]any{
			"project_id": intProperty("Current project ID. The server verifies this value."),
		}, "project_id"),
	},
	{
		name:        "get_elitea_core_tools",
		description: "List toolkit instances in the current project with bounded pagination.",
		permission:  "models.applications.tools.list",
		operation:   internalListToolkits,
		schema: objectSchema(map[string]any{
			"project_id": intProperty("Current project ID. The server verifies this value."),
			"limit":      integerRangeProperty("Maximum results.", 1, 100),
			"offset":     integerMinProperty("Pagination offset.", 0),
		}, "project_id"),
	},
	{
		name: "post_elitea_core_tools",
		description: "Create a toolkit instance. Read get_elitea_core_toolkits first and use the selected " +
			"type's settings schema.",
		permission: "models.applications.tools.create",
		operation:  internalCreateToolkit,
		schema: objectSchema(map[string]any{
			"project_id":  intProperty("Current project ID. The server verifies this value."),
			"type":        boundedStringProperty("Toolkit type from the current type catalogue.", 1, 256),
			"name":        boundedStringProperty("Toolkit name.", 1, 768),
			"description": boundedStringProperty("Optional toolkit description.", 0, 2304),
			"settings":    openObjectProperty("Toolkit settings matching the selected type schema."),
			"meta":        openObjectProperty("Optional toolkit metadata."),
		}, "project_id", "type", "name"),
	},
	{
		name: "put_elitea_core_tool",
		description: "Update an existing toolkit instance. Supply only fields that must change; credential " +
			"references, dynamic secrets, and deployment guardrails are validated by Main.",
		permission: "models.applications.tool.update",
		operation:  internalUpdateToolkit,
		schema: objectSchema(map[string]any{
			"project_id":  intProperty("Current project ID. The server verifies this value."),
			"toolkit_id":  intProperty("Toolkit ID."),
			"type":        boundedStringProperty("Optional toolkit type.", 1, 256),
			"name":        boundedStringProperty("Optional toolkit name.", 1, 768),
			"description": boundedStringProperty("Optional toolkit description.", 0, 2304),
			"settings":    openObjectProperty("Replacement settings matching the toolkit type schema."),
			"meta":        openObjectProperty("Replacement toolkit metadata."),
		}, "project_id", "toolkit_id"),
	},
	{
		name:        "patch_elitea_core_tool",
		description: "Link or unlink a toolkit from an agent version, optionally replacing its selected tools.",
		permission:  "models.applications.tool.patch",
		operation:   internalUpdateToolRelation,
		schema: objectSchema(map[string]any{
			"project_id":        intProperty("Current project ID. The server verifies this value."),
			"toolkit_id":        intProperty("Toolkit ID."),
			"entity_id":         intProperty("Owning agent application ID."),
			"entity_version_id": intProperty("Owning agent version ID."),
			"entity_type":       enumProperty("Relation entity type.", "agent"),
			"has_relation":      map[string]any{"type": "boolean"},
			"selected_tools": map[string]any{
				"type": "array", "items": map[string]any{"type": "string", "minLength": 1},
			},
		}, "project_id", "toolkit_id", "entity_id", "entity_version_id", "has_relation"),
	},
}

func internalToolkitTools() []Tool {
	tools := make([]Tool, 0, len(internalToolkitToolDefinitions))
	for _, definition := range internalToolkitToolDefinitions {
		tools = append(tools, Tool{
			Name:                     definition.name,
			Description:              definition.description,
			InputSchema:              definition.schema,
			internalToolkitOperation: definition.operation,
			permission:               definition.permission,
		})
	}
	return tools
}

func openObjectProperty(description string) map[string]any {
	return map[string]any{
		"type":                 "object",
		"description":          description,
		"additionalProperties": true,
	}
}
