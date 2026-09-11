package mcp

const internalSkillsCategory = "elitea_core/skills"

type internalSkillOperation string

const (
	internalListSkills          internalSkillOperation = "list_skills"
	internalCreateSkill         internalSkillOperation = "create_skill"
	internalGetSkill            internalSkillOperation = "get_skill"
	internalUpdateSkill         internalSkillOperation = "update_skill"
	internalUpdateSkillRelation internalSkillOperation = "update_skill_relation"
	internalListAttachedSkills  internalSkillOperation = "list_attached_skills"
)

type internalSkillToolDefinition struct {
	name        string
	description string
	permission  string
	operation   internalSkillOperation
	schema      map[string]any
}

var internalSkillToolDefinitions = []internalSkillToolDefinition{
	{
		name:        "get_elitea_core_skills",
		description: "List skills in the current project. Filter by text and use bounded pagination.",
		permission:  "models.applications.skills.list",
		operation:   internalListSkills,
		schema: objectSchema(map[string]any{
			"project_id": intProperty("Current project ID. The server verifies this value."),
			"query":      stringProperty("Optional name or description search."),
			"limit":      integerRangeProperty("Maximum results; requested IDs can increase this to their count.", 1, 1000),
			"offset":     integerRangeProperty("Results to skip.", 0, 100000),
			"ids":        stringProperty("Comma-separated skill IDs, at most 100."),
			"tags":       stringProperty("Comma-separated tag IDs. Every requested tag must occur on a skill version."),
			"author_id":  intProperty("Match an author of any skill version."),
			"statuses":   stringProperty("Comma-separated version statuses. Unknown statuses are ignored."),
			"page":       integerMinProperty("Page number.", 1),
			"page_size":  integerRangeProperty("Maximum results per page.", 1, 100),
			"sort_by":    enumProperty("Sort field.", "created_at", "name"),
			"sort_order": enumProperty("Sort direction.", "asc", "desc"),
		}, "project_id"),
	},
	{
		name:        "post_elitea_core_skills",
		description: "Create one skill with one required base version.",
		permission:  "models.applications.skills.create",
		operation:   internalCreateSkill,
		schema: objectSchema(map[string]any{
			"project_id":  intProperty("Current project ID. The server verifies this value."),
			"name":        skillNameProperty("Skill name."),
			"description": boundedStringProperty("Skill description.", 1, 2304),
			"versions": map[string]any{
				"type": "array", "minItems": 1, "maxItems": 1,
				"items": skillVersionWriteSchema(true),
			},
		}, "project_id", "name", "description", "versions"),
	},
	{
		name:        "get_elitea_core_skill",
		description: "Read one skill with the requested version, or its configured default version when version_id is omitted.",
		permission:  "models.applications.skills.details",
		operation:   internalGetSkill,
		schema: mergeObjectSchema(skillIdentitySchema(), map[string]any{
			"version_id": intProperty("Optional version ID belonging to this skill."),
		}, "project_id", "skill_id"),
	},
	{
		name:        "put_elitea_core_skill",
		description: "Update metadata and nested version content. version.id selects the version; omission selects its default. With version_id, use flat version fields.",
		permission:  "models.applications.skills.update",
		operation:   internalUpdateSkill,
		schema: mergeObjectSchema(skillVersionWriteSchema(false), map[string]any{
			"project_id":   intProperty("Current project ID. The server verifies this value."),
			"skill_id":     intProperty("Skill ID."),
			"description":  boundedStringProperty("Optional skill description.", 1, 2304),
			"version_id":   intProperty("Update this version with flat name, instructions, tags, and meta fields."),
			"instructions": boundedStringProperty("Version instructions; requires version_id.", 1, 5000),
			"tags":         map[string]any{"type": "array", "items": skillTagWriteSchema()},
			"meta":         map[string]any{"type": "object"},
			"name":         boundedStringProperty("Skill name, or version name when version_id is provided.", 1, 128),
		}, "project_id", "skill_id"),
	},
	{
		name:        "patch_elitea_core_skill",
		description: "Link or unlink one skill version from an agent version.",
		permission:  "models.applications.skills.update",
		operation:   internalUpdateSkillRelation,
		schema: objectSchema(map[string]any{
			"project_id":        intProperty("Current project ID. The server verifies this value."),
			"skill_id":          intProperty("Skill ID."),
			"entity_version_id": intProperty("Agent version ID."),
			"entity_type":       enumProperty("Entity type.", "agent"),
			"has_relation":      map[string]any{"type": "boolean"},
			"skill_version_id":  intProperty("Skill version ID. This value is required when linking."),
		}, "project_id", "skill_id", "entity_version_id", "has_relation"),
	},
	{
		name:        "get_elitea_core_application_skills",
		description: "List skills attached to one agent version.",
		permission:  "models.applications.applications.details",
		operation:   internalListAttachedSkills,
		schema: objectSchema(map[string]any{
			"project_id":     intProperty("Current project ID. The server verifies this value."),
			"app_version_id": intProperty("Agent version ID."),
		}, "project_id", "app_version_id"),
	},
}

func internalSkillTools() []Tool {
	tools := make([]Tool, 0, len(internalSkillToolDefinitions))
	for _, definition := range internalSkillToolDefinitions {
		tools = append(tools, Tool{
			Name:                   definition.name,
			Description:            definition.description,
			InputSchema:            definition.schema,
			internalSkillOperation: definition.operation,
			permission:             definition.permission,
		})
	}
	return tools
}

func skillIdentitySchema() map[string]any {
	return objectSchema(map[string]any{
		"project_id": intProperty("Current project ID. The server verifies this value."),
		"skill_id":   intProperty("Skill ID."),
	}, "project_id", "skill_id")
}

func skillVersionWriteSchema(required bool) map[string]any {
	tags := map[string]any{
		"type":  "array",
		"items": skillTagWriteSchema(),
	}
	properties := map[string]any{
		"version": objectSchema(map[string]any{
			"id":           intProperty("Existing version ID; omission selects the default version."),
			"name":         boundedStringProperty("Version name. The base version cannot be renamed.", 1, 128),
			"instructions": boundedStringProperty("Skill instructions.", 1, 5000),
			"tags":         tags,
			"meta":         map[string]any{"type": "object"},
		}),
	}
	if required {
		properties = map[string]any{
			"name":         map[string]any{"type": "string", "const": "base"},
			"instructions": boundedStringProperty("Skill instructions.", 1, 5000),
			"tags":         tags,
		}
		return objectSchema(properties, "name", "instructions")
	}
	return objectSchema(properties)
}

func skillTagWriteSchema() map[string]any {
	return objectSchema(map[string]any{
		"name": boundedStringProperty("Tag name.", 1, 128),
	}, "name")
}

func skillNameProperty(description string) map[string]any {
	return map[string]any{
		"type":        "string",
		"minLength":   1,
		"maxLength":   64,
		"pattern":     `^[a-z0-9]$|^[a-z0-9][a-z0-9-]*[a-z0-9]$`,
		"not":         map[string]any{"pattern": "claude|anthropic"},
		"description": description,
	}
}

func boundedStringProperty(description string, minimum, maximum int) map[string]any {
	return map[string]any{
		"type": "string", "minLength": minimum, "maxLength": maximum, "description": description,
	}
}
