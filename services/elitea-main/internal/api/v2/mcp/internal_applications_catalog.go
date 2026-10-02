package mcp

// The internal application builder is a fixed, Main-owned MCP category.
//
// The current Python platform marks individual REST operations with
// `mcp_tool=True`. The Go OpenAPI document has no equivalent marker, so this
// list is intentionally explicit. Do not derive it from every applications
// route: that would publish delete, publish, and execution operations that the
// current platform deliberately keeps out of this category.

const internalApplicationsCategory = "elitea_core/applications"

type internalApplicationOperation string

const (
	internalListApplications    internalApplicationOperation = "list_applications"
	internalCreateApplication   internalApplicationOperation = "create_application"
	internalGetApplication      internalApplicationOperation = "get_application"
	internalCreateVersion       internalApplicationOperation = "create_version"
	internalGetVersion          internalApplicationOperation = "get_version"
	internalUpdateVersion       internalApplicationOperation = "update_version"
	internalPatchInstructions   internalApplicationOperation = "patch_instructions"
	internalUpdateAgentRelation internalApplicationOperation = "update_application_relation"
)

type internalApplicationToolDefinition struct {
	name        string
	description string
	permission  string
	operation   internalApplicationOperation
	schema      map[string]any
}

var internalApplicationToolDefinitions = []internalApplicationToolDefinition{
	{
		name: "get_elitea_core_applications",
		description: "List agents in the current project. Set agents_type to pipeline to list pipelines. " +
			"Filter by text, tags, author, status, IDs, likes, or trend dates. Omit agents_type for all applications.",
		permission: "models.applications.applications.list",
		operation:  internalListApplications,
		schema: objectSchema(map[string]any{
			"project_id":         intProperty("Current project ID. The server verifies this value."),
			"query":              stringProperty("Optional name search."),
			"agents_type":        enumProperty("Optional application type filter. Omit for all.", "all", "classic", "pipeline"),
			"limit":              integerRangeProperty("Maximum results.", 1, 100),
			"offset":             integerRangeProperty("Pagination offset.", 0, 100000),
			"tags":               boundedStringProperty("Comma-separated tag names or IDs. Every tag must match.", 0, 12800),
			"author_id":          integerRangeProperty("Match an author of any version.", 1, 2147483647),
			"statuses":           boundedStringProperty("Comma-separated version statuses. Any listed status can match.", 0, 2079),
			"ids":                boundedStringProperty("Comma-separated positive application IDs, at most 100. Pagination still applies.", 1, 1099),
			"my_liked":           map[string]any{"type": "boolean", "description": "Only applications liked by the authenticated user."},
			"without_tags":       map[string]any{"type": "boolean", "description": "Only applications with no tags on any version."},
			"trend_start_period": stringProperty("Start of the like period, ISO timestamp."),
			"trend_end_period":   stringProperty("End of the like period, ISO timestamp. Defaults to current time."),
			"sort_by":            enumProperty("Sort field.", "created_at", "updated_at", "name", "id", "author", "authors", "likes"),
			"sort_order":         enumProperty("Sort direction.", "asc", "desc"),
		}, "project_id"),
	},
	{
		name:        "post_elitea_core_applications",
		description: "Create an agent or pipeline and its initial base version in the current project.",
		permission:  "models.applications.applications.create",
		operation:   internalCreateApplication,
		schema: objectSchema(map[string]any{
			"project_id":  intProperty("Current project ID. The server verifies this value."),
			"name":        stringProperty("Application name."),
			"description": stringProperty("Application description."),
			"type":        stringProperty("Application type."),
			"icon":        stringProperty("Optional icon."),
			"versions": map[string]any{
				"type": "array", "minItems": 1, "maxItems": 1,
				"items": versionWriteSchema(true),
			},
		}, "project_id", "name", "versions"),
	},
	{
		name: "get_elitea_core_application",
		description: "Read one agent or pipeline with its active version. If version_name is supplied, " +
			"read that named version.",
		permission: "models.applications.application.details",
		operation:  internalGetApplication,
		schema: objectSchema(map[string]any{
			"project_id":     intProperty("Current project ID. The server verifies this value."),
			"application_id": intProperty("Application ID."),
			"version_name":   stringProperty("Optional version name."),
		}, "project_id", "application_id"),
	},
	{
		name:        "post_elitea_core_versions",
		description: "Create a new draft version of an existing agent or pipeline.",
		permission:  "models.applications.versions.create",
		operation:   internalCreateVersion,
		schema: mergeObjectSchema(versionWriteSchema(true), map[string]any{
			"project_id":     intProperty("Current project ID. The server verifies this value."),
			"application_id": intProperty("Application ID."),
			"copy_skills_from_version_id": integerRangeProperty(
				"Optional source version in the same application. Copy its exact skill bindings. A missing or foreign source is ignored.", 1, 2147483647),
		}, "project_id", "application_id", "name"),
	},
	{
		name:        "get_elitea_core_version",
		description: "Read the complete configuration of one application version, including its instructions hash.",
		permission:  "models.applications.version.details",
		operation:   internalGetVersion,
		schema:      applicationVersionIdentitySchema(),
	},
	{
		name: "put_elitea_core_version",
		description: "Update non-instruction settings of one draft version. The operation creates a full " +
			"backup version first. Use the instruction-patch tool for prompt or pipeline YAML changes.",
		permission: "models.applications.version.update",
		operation:  internalUpdateVersion,
		schema: mergeObjectSchema(versionWriteSchema(false), map[string]any{
			"project_id":     intProperty("Current project ID. The server verifies this value."),
			"application_id": intProperty("Application ID."),
			"version_id":     intProperty("Version ID."),
		}, "project_id", "application_id", "version_id"),
	},
	{
		name: "post_elitea_core_version_instruction_patch",
		description: "Atomically create a full backup and patch instructions with optimistic concurrency. " +
			"Read the version immediately before this call and copy instructions_sha256.",
		permission: "models.applications.version.update",
		operation:  internalPatchInstructions,
		schema: mergeObjectSchema(applicationVersionIdentitySchema(), map[string]any{
			"expected_instructions_sha256": map[string]any{
				"type": "string", "minLength": 64, "maxLength": 64, "pattern": "^[0-9a-fA-F]{64}$",
			},
			"old_text":    stringProperty("Text that must occur exactly once unless replace_all is true."),
			"replacement": stringProperty("Replacement instructions or replacement fragment."),
			"replace_all": map[string]any{"type": "boolean", "default": false},
		}, "project_id", "application_id", "version_id", "expected_instructions_sha256", "replacement"),
	},
	{
		name:        "patch_elitea_core_application_relation",
		description: "Link or unlink a child agent version as a tool of a parent agent version.",
		permission:  "models.applications.application_relation.patch",
		operation:   internalUpdateAgentRelation,
		schema: objectSchema(map[string]any{
			"project_id":        intProperty("Current project ID. The server verifies this value."),
			"application_id":    intProperty("Child application ID."),
			"version_id":        intProperty("Child version ID."),
			"entity_id":         intProperty("Parent application ID."),
			"entity_version_id": intProperty("Parent version ID."),
			"has_relation":      map[string]any{"type": "boolean"},
		}, "project_id", "application_id", "version_id", "entity_id", "entity_version_id", "has_relation"),
	},
}

func internalApplicationTools() []Tool {
	tools := make([]Tool, 0, len(internalApplicationToolDefinitions))
	for _, definition := range internalApplicationToolDefinitions {
		tools = append(tools, Tool{
			Name:                         definition.name,
			Description:                  definition.description,
			InputSchema:                  definition.schema,
			internalApplicationOperation: definition.operation,
			permission:                   definition.permission,
		})
	}
	return tools
}

func objectSchema(properties map[string]any, required ...string) map[string]any {
	schema := map[string]any{
		"type":                 "object",
		"properties":           properties,
		"additionalProperties": false,
	}
	if len(required) > 0 {
		schema["required"] = required
	}
	return schema
}

func mergeObjectSchema(base map[string]any, additions map[string]any, required ...string) map[string]any {
	properties := make(map[string]any)
	if current, ok := base["properties"].(map[string]any); ok {
		for name, property := range current {
			properties[name] = property
		}
	}
	for name, property := range additions {
		properties[name] = property
	}
	return objectSchema(properties, required...)
}

func applicationVersionIdentitySchema() map[string]any {
	return objectSchema(map[string]any{
		"project_id":     intProperty("Current project ID. The server verifies this value."),
		"application_id": intProperty("Application ID."),
		"version_id":     intProperty("Version ID."),
	}, "project_id", "application_id", "version_id")
}

func versionWriteSchema(includeInstructions bool) map[string]any {
	properties := map[string]any{
		"name": stringProperty("Version name."),
		"agent_type": enumProperty("Agent type.",
			"react", "elitea", "dial", "openai", "codemie", "raw", "autogen", "llama", "pipeline", "xml"),
		"welcome_message":       stringProperty("Optional welcome message."),
		"llm_settings":          map[string]any{"type": "object", "additionalProperties": true},
		"conversation_starters": map[string]any{"type": "array", "maxItems": 4, "items": map[string]any{"type": "string"}},
		"variables":             map[string]any{"type": "array", "items": namedValueSchema()},
		"tags":                  map[string]any{"type": "array", "items": versionTagSchema()},
		"meta":                  map[string]any{"type": "object", "additionalProperties": true},
		"pipeline_settings":     map[string]any{"type": "object", "additionalProperties": true},
		"notes":                 map[string]any{"type": "string", "maxLength": 1000},
	}
	if includeInstructions {
		properties["instructions"] = stringProperty("Agent prompt or pipeline YAML.")
	}
	return objectSchema(properties)
}

func versionTagSchema() map[string]any {
	return objectSchema(map[string]any{
		"id":   intProperty("Existing tag ID. The server resolves tags by name."),
		"name": stringProperty("Tag name."),
		"data": map[string]any{"type": "object", "description": "Optional JSON tag metadata."},
	}, "name")
}

func namedValueSchema() map[string]any {
	return objectSchema(map[string]any{
		"name":  stringProperty("Name."),
		"value": stringProperty("Value."),
	}, "name", "value")
}

func stringProperty(description string) map[string]any {
	return map[string]any{"type": "string", "description": description}
}

func intProperty(description string) map[string]any {
	return map[string]any{"type": "integer", "minimum": 1, "description": description}
}

func integerMinProperty(description string, minimum int) map[string]any {
	return map[string]any{"type": "integer", "minimum": minimum, "description": description}
}

func integerRangeProperty(description string, minimum, maximum int) map[string]any {
	return map[string]any{"type": "integer", "minimum": minimum, "maximum": maximum, "description": description}
}

func enumProperty(description string, values ...string) map[string]any {
	return map[string]any{"type": "string", "enum": values, "description": description}
}
