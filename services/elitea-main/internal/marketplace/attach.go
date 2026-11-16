package marketplace

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"

	"github.com/jackc/pgx/v5"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/db/tenantschema"
)

// Binding binds one requirement of a listing version to the project that
// hires it: a project toolkit for a toolkit or MCP requirement, a project skill
// for a skill requirement. Disabled hires without it: the tools that need it
// stay unavailable.
type Binding struct {
	RequirementKey string `json:"requirement_key"`
	ToolkitID      int64  `json:"toolkit_id"`
	Disabled       bool   `json:"disabled"`
}

// BindingMap indexes bindings by requirement key.
func BindingMap(bindings []Binding) map[string]Binding {
	out := make(map[string]Binding, len(bindings))
	for _, binding := range bindings {
		out[binding.RequirementKey] = binding
	}
	return out
}

// AttachTools writes the tool and skill mappings of a definition onto an agent
// version of the project, through the bindings, and walks the nested agents
// that are already wired under the version. A hire runs it at once when the
// agent needs no approval, and the final approval of a hire change request runs
// it later: until then the agent holds no tool.
func AttachTools(ctx context.Context, db DB, schema string, applicationID, versionID int64, node DefinitionNode, bindings map[string]Binding) error {
	for _, tool := range node.Tools {
		binding, ok := bindings[tool.Key]
		if !ok || binding.Disabled || binding.ToolkitID <= 0 {
			continue
		}
		selected, err := json.Marshal(tool.SelectedTools)
		if err != nil {
			return fmt.Errorf("marketplace: encode the selected tools: %w", err)
		}
		if _, err := db.Exec(ctx, fmt.Sprintf(`
INSERT INTO %s.entity_tool_mapping (entity_version_id, entity_id, entity_type, tool_id, selected_tools, created_at)
VALUES ($1, $2, 'agent', $3, $4::jsonb, now() AT TIME ZONE 'UTC')
ON CONFLICT ON CONSTRAINT _entity_tool_unique DO NOTHING`, schema),
			versionID, applicationID, binding.ToolkitID, string(selected)); err != nil {
			return fmt.Errorf("marketplace: attach the toolkit %d: %w", binding.ToolkitID, err)
		}
	}
	for _, skill := range node.Skills {
		binding, ok := bindings[skill.Key]
		if !ok || binding.Disabled || binding.ToolkitID <= 0 {
			continue
		}
		if _, err := db.Exec(ctx, fmt.Sprintf(`
INSERT INTO %[1]s.entity_skill_mapping (entity_version_id, entity_type, skill_id, skill_version_id, created_at)
SELECT $1, 'agent', s.id,
       (SELECT v.id FROM %[1]s.skill_versions v WHERE v.skill_id = s.id
        ORDER BY (v.id::text = COALESCE(s.meta->>'default_version_id', '')) DESC, v.id DESC LIMIT 1),
       now() AT TIME ZONE 'UTC'
FROM %[1]s.skills s WHERE s.id = $2
ON CONFLICT ON CONSTRAINT _entity_skill_unique DO NOTHING`, schema), versionID, binding.ToolkitID); err != nil {
			return fmt.Errorf("marketplace: attach the skill %d: %w", binding.ToolkitID, err)
		}
	}
	for _, nested := range node.NestedAgents {
		var appText, versionText string
		err := db.QueryRow(ctx, fmt.Sprintf(`
SELECT COALESCE(t.settings->>'application_id', ''), COALESCE(t.settings->>'application_version_id', '')
FROM %[1]s.entity_tool_mapping m JOIN %[1]s.elitea_tools t ON t.id = m.tool_id
WHERE m.entity_version_id = $1 AND t.type = 'application' AND t.name = $2
ORDER BY m.id DESC LIMIT 1`, schema), versionID, nested.ToolName).Scan(&appText, &versionText)
		if errors.Is(err, pgx.ErrNoRows) {
			continue
		}
		if err != nil {
			return fmt.Errorf("marketplace: find the nested agent %q: %w", nested.ToolName, err)
		}
		childApp, appErr := strconv.ParseInt(appText, 10, 64)
		childVersion, versionErr := strconv.ParseInt(versionText, 10, 64)
		if appErr != nil || versionErr != nil {
			continue
		}
		if err := AttachTools(ctx, db, schema, childApp, childVersion, nested.Node, bindings); err != nil {
			return err
		}
	}
	return nil
}

// ActivateHireTx enables the tools of a hired agent in the transaction of the
// final approval of its hire change request. It answers ErrNotHireable when the
// version was yanked, withdrawn or delisted since the hire: the approvers
// approved a version that can no longer be hired.
func ActivateHireTx(ctx context.Context, tx pgx.Tx, projectID, applicationID int64) error {
	var (
		listingID, versionID int64
		bindingsRaw          []byte
		enabled              bool
	)
	err := tx.QueryRow(ctx, `
SELECT listing_id, version_id, bindings, tools_enabled FROM elitea_marketplace.installs
WHERE project_id = $1 AND entity_kind = 'agent' AND entity_id = $2 FOR UPDATE`, projectID, applicationID).
		Scan(&listingID, &versionID, &bindingsRaw, &enabled)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("marketplace: read the hire: %w", err)
	}
	if enabled {
		return nil
	}
	version, err := LoadVersion(ctx, tx, listingID, versionID)
	if err != nil {
		return err
	}
	if !version.Hireable() {
		return ErrNotHireable
	}
	node, err := ParseDefinition(version.Definition)
	if err != nil {
		return err
	}
	var bindings []Binding
	if err := json.Unmarshal(bindingsRaw, &bindings); err != nil {
		return fmt.Errorf("marketplace: read the stored bindings: %w", err)
	}
	schema, err := tenantschema.QuoteInt(projectID)
	if err != nil {
		return fmt.Errorf("marketplace: project schema: %w", err)
	}
	var agentVersionID int64
	if err := tx.QueryRow(ctx, fmt.Sprintf(`
SELECT id FROM %s.application_versions
WHERE application_id = $1 AND meta ? 'origin_listing_id' ORDER BY id LIMIT 1`, schema), applicationID).Scan(&agentVersionID); err != nil {
		return fmt.Errorf("marketplace: find the hired version: %w", err)
	}
	if err := AttachTools(ctx, tx, schema, applicationID, agentVersionID, node, BindingMap(bindings)); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `
UPDATE elitea_marketplace.installs SET tools_enabled = true, updated_at = clock_timestamp()
WHERE project_id = $1 AND entity_kind = 'agent' AND entity_id = $2`, projectID, applicationID); err != nil {
		return fmt.Errorf("marketplace: mark the tools enabled: %w", err)
	}
	return nil
}

// DetachTools removes the tool and skill mappings a set of bindings wrote, on
// the version and on the nested agents wired under it. A re-binding runs it
// with the old bindings, then AttachTools with the new ones.
func DetachTools(ctx context.Context, db DB, schema string, versionID int64, node DefinitionNode, bindings map[string]Binding) error {
	for _, tool := range node.Tools {
		binding, ok := bindings[tool.Key]
		if !ok || binding.Disabled || binding.ToolkitID <= 0 {
			continue
		}
		if _, err := db.Exec(ctx, fmt.Sprintf(
			`DELETE FROM %s.entity_tool_mapping WHERE entity_version_id = $1 AND tool_id = $2 AND entity_type = 'agent'`, schema),
			versionID, binding.ToolkitID); err != nil {
			return fmt.Errorf("marketplace: detach the toolkit %d: %w", binding.ToolkitID, err)
		}
	}
	for _, skill := range node.Skills {
		binding, ok := bindings[skill.Key]
		if !ok || binding.Disabled || binding.ToolkitID <= 0 {
			continue
		}
		if _, err := db.Exec(ctx, fmt.Sprintf(
			`DELETE FROM %s.entity_skill_mapping WHERE entity_version_id = $1 AND skill_id = $2 AND entity_type = 'agent'`, schema),
			versionID, binding.ToolkitID); err != nil {
			return fmt.Errorf("marketplace: detach the skill %d: %w", binding.ToolkitID, err)
		}
	}
	for _, nested := range node.NestedAgents {
		var versionText string
		err := db.QueryRow(ctx, fmt.Sprintf(`
SELECT COALESCE(t.settings->>'application_version_id', '')
FROM %[1]s.entity_tool_mapping m JOIN %[1]s.elitea_tools t ON t.id = m.tool_id
WHERE m.entity_version_id = $1 AND t.type = 'application' AND t.name = $2
ORDER BY m.id DESC LIMIT 1`, schema), versionID, nested.ToolName).Scan(&versionText)
		if errors.Is(err, pgx.ErrNoRows) {
			continue
		}
		if err != nil {
			return fmt.Errorf("marketplace: find the nested agent %q: %w", nested.ToolName, err)
		}
		childVersion, parseErr := strconv.ParseInt(versionText, 10, 64)
		if parseErr != nil {
			continue
		}
		if err := DetachTools(ctx, db, schema, childVersion, nested.Node, bindings); err != nil {
			return err
		}
	}
	return nil
}
