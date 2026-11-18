package marketplace

import (
	"context"
	"encoding/json"
	"fmt"
)

// ReadOnlyCode is the error code of a refused edit of an installed agent's
// definition (ADR-0051 decision 3).
const ReadOnlyCode = "installed_definition_read_only"

// ReadOnlyMessage is what the refusal says.
const ReadOnlyMessage = "This agent was installed from the Marketplace. Its definition is read-only. Fork it to change it."

// DefinitionKeys are the keys of a version save that change an agent's
// definition. An installed agent refuses a save that carries any of them. The
// name, description, icon, tags, folder, bindings, lead role and proactivity
// stay editable.
var DefinitionKeys = []string{
	"instructions", "welcome_message", "agent_type", "llm_settings", "conversation_starters",
	"pipeline_settings", "soul",
}

// DefinitionChanged reports whether a version save changes the definition of an
// installed agent: a definition key whose value differs from the stored one. A
// save that repeats the stored values (the editor sends the whole form) is not
// a change. An unreadable version answers false.
func DefinitionChanged(ctx context.Context, db DB, schema string, versionID any, body map[string]any) bool {
	var instructions, welcome, agentType string
	var llm, starters, pipeline, soul *string
	err := db.QueryRow(ctx, fmt.Sprintf(`
SELECT COALESCE(instructions, ''), COALESCE(welcome_message, ''), COALESCE(agent_type, ''),
       llm_settings::text, conversation_starters::text, pipeline_settings::text, soul::text
FROM %s.application_versions WHERE id = $1`, schema), versionID).
		Scan(&instructions, &welcome, &agentType, &llm, &starters, &pipeline, &soul)
	if err != nil {
		return false
	}
	decode := func(text *string) any {
		var out any
		if text != nil {
			_ = json.Unmarshal([]byte(*text), &out) // a malformed column reads as null
		}
		return out
	}
	stored := map[string]any{
		"instructions": instructions, "welcome_message": welcome, "agent_type": agentType,
		"llm_settings": decode(llm), "conversation_starters": decode(starters),
		"pipeline_settings": decode(pipeline), "soul": decode(soul),
	}
	for _, key := range DefinitionKeys {
		sent, present := body[key]
		if !present {
			continue
		}
		if !jsonEqual(sent, stored[key]) {
			return true
		}
	}
	return false
}

// jsonEqual compares two decoded JSON values by their canonical encoding, so an
// empty object and a missing one, and number types, do not differ by accident.
func jsonEqual(a, b any) bool {
	normalise := func(v any) string {
		if v == nil {
			return "null"
		}
		if text, ok := v.(string); ok {
			// A soul may arrive as a JSON string of the document.
			var inner any
			if json.Unmarshal([]byte(text), &inner) == nil {
				if _, isObject := inner.(map[string]any); isObject {
					v = inner
				}
			}
		}
		out, err := json.Marshal(v)
		if err != nil {
			return ""
		}
		return string(out)
	}
	left, right := normalise(a), normalise(b)
	return left == right || (isEmptyJSON(left) && isEmptyJSON(right))
}

func isEmptyJSON(text string) bool {
	return text == "null" || text == "{}" || text == "[]" || text == `""`
}

// IsInstalledVersion reports whether an agent version belongs to an agent
// installed from the Marketplace. schema is the quoted tenant schema. A version
// that cannot be read is not installed: the write that follows names no row.
func IsInstalledVersion(ctx context.Context, db DB, schema string, versionID any) bool {
	var installed bool
	err := db.QueryRow(ctx, fmt.Sprintf(
		`SELECT COALESCE(meta->>'origin_kind', '') = 'installed' FROM %s.application_versions WHERE id = $1`, schema),
		versionID).Scan(&installed)
	return err == nil && installed
}

// IsInstalledApplication reports whether an agent was installed from the
// Marketplace, by any of its versions.
func IsInstalledApplication(ctx context.Context, db DB, schema string, applicationID any) bool {
	var installed bool
	err := db.QueryRow(ctx, fmt.Sprintf(
		`SELECT EXISTS (SELECT 1 FROM %s.application_versions
		                WHERE application_id = $1 AND COALESCE(meta->>'origin_kind', '') = 'installed')`, schema),
		applicationID).Scan(&installed)
	return err == nil && installed
}
