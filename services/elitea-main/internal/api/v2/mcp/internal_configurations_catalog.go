package mcp

// The internal configurations category is a fixed Main-owned surface.
//
// The current Python platform marks eight operations with mcp_tool=True.
// Types, models, and default-model mutation require the composed typed services.
const internalConfigurationsCategory = "configurations"

type internalConfigurationOperation string

const (
	internalListConfigurationTypesAvailable internalConfigurationOperation = "list_available_configuration_types"
	internalListConfigurations              internalConfigurationOperation = "list_configurations"
	internalCreateConfiguration             internalConfigurationOperation = "create_configuration"
	internalGetConfiguration                internalConfigurationOperation = "get_configuration"
	internalUpdateConfiguration             internalConfigurationOperation = "update_configuration"
	internalListStoredConfigurationTypes    internalConfigurationOperation = "list_stored_configuration_types"
	internalListConfigurationModels         internalConfigurationOperation = "list_configuration_models"
	internalSetDefaultConfigurationModel    internalConfigurationOperation = "set_default_configuration_model"
)

type internalConfigurationToolDefinition struct {
	name        string
	description string
	permission  string
	operation   internalConfigurationOperation
	schema      map[string]any
}

var internalConfigurationToolDefinitions = []internalConfigurationToolDefinition{
	{
		name: "get_configurations_available",
		description: "List available configuration type names. Supply type to read its complete creation schema. " +
			"Optionally restrict the result to one or more sections.",
		// The current REST catalogue is authenticated but not project-authorized.
		// Internal MCP is project-scoped and intended to construct project
		// configurations, so it deliberately requires the same read permission
		// as the configuration list rather than introducing a weaker execution
		// path through MCP.
		permission: "configurations.configurations.list",
		operation:  internalListConfigurationTypesAvailable,
		schema: objectSchema(map[string]any{
			"project_id": intProperty("Current project ID. The server verifies this value."),
			"type":       boundedStringProperty("Exact configuration type to retrieve with its schema. Omit to list names first.", 1, 128),
			"section": stringArrayProperty(
				"Optional configuration sections.", 64, 128,
			),
		}, "project_id"),
	},
	{
		name:        "get_configurations_configurations",
		description: "List configurations in the current project using bounded filters, sorting, and shared pagination.",
		permission:  "configurations.configurations.list",
		operation:   internalListConfigurations,
		schema: objectSchema(map[string]any{
			"project_id": intProperty("Current project ID. The server verifies this value."),
			"type":       stringArrayProperty("Optional configuration types.", 64, 128),
			"section":    stringArrayProperty("Optional configuration sections.", 64, 128),
			"ids":        boundedStringProperty("Optional comma-separated positive configuration IDs. Maximum 100; empty means no filter.", 0, 1200),
			"query":      boundedStringProperty("Optional label search.", 0, 1024),
			"offset":     integerMinProperty("Pagination offset.", 0),
			"limit":      integerRangeProperty("Maximum project-owned results.", 1, 200),
			"include_shared": map[string]any{
				"type": "boolean", "description": "Include shared configurations from the public project.",
			},
			"shared_offset": integerMinProperty("Shared-results pagination offset.", 0),
			"shared_limit":  integerRangeProperty("Maximum shared results.", 1, 200),
			"sort_by": enumProperty("Sort column.",
				"id", "uuid", "project_id", "label", "elitea_title", "type", "section",
				"data", "meta", "shared", "status_ok", "status_logs", "source", "author_id",
				"created_at", "updated_at"),
			"sort_order": enumProperty("Sort direction.", "asc", "desc"),
		}, "project_id"),
	},
	{
		name: "post_configurations_configurations",
		description: "Create a project configuration. Read get_configurations_available first and provide data " +
			"matching the selected type's schema. Main seals schema-declared secrets before persistence.",
		permission: "configurations.configuration.create",
		operation:  internalCreateConfiguration,
		schema: objectSchema(map[string]any{
			"project_id":   intProperty("Current project ID. The server verifies this value."),
			"elitea_title": configurationTitleProperty("Stable configuration identifier."),
			"label":        boundedStringProperty("Optional display name.", 0, 768),
			"type":         boundedStringProperty("Configuration type from the available catalogue.", 1, 128),
			"shared":       map[string]any{"type": "boolean"},
			"data":         openObjectProperty("Configuration payload matching the selected type schema."),
		}, "project_id", "elitea_title", "label", "type", "data"),
	},
	{
		name:        "get_configurations_configuration",
		description: "Read one configuration from the current project by numeric ID.",
		permission:  "configurations.configuration.details",
		operation:   internalGetConfiguration,
		schema: objectSchema(map[string]any{
			"project_id": intProperty("Current project ID. The server verifies this value."),
			"config_id":  intProperty("Configuration ID."),
		}, "project_id", "config_id"),
	},
	{
		name: "put_configurations_configuration",
		description: "Partially update one project configuration. Omitted fields remain unchanged; Main seals " +
			"schema-declared secrets and re-runs provider admission.",
		permission: "configurations.configuration.update",
		operation:  internalUpdateConfiguration,
		schema: objectSchema(map[string]any{
			"project_id":   intProperty("Current project ID. The server verifies this value."),
			"config_id":    intProperty("Configuration ID."),
			"elitea_title": configurationTitleProperty("Optional stable configuration identifier."),
			"label":        boundedStringProperty("Optional display name.", 0, 768),
			"shared":       map[string]any{"type": "boolean"},
			"data":         openObjectProperty("Replacement configuration payload."),
			"meta":         openObjectProperty("Replacement configuration metadata."),
		}, "project_id", "config_id"),
	},
}

var internalTypedConfigurationToolDefinitions = []internalConfigurationToolDefinition{
	{
		name:        "get_configurations_types",
		description: "List distinct configuration types already used in the project. This is not the configuration schema catalogue.",
		permission:  "configurations.configurations.list",
		operation:   internalListStoredConfigurationTypes,
		schema: objectSchema(map[string]any{
			"project_id": intProperty("Current project ID. The server verifies this value."),
			"section":    boundedStringProperty("Configuration section. Defaults to credentials; an empty string includes all sections.", 0, 128),
		}, "project_id"),
	},
	{
		name:        "get_configurations_models",
		description: "List selectable models and the current default for one section, optionally including shared models.",
		permission:  "configurations.configurations.list",
		operation:   internalListConfigurationModels,
		schema: objectSchema(map[string]any{
			"project_id":     intProperty("Current project ID. The server verifies this value."),
			"section":        boundedStringProperty("Model section. Defaults to llm. Unknown sections return an empty catalogue.", 0, 128),
			"include_shared": map[string]any{"type": "boolean", "description": "Include models shared by the public project. Defaults to false."},
		}, "project_id"),
	},
	{
		name:        "post_configurations_models",
		description: "Set the project's default model for a section. This does not create a model configuration.",
		permission:  "configurations.configuration.update",
		operation:   internalSetDefaultConfigurationModel,
		schema: objectSchema(map[string]any{
			"project_id":        intProperty("Current project ID. The server verifies this value."),
			"name":              boundedStringProperty("Existing model name.", 1, 1024),
			"target_project_id": intProperty("Project that supplies the model. This does not change the project whose default is updated."),
			"section": map[string]any{"anyOf": []any{
				boundedStringProperty("Model section. Defaults to llm.", 0, 128), map[string]any{"type": "null"},
			}},
		}, "project_id", "name", "target_project_id"),
	},
}

func internalConfigurationTools(typedAvailable bool) []Tool {
	tools := make([]Tool, 0, len(internalConfigurationToolDefinitions))
	definitions := internalConfigurationToolDefinitions
	if typedAvailable {
		definitions = append(append([]internalConfigurationToolDefinition{}, definitions...), internalTypedConfigurationToolDefinitions...)
	}
	for _, definition := range definitions {
		tools = append(tools, Tool{
			Name:                           definition.name,
			Description:                    definition.description,
			InputSchema:                    definition.schema,
			internalConfigurationOperation: definition.operation,
			permission:                     definition.permission,
		})
	}
	return tools
}

func stringArrayProperty(description string, maxItems, maxLength int) map[string]any {
	return map[string]any{
		"type":        "array",
		"description": description,
		"maxItems":    maxItems,
		"items": map[string]any{
			"type": "string", "maxLength": maxLength,
		},
	}
}

func configurationTitleProperty(description string) map[string]any {
	return map[string]any{
		"type": "string", "description": description, "minLength": 1, "maxLength": 128,
		"pattern": "^[A-Za-z0-9_-]+$",
	}
}
