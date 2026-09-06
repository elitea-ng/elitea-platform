package eliteacore

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strings"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/ownership"
)

// The reads and the encoders that the export and the import paths share.
//
// Both paths had one shape of defect, reported as issue #505 and named in pull
// request #499: a failure became a success, and the caller could not tell.
// The export answered 200 with a document that was missing whatever the failed
// read held. The import answered 201 with a row that was missing whatever the
// failed encode held. Neither said anything.
//
// The helpers here exist so that each failure has exactly one place to be
// reported from, and so that a reader can see the whole set at once.

// exportReadFailed is the message the export answers with when a read is lost.
//
// The export is a BACKUP. A document that is missing a version, a toolkit, a
// variable or a tag is worse than no document at all, because the operator
// keeps it, deletes the original and finds out later. So a lost read refuses
// the whole export rather than serving what it managed to collect. This is the
// rule #439 applied to `AvailableTools`: no rows gives a document with no rows,
// a lost read gives 500 and a named reason.
const exportReadFailed = "unable to read the application to export"

// exportedToolkits reads the toolkits that any version of one application uses.
//
// It returns the export entries and the tool_id-to-import_uuid map that the
// version entries reference. All three faults #439 names are reported: the
// query, each row's scan, and the fault that `rows.Next` reports at the end
// through `rows.Err` — a connection lost half way through a result set stops
// the loop and looks exactly like the end of the rows.
func (h *Handler) exportedToolkits(ctx context.Context, schema, applicationID string) ([]map[string]any, map[int]string, error) {
	rows, err := h.pool.Query(ctx, fmt.Sprintf(`
		SELECT DISTINCT t.id, t.name, t.type, COALESCE(t.settings::text, '{}')
		FROM %s.entity_tool_mapping etm
		JOIN %s.elitea_tools t ON t.id = etm.tool_id
		JOIN %s.application_versions av ON av.id = etm.entity_version_id
		WHERE av.application_id = $1`, schema, schema, schema), applicationID)
	if err != nil {
		return nil, nil, fmt.Errorf("query the toolkits of application %q in %q: %w", applicationID, schema, err)
	}
	defer rows.Close()

	toolkitMap := map[int]string{}
	toolkits := make([]map[string]any, 0)
	for rows.Next() {
		var toolID int
		var name, toolType, settingsText string
		if err := rows.Scan(&toolID, &name, &toolType, &settingsText); err != nil {
			return nil, nil, fmt.Errorf("scan a toolkit of application %q in %q: %w", applicationID, schema, err)
		}
		var stored map[string]any
		if err := json.Unmarshal([]byte(settingsText), &stored); err != nil {
			return nil, nil, fmt.Errorf("decode the settings of toolkit %d in %q: %w", toolID, schema, err)
		}
		if stored == nil {
			stored = map[string]any{}
		}
		// Strip sensitive settings for export.
		settings := map[string]any{}
		for key, value := range stored {
			settings[key] = value
		}
		for _, secret := range []string{"api_key", "access_token", "token", "api_key_type",
			"client_secret", "gitlab_personal_access_token", "private_token",
			"sonar_token", "qtest_api_token", "client_id",
			"password", "secret", "app_id"} {
			delete(settings, secret)
		}
		importUUID := fmt.Sprintf("tool-%d", toolID)
		toolkitMap[toolID] = importUUID
		toolkits = append(toolkits, map[string]any{
			"id": toolID, "name": name, "type": toolType,
			"import_uuid": importUUID, "settings": settings,
		})
	}
	if err := rows.Err(); err != nil {
		return nil, nil, fmt.Errorf("read the toolkits of application %q in %q: %w", applicationID, schema, err)
	}
	return toolkits, toolkitMap, nil
}

// exportedVersionRow is one row of the version query, held until the result set
// is closed.
//
// The rows are collected before the per-version reads run. The previous code
// ran four more queries against the same pool while this result set was still
// open, which needs a second connection for every version and deadlocks a pool
// of one. Collecting first costs one slice and removes that dependency.
type exportedVersionRow struct {
	id            int
	applicationID int
	authorID      int
	name          string
	status        string
	agentType     string
	instructions  string
	welcomeMsg    string
	llmText       string
	startersText  string
	metaText      string
	uuid          string
}

// exportedVersions reads every version of one application, with its tools,
// variables, tags and skill attachments.
func (h *Handler) exportedVersions(
	ctx context.Context, schema, applicationID string, toolkitMap map[int]string,
) ([]map[string]any, error) {
	collected, err := h.exportedVersionRows(ctx, schema, applicationID)
	if err != nil {
		return nil, err
	}

	versions := make([]map[string]any, 0, len(collected))
	for _, row := range collected {
		var llm, starters, meta any
		if err := json.Unmarshal([]byte(row.llmText), &llm); err != nil {
			return nil, fmt.Errorf("decode llm_settings of version %d in %q: %w", row.id, schema, err)
		}
		if err := json.Unmarshal([]byte(row.startersText), &starters); err != nil {
			return nil, fmt.Errorf("decode conversation_starters of version %d in %q: %w", row.id, schema, err)
		}
		if err := json.Unmarshal([]byte(row.metaText), &meta); err != nil {
			return nil, fmt.Errorf("decode meta of version %d in %q: %w", row.id, schema, err)
		}

		// Ensure meta has icon_meta.
		metaMap, isMetaMap := meta.(map[string]any)
		if isMetaMap {
			if _, hasIcon := metaMap["icon_meta"]; !hasIcon {
				metaMap["icon_meta"] = map[string]any{}
			}
		} else {
			metaMap = map[string]any{"icon_meta": map[string]any{}}
			meta = metaMap
		}

		// Ensure llm_settings.model_project_id is a string.
		if llmMap, ok := llm.(map[string]any); ok {
			if projectID, exists := llmMap["model_project_id"]; exists {
				if numeric, ok := projectID.(float64); ok {
					llmMap["model_project_id"] = fmt.Sprintf("%d", int(numeric))
				}
			}
		}

		tools, err := h.exportedVersionTools(ctx, schema, row.id, toolkitMap)
		if err != nil {
			return nil, err
		}
		variables, err := h.exportedVersionVariables(ctx, schema, row.id)
		if err != nil {
			return nil, err
		}
		tags, err := h.exportedVersionTags(ctx, schema, row.id)
		if err != nil {
			return nil, err
		}
		skills, err := h.exportedVersionSkills(ctx, schema, row.id)
		if err != nil {
			return nil, err
		}

		_, isForked := metaMap["parent_entity_id"]

		entry := map[string]any{
			"id": fmt.Sprintf("%d", row.id), "name": row.name, "status": row.status,
			"application_id": fmt.Sprintf("%d", row.applicationID),
			"author_id":      fmt.Sprintf("%d", row.authorID),
			"agent_type":     row.agentType, "instructions": row.instructions,
			"welcome_message": row.welcomeMsg, "llm_settings": llm,
			"conversation_starters": starters, "meta": meta,
			"tools": tools, "variables": variables, "tags": tags,
			"skills": skills, "is_forked": isForked,
		}
		if row.uuid != "" {
			entry["import_version_uuid"] = row.uuid
		}
		versions = append(versions, entry)
	}
	return versions, nil
}

func (h *Handler) exportedVersionRows(ctx context.Context, schema, applicationID string) ([]exportedVersionRow, error) {
	rows, err := h.pool.Query(ctx, fmt.Sprintf(`
		SELECT id, name, status, COALESCE(agent_type, 'openai'),
			COALESCE(instructions, ''), COALESCE(welcome_message, ''),
			COALESCE(llm_settings::text, '{}'), COALESCE(conversation_starters::text, '[]'),
			COALESCE(meta::text, '{}'), COALESCE(uuid::text, ''), application_id, COALESCE(author_id, 0)
		FROM %s.application_versions WHERE application_id = $1
		ORDER BY created_at`, schema), applicationID)
	if err != nil {
		return nil, fmt.Errorf("query the versions of application %q in %q: %w", applicationID, schema, err)
	}
	defer rows.Close()

	collected := make([]exportedVersionRow, 0)
	for rows.Next() {
		var row exportedVersionRow
		if err := rows.Scan(&row.id, &row.name, &row.status, &row.agentType,
			&row.instructions, &row.welcomeMsg, &row.llmText, &row.startersText,
			&row.metaText, &row.uuid, &row.applicationID, &row.authorID); err != nil {
			return nil, fmt.Errorf("scan a version of application %q in %q: %w", applicationID, schema, err)
		}
		collected = append(collected, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read the versions of application %q in %q: %w", applicationID, schema, err)
	}
	return collected, nil
}

// exportedVersionTools reads one version's toolkit references.
//
// `COALESCE(selected_tools::text, '[]')` and not `'{}'`: the column's own
// default is `'[]'::jsonb` (internal/infra/db/migrations/001_initial.sql), the
// route that writes a selection writes a JSON array of tool names
// (internal/api/v2/toolkits/handler.go, selectedToolsPayload), and the chat
// read only understands an array (internal/db/queries/agent_chat.sql). A NULL
// column that exported as `{}` put a value in the file that no reader of the
// file can use.
func (h *Handler) exportedVersionTools(
	ctx context.Context, schema string, versionID int, toolkitMap map[int]string,
) ([]map[string]any, error) {
	rows, err := h.pool.Query(ctx, fmt.Sprintf(`
		SELECT tool_id, COALESCE(selected_tools::text, '[]')
		FROM %s.entity_tool_mapping WHERE entity_version_id = $1`, schema), versionID)
	if err != nil {
		return nil, fmt.Errorf("query the toolkit references of version %d in %q: %w", versionID, schema, err)
	}
	defer rows.Close()

	tools := make([]map[string]any, 0)
	for rows.Next() {
		var toolID int
		var selectedText string
		if err := rows.Scan(&toolID, &selectedText); err != nil {
			return nil, fmt.Errorf("scan a toolkit reference of version %d in %q: %w", versionID, schema, err)
		}
		var selected any
		if err := json.Unmarshal([]byte(selectedText), &selected); err != nil {
			return nil, fmt.Errorf("decode selected_tools of version %d in %q: %w", versionID, schema, err)
		}
		tools = append(tools, map[string]any{
			"import_uuid":    toolkitMap[toolID],
			"selected_tools": selected,
		})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read the toolkit references of version %d in %q: %w", versionID, schema, err)
	}
	return tools, nil
}

func (h *Handler) exportedVersionVariables(ctx context.Context, schema string, versionID int) ([]map[string]any, error) {
	rows, err := h.pool.Query(ctx, fmt.Sprintf(`
		SELECT name, COALESCE(value, '') FROM %s.application_variables
		WHERE application_version_id = $1 ORDER BY id`, schema), versionID)
	if err != nil {
		return nil, fmt.Errorf("query the variables of version %d in %q: %w", versionID, schema, err)
	}
	defer rows.Close()

	variables := make([]map[string]any, 0)
	for rows.Next() {
		var name, value string
		if err := rows.Scan(&name, &value); err != nil {
			return nil, fmt.Errorf("scan a variable of version %d in %q: %w", versionID, schema, err)
		}
		variables = append(variables, map[string]any{"name": name, "value": value})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read the variables of version %d in %q: %w", versionID, schema, err)
	}
	return variables, nil
}

func (h *Handler) exportedVersionTags(ctx context.Context, schema string, versionID int) ([]map[string]any, error) {
	rows, err := h.pool.Query(ctx, fmt.Sprintf(`
		SELECT t.name, COALESCE(t.data::text, '{}')
		FROM %s.application_version_tag_association vta
		JOIN %s.tags t ON t.id = vta.tag_id
		WHERE vta.version_id = $1`, schema, schema), versionID)
	if err != nil {
		return nil, fmt.Errorf("query the tags of version %d in %q: %w", versionID, schema, err)
	}
	defer rows.Close()

	tags := make([]map[string]any, 0)
	for rows.Next() {
		var name, dataText string
		if err := rows.Scan(&name, &dataText); err != nil {
			return nil, fmt.Errorf("scan a tag of version %d in %q: %w", versionID, schema, err)
		}
		var data any
		if err := json.Unmarshal([]byte(dataText), &data); err != nil {
			return nil, fmt.Errorf("decode the data of a tag of version %d in %q: %w", versionID, schema, err)
		}
		if data == nil {
			data = map[string]any{}
		}
		tags = append(tags, map[string]any{"name": name, "data": data})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read the tags of version %d in %q: %w", versionID, schema, err)
	}
	return tags, nil
}

// ── the skill attachments ───────────────────────────────────────────────────
//
// Five paths copy an agent version. Publish (handler.go, `Publish`) and the
// embed under it (`embedSubAgentsRecursive`) carried the rows of
// `entity_skill_mapping`. The export, the import and the fork did not, so an
// agent that went through any of those three came back with every skill gone,
// with no message on the response, on the log or in the wizard (#611).
//
// The document shape is pylon's, because the same files move between the two
// implementations: a version carries a `skills` array of REFERENCES, and the
// document carries one top-level `skills` array with the content of each
// referenced skill (legacy/plugins/elitea_core/utils/export_import.py,
// `_export_skills_main`). A reference names a skill by `import_uuid` and a
// version by `version_name`. It cannot name them by id: `skills.id` is local to
// the project the file came from, and an import writes into a different one.
//
// The reference also carries `entity_type`, because that column is part of the
// key the table is unique on and part of the predicate the chat read matches on
// (internal/db/queries/agent_chat.sql:132). A copy that defaulted it would write
// rows that nothing reads.

// exportedSkillReference is the placeholder key that `exportedVersionSkills`
// writes and `exportedSkills` replaces.
//
// The version read runs before the skill read, so it knows the `skill_id` and
// not yet the `import_uuid` the document must carry. It writes the id under
// this key, and the skill read swaps it for the uuid once it has read the
// skills. The key never reaches a response: `exportedSkills` deletes every one
// of them, and refuses the export if it finds a reference it cannot resolve.
const exportedSkillReference = "skill_id"

// exportedVersionSkills reads one version's skill attachments.
func (h *Handler) exportedVersionSkills(ctx context.Context, schema string, versionID int) ([]map[string]any, error) {
	rows, err := h.pool.Query(ctx, fmt.Sprintf(`
		SELECT mapping.skill_id, mapping.entity_type, COALESCE(version.name, '')
		FROM %s.entity_skill_mapping AS mapping
		LEFT JOIN %s.skill_versions AS version ON version.id = mapping.skill_version_id
		WHERE mapping.entity_version_id = $1
		ORDER BY mapping.id`, schema, schema), versionID)
	if err != nil {
		return nil, fmt.Errorf("query the skill attachments of version %d in %q: %w", versionID, schema, err)
	}
	defer rows.Close()

	references := make([]map[string]any, 0)
	for rows.Next() {
		var skillID int
		var entityType, versionName string
		if err := rows.Scan(&skillID, &entityType, &versionName); err != nil {
			return nil, fmt.Errorf("scan a skill attachment of version %d in %q: %w", versionID, schema, err)
		}
		reference := map[string]any{
			exportedSkillReference: skillID,
			"entity_type":          entityType,
		}
		// `skill_version_id` is nullable, so an attachment can name no version
		// at all. The key is left off rather than written empty: an absent key
		// says "this file names no version", which is what the row holds, and
		// the import resolves it to the skill's own base version.
		if versionName != "" {
			reference["version_name"] = versionName
		}
		references = append(references, reference)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read the skill attachments of version %d in %q: %w", versionID, schema, err)
	}
	return references, nil
}

// exportedSkills builds the document's top-level `skills` array and rewrites
// every version reference to name a skill by its `import_uuid`.
//
// It runs over the version entries the document will actually carry, and so
// AFTER the fork branch has dropped the versions it does not export. A skill
// that only a dropped version used is then not in the array, and the array
// never names a skill no reference points at.
func (h *Handler) exportedSkills(ctx context.Context, schema string, versions []map[string]any) ([]map[string]any, error) {
	references, selectedVersions := collectedSkillReferences(versions)
	if len(references) == 0 {
		return nil, nil
	}

	skillIDs := make([]int, 0, len(references))
	for skillID := range references {
		skillIDs = append(skillIDs, skillID)
	}
	sort.Ints(skillIDs)

	documents, uuidByID, err := h.exportedSkillDocuments(ctx, schema, skillIDs, selectedVersions)
	if err != nil {
		return nil, err
	}
	for skillID, entries := range references {
		importUUID, resolved := uuidByID[skillID]
		if !resolved {
			// Unreachable while the foreign key holds: `skill_id` REFERENCES
			// `skills(id) ON DELETE CASCADE` (001_initial.sql:422-432), so a
			// deleted skill takes its attachments with it. It is refused rather
			// than dropped, because dropping it is the defect this whole file
			// repairs: the export would answer 200 with an agent that has lost
			// a skill.
			return nil, fmt.Errorf("skill %d of %q is attached to a version and has no row", skillID, schema)
		}
		for _, reference := range entries {
			delete(reference, exportedSkillReference)
			reference["import_uuid"] = importUUID
		}
	}
	return documents, nil
}

// collectedSkillReferences groups every version reference by the skill it
// names, and collects the version names those references use.
//
// Only the named versions are exported. A skill's unused version history says
// nothing about the agent in the file, and importing it would create versions
// in the destination project that nothing points at
// (export_import.py:266-274).
func collectedSkillReferences(versions []map[string]any) (map[int][]map[string]any, map[int]map[string]bool) {
	references := map[int][]map[string]any{}
	selectedVersions := map[int]map[string]bool{}
	for _, version := range versions {
		entries, ok := version["skills"].([]map[string]any)
		if !ok {
			continue
		}
		for _, reference := range entries {
			skillID, ok := reference[exportedSkillReference].(int)
			if !ok {
				continue
			}
			references[skillID] = append(references[skillID], reference)
			if versionName, named := reference["version_name"].(string); named && versionName != "" {
				if selectedVersions[skillID] == nil {
					selectedVersions[skillID] = map[string]bool{}
				}
				selectedVersions[skillID][versionName] = true
			}
		}
	}
	return references, selectedVersions
}

// exportedSkillDocuments reads the named skills with their versions and the
// tags of those versions.
//
// It returns the document entries and the skill_id-to-import_uuid map the
// references need. Every fault is reported, for the reason `exportedToolkits`
// states: a skill this read loses is a skill the imported agent does not have.
func (h *Handler) exportedSkillDocuments(
	ctx context.Context, schema string, skillIDs []int, selectedVersions map[int]map[string]bool,
) ([]map[string]any, map[int]string, error) {
	versionsBySkill, versionIDs, err := h.exportedSkillVersions(ctx, schema, skillIDs, selectedVersions)
	if err != nil {
		return nil, nil, err
	}
	tagsByVersion, err := h.exportedSkillVersionTags(ctx, schema, versionIDs)
	if err != nil {
		return nil, nil, err
	}
	for _, entries := range versionsBySkill {
		for _, entry := range entries {
			versionID, _ := entry["id"].(int)
			tags := tagsByVersion[versionID]
			if tags == nil {
				tags = make([]map[string]any, 0)
			}
			entry["tags"] = tags
		}
	}

	rows, err := h.pool.Query(ctx, fmt.Sprintf(`
		SELECT id, name, COALESCE(description, ''), COALESCE(uuid::text, ''),
			COALESCE(meta::text, '{}'), owner_id
		FROM %s.skills WHERE id = ANY($1) ORDER BY id`, schema), skillIDs)
	if err != nil {
		return nil, nil, fmt.Errorf("query the attached skills of %q: %w", schema, err)
	}
	defer rows.Close()

	documents := make([]map[string]any, 0, len(skillIDs))
	uuidByID := map[int]string{}
	for rows.Next() {
		var skillID, ownerID int
		var name, description, skillUUID, metaText string
		if err := rows.Scan(&skillID, &name, &description, &skillUUID, &metaText, &ownerID); err != nil {
			return nil, nil, fmt.Errorf("scan an attached skill of %q: %w", schema, err)
		}
		var meta any
		if err := json.Unmarshal([]byte(metaText), &meta); err != nil {
			return nil, nil, fmt.Errorf("decode the meta of skill %d in %q: %w", skillID, schema, err)
		}
		if meta == nil {
			meta = map[string]any{}
		}
		// `skills.uuid` has a default but no NOT NULL, so a row written before
		// that default existed can hold none. `skill-<id>` is then the key, in
		// the idiom `exportedToolkits` already uses for a toolkit. It is unique
		// inside one document, which is all a reference needs.
		importUUID := skillUUID
		if importUUID == "" {
			importUUID = fmt.Sprintf("skill-%d", skillID)
		}
		uuidByID[skillID] = importUUID
		documents = append(documents, map[string]any{
			// `entity` is what the import wizard sets on every entry it sends
			// (apps/elitea-ui .../importWizardParser.helpers.js,
			// prepareImportWizardData, which reads the key of each top-level
			// array). The export stamps it as well, so a file sent to the
			// import route unchanged names its own entities.
			"entity":      "skills",
			"id":          fmt.Sprintf("%d", skillID),
			"import_uuid": importUUID,
			"name":        name,
			"description": description,
			"owner_id":    fmt.Sprintf("%d", ownerID),
			"meta":        meta,
			"versions":    versionsBySkill[skillID],
		})
	}
	if err := rows.Err(); err != nil {
		return nil, nil, fmt.Errorf("read the attached skills of %q: %w", schema, err)
	}
	return documents, uuidByID, nil
}

// exportedSkillVersions reads the versions of the named skills, keeping only
// the versions the attachments name.
func (h *Handler) exportedSkillVersions(
	ctx context.Context, schema string, skillIDs []int, selectedVersions map[int]map[string]bool,
) (map[int][]map[string]any, []int, error) {
	// `status` is deliberately not selected. It says whether this project holds
	// a twin of the version in the public project, which is true of the project
	// the file came FROM and can never be true of the project it is imported
	// into. Pylon leaves it out of the export model for the same reason
	// (legacy/plugins/elitea_core/models/pd/skill_version.py,
	// SkillVersionExportModel).
	rows, err := h.pool.Query(ctx, fmt.Sprintf(`
		SELECT id, skill_id, name, instructions, COALESCE(meta::text, '{}'), author_id
		FROM %s.skill_versions WHERE skill_id = ANY($1) ORDER BY skill_id, id`, schema), skillIDs)
	if err != nil {
		return nil, nil, fmt.Errorf("query the versions of the attached skills of %q: %w", schema, err)
	}
	defer rows.Close()

	all := map[int][]map[string]any{}
	for rows.Next() {
		var versionID, skillID, authorID int
		var name, instructions, metaText string
		if err := rows.Scan(&versionID, &skillID, &name, &instructions, &metaText, &authorID); err != nil {
			return nil, nil, fmt.Errorf("scan a version of an attached skill of %q: %w", schema, err)
		}
		var meta any
		if err := json.Unmarshal([]byte(metaText), &meta); err != nil {
			return nil, nil, fmt.Errorf("decode the meta of skill version %d in %q: %w", versionID, schema, err)
		}
		if meta == nil {
			meta = map[string]any{}
		}
		all[skillID] = append(all[skillID], map[string]any{
			"id": versionID, "name": name, "instructions": instructions,
			"author_id": authorID, "meta": meta,
		})
	}
	if err := rows.Err(); err != nil {
		return nil, nil, fmt.Errorf("read the versions of the attached skills of %q: %w", schema, err)
	}

	kept := map[int][]map[string]any{}
	versionIDs := make([]int, 0)
	for _, skillID := range skillIDs {
		entries := all[skillID]
		selected := selectedVersions[skillID]
		chosen := make([]map[string]any, 0, len(entries))
		for _, entry := range entries {
			name, _ := entry["name"].(string)
			if len(selected) == 0 || selected[name] {
				chosen = append(chosen, entry)
			}
		}
		// A selection that matches nothing keeps the whole history rather than
		// exporting a skill with fewer versions than it has: the reference would
		// then fail on import for a reason that is the export's fault
		// (export_import.py:293-302).
		//
		// It does NOT rescue a skill that holds no version at all. Nothing
		// requires `skills` to have a `skill_versions` row, and
		// `entity_skill_mapping.skill_version_id` is nullable, so an agent can be
		// attached to such a skill. `entries` is then nil, and assigning it
		// straight through put a JSON `null` in the document where every sibling
		// array — tools, variables, tags — writes `[]`. The empty slice keeps the
		// document's one shape. The import still refuses the entry, and says so:
		// a skill with no instructions is a skill nothing attached to it can run.
		if len(chosen) == 0 {
			chosen = entries
		}
		if chosen == nil {
			chosen = make([]map[string]any, 0)
		}
		kept[skillID] = chosen
		for _, entry := range chosen {
			if versionID, ok := entry["id"].(int); ok {
				versionIDs = append(versionIDs, versionID)
			}
		}
	}
	return kept, versionIDs, nil
}

// exportedSkillVersionTags reads the tags of the exported skill versions.
func (h *Handler) exportedSkillVersionTags(
	ctx context.Context, schema string, versionIDs []int,
) (map[int][]map[string]any, error) {
	byVersion := map[int][]map[string]any{}
	if len(versionIDs) == 0 {
		return byVersion, nil
	}
	rows, err := h.pool.Query(ctx, fmt.Sprintf(`
		SELECT association.version_id, tag.name, COALESCE(tag.data::text, '{}')
		FROM %s.skill_version_tag_association AS association
		JOIN %s.tags AS tag ON tag.id = association.tag_id
		WHERE association.version_id = ANY($1)
		ORDER BY association.version_id, tag.id`, schema, schema), versionIDs)
	if err != nil {
		return nil, fmt.Errorf("query the tags of the attached skills of %q: %w", schema, err)
	}
	defer rows.Close()

	for rows.Next() {
		var versionID int
		var name, dataText string
		if err := rows.Scan(&versionID, &name, &dataText); err != nil {
			return nil, fmt.Errorf("scan a tag of an attached skill of %q: %w", schema, err)
		}
		var data any
		if err := json.Unmarshal([]byte(dataText), &data); err != nil {
			return nil, fmt.Errorf("decode the data of a skill tag in %q: %w", schema, err)
		}
		if data == nil {
			data = map[string]any{}
		}
		byVersion[versionID] = append(byVersion[versionID], map[string]any{"name": name, "data": data})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read the tags of the attached skills of %q: %w", schema, err)
	}
	return byVersion, nil
}

// ── the import side ─────────────────────────────────────────────────────────

// importWriteFailed is the message the import and the fork answer with when
// they cannot reach a database at all.
const importWriteFailed = "the import cannot run without a database connection"

// importChannels builds one `result` or one `errors` envelope of an import or a
// fork answer.
//
// # Every answer carries every channel
//
// The wizard reads `result[item.entity]` for EVERY row it sent, and it reads it
// with no guard:
//
//	const foundInResult = result[item.entity].find(it => it.index === index);
//
// (apps/elitea-ui .../ImportWizardModal/IWModalForkButton.jsx). A channel the
// answer leaves out is `undefined` there, `.find` on it raises a TypeError, and
// the raise happens inside an async map callback. The `Promise.all` that awaits
// the callbacks rejects, its `.then` never runs, and NEITHER the error toast
// NOR the success branch fires: the user forks an agent that has toolkits and
// the dialog stops with no message at all. The same line reads
// `errors[item.entity]` through `getErrorImportUUID`, so both envelopes need
// the same treatment.
//
// The set is the legacy set. `rpc/import_wizzard.py` seeds `result[key] = []`
// and `errors[key] = []` for every key of ENTITY_IMPORT_MAPPER, which is
// exactly agents, toolkits and skills
// (legacy/plugins/elitea_core/utils/export_import_utils.py:21-25). The legacy
// fork answers through that same wizard, so the fork and the import carry ONE
// set, and a caller can read a channel of either answer with no guard.
//
// The fork answered with `datasources` and `prompts` instead of `toolkits` when
// the request named no application. Those two keys are in no legacy mapper, no
// path of this service can put an entry in them, and a read-only search of
// EliteaUI, admin_ui, Maintenance-UI and elitea-web finds no reader. They are
// removed, because a channel that only one of the two fork paths can carry is
// the same defect in the other direction.
//
// An absent channel and a `null` channel break the wizard in the same way, so a
// nil slice becomes an empty array here rather than at each call.
func importChannels[T any](agents, toolkits, skills []T) map[string]any {
	channel := func(entries []T) any {
		if entries == nil {
			return []T{}
		}
		return entries
	}
	return map[string]any{
		"agents":   channel(agents),
		"toolkits": channel(toolkits),
		"skills":   channel(skills),
	}
}

// importVersionTags writes the `tags` array of one imported agent version into
// the store every other path already keeps a tag in, and returns the rows it
// wrote plus one message for each tag it could not write.
//
// # The store
//
// A tag is not a column. It is a row of `p_<id>.tags` (id, name UNIQUE, data
// jsonb) joined to the version through `p_<id>.application_version_tag_
// association` (001_initial.sql:302-306, :362-366). Every path in this
// repository writes exactly that pair:
//
//   - the interactive editor — applications/handler.go, replaceVersionTags,
//     called from UpdateVersion;
//   - the skill import one file over — import_skills.go,
//     importSkillVersionTags, into the sibling association table;
//   - the FORK — handler.go, the `tags` block of the version loop;
//
// and the EXPORT reads it back (exportedVersionTags above). The import read the
// key not at all, so an exported agent came back with its tags gone: absent
// from the version-detail GET the editor reloads through, absent from the
// marketplace filters that count `application_version_tag_association`
// (handler.go, runPublishValidation), and absent from the next export, so the
// document could not survive a second round trip. `meta` cannot stand in for
// it — pylon's `update_version` dumps with
// `exclude={'tags', 'variables', 'tools'}`, so a tag never reaches that column
// at all.
//
// # The conflict rule is the editor's, not the fork's
//
// One `tags` row is shared by EVERY version that carries the name, because
// `name` is UNIQUE per tenant schema. So `data` is written only when the name
// is NEW: an existing tag keeps the data it already has, exactly as
// replaceVersionTags and importSkillVersionTags leave it. The fork's variant
// (`DO UPDATE SET data = EXCLUDED.data`) repaints the tag for every other agent
// in the project that uses the name, which importing one file must not do.
//
// # Reported, not best-effort
//
// A tag is what a marketplace listing is found by, so a lost one is the same
// 201-with-an-incomplete-agent that the variable insert beside it and the
// fork's identical insert were both changed away from (#420). Each failure
// becomes an `errors.agents` entry at the call site, which makes the answer
// 207.
//
// The rows returned are what the DATABASE holds after the write — the tag's id
// and its stored `data`, not the file's — so the caller's echo can report what
// was persisted rather than what was asked for.
func (h *Handler) importVersionTags(
	ctx context.Context, schema, versionName string, versionID int, source map[string]any,
) ([]map[string]any, []string) {
	written := make([]map[string]any, 0)
	messages := make([]string, 0)

	raw, present := source["tags"]
	if !present || raw == nil {
		return written, messages
	}
	entries, isArray := raw.([]any)
	if !isArray {
		// The same rule importedJSONArray applies to a column: a key of the
		// wrong JSON type is a fault in the file and is reported, rather than
		// silently importing an agent with no tags.
		return written, append(messages,
			"unable to import the tags of version "+versionName+": tags must be a JSON array")
	}

	seen := make(map[string]bool, len(entries))
	for _, entry := range entries {
		name, data := importedTagFrom(entry)
		// A nameless tag cannot be stored (`tags.name` is NOT NULL) and a
		// duplicate name is one row either way, so both are skipped rather than
		// failing the version — the rule replaceVersionTags applies to the same
		// list arriving from the editor.
		if name == "" || seen[name] {
			continue
		}
		seen[name] = true

		var dataJSON []byte
		if data != nil {
			encoded, err := json.Marshal(data)
			if err != nil {
				messages = append(messages, "unable to import tag "+name+": "+err.Error())
				continue
			}
			dataJSON = encoded
		}

		var tagID int
		var storedData string
		if err := h.pool.QueryRow(ctx, fmt.Sprintf(`
			INSERT INTO %s.tags (name, data) VALUES ($1, $2::jsonb)
			ON CONFLICT (name) DO UPDATE SET name = EXCLUDED.name
			RETURNING id, COALESCE(data::text, 'null')`, schema), name, dataJSON).Scan(&tagID, &storedData); err != nil {
			messages = append(messages, "unable to import tag "+name+": "+err.Error())
			continue
		}
		if _, err := h.pool.Exec(ctx, fmt.Sprintf(`
			INSERT INTO %s.application_version_tag_association (version_id, tag_id) VALUES ($1, $2)
			ON CONFLICT DO NOTHING`, schema), versionID, tagID); err != nil {
			messages = append(messages, "unable to import tag "+name+": "+err.Error())
			continue
		}

		var stored any
		// storedData is JSON this database just returned — it cannot fail.
		_ = json.Unmarshal([]byte(storedData), &stored)
		written = append(written, map[string]any{"id": tagID, "name": name, "data": stored})
	}
	return written, messages
}

// importedTagFrom reads the name and the data out of one entry of a `tags`
// array.
//
// It takes the two shapes the sibling readers take: the object the export
// writes ({name, data} — exportedVersionTags) and the object the editor sends
// ({id, name, data} — applications/handler.go, tagNameFrom), plus the bare
// string a hand-written file carries, which is what importSkillVersionTags and
// pylon's `_normalize_tags` accept. An entry's `id` is ignored on purpose: it
// names a row of the SOURCE project's `tags` table and means nothing here.
func importedTagFrom(raw any) (string, any) {
	switch typed := raw.(type) {
	case string:
		return strings.TrimSpace(typed), nil
	case map[string]any:
		name, _ := typed["name"].(string)
		return strings.TrimSpace(name), typed["data"]
	default:
		return "", nil
	}
}

// importPrincipalUserID resolves the user that the import and the fork write
// into applications.owner_id, application_versions.author_id and
// elitea_tools.author_id.
//
// Both paths read the principal like this:
//
//	userID := 1
//	if user.ID != "" {
//		_, _ = fmt.Sscanf(user.ID, "%d", &userID)
//	}
//
// An identifier the scan could not read left the 1 in place, so every row the
// request wrote was given to user 1 — a real account on every deployment, and
// the first account on most of them. The rows then appear in that person's
// name, the agent list filters on the same column, and nothing recorded that a
// substitution had happened.
//
// `OwningUserID` is the rule the sibling create route already applies
// (internal/api/v2/applications/handler.go, `principal`). It accepts a session
// principal and a fully validated token principal, whose owner it returns, and
// refuses a token principal that carries no owner. An import writes rows that
// carry an author, so it needs the same principal a create needs.
func importPrincipalUserID(ctx context.Context) (ownership.UserID, bool) {
	user, ok := auth.UserFromContext(ctx)
	if !ok {
		return 0, false
	}
	ownerID, ok := user.OwningUserID()
	// The author and owner columns are INTEGER, so an id above math.MaxInt32
	// names no row this schema holds. On a 32-bit build, `int(ownerID)`
	// truncates such an id into one that belongs to a different account. That
	// is the same mis-attribution this function exists to stop. An id outside
	// the range is refused, not truncated.
	if !ok || ownerID <= 0 || ownerID > math.MaxInt32 {
		return 0, false
	}
	// The type carries the meaning from here on: this is a USER id, and #533
	// gave the two kinds of number their own types so that one cannot be passed
	// where the other belongs. See internal/domain/ownership.
	authorID, err := ownership.NewUserID(ownerID)
	if err != nil {
		return 0, false
	}
	return authorID, true
}

// The three functions below encode one request-body key for one jsonb column.
//
// Five `if b, e := json.Marshal(x); e == nil` blocks wrote an empty value for
// `llm_settings`, `conversation_starters`, `meta`, a toolkit's `settings` and
// `selected_tools` when the encode failed, and the import still answered 201.
// The encode is only half of it: each block starts with a type assertion, and a
// value of the wrong JSON type took the same silent empty default. That is the
// `selected_tools` data loss — see importedJSONValue.
//
// Each function keeps ABSENT apart from WRONG. An absent key, and a JSON null,
// mean "this file says nothing about this column", so the column keeps its
// default. A key that is present with the wrong shape, or that cannot be
// encoded, is a fault and is reported.

// importedJSONObject encodes one key that a jsonb column holds as a JSON
// object: `llm_settings`, `meta`, and a toolkit's `settings`.
func importedJSONObject(source map[string]any, key string) (string, error) {
	raw, present := source[key]
	if !present || raw == nil {
		return "{}", nil
	}
	value, ok := raw.(map[string]any)
	if !ok {
		return "", fmt.Errorf("%s must be a JSON object", key)
	}
	return importedJSONEncode(key, value)
}

// importedJSONArray encodes one key that a jsonb column holds as a JSON array:
// `conversation_starters`.
func importedJSONArray(source map[string]any, key string) (string, error) {
	raw, present := source[key]
	if !present || raw == nil {
		return "[]", nil
	}
	value, ok := raw.([]any)
	if !ok {
		return "", fmt.Errorf("%s must be a JSON array", key)
	}
	return importedJSONEncode(key, value)
}

// importedJSONValue keeps whatever JSON the file carries, whatever its type.
//
// This is `selected_tools`, and it is the fault the issue asks to repair first.
// The import read the key as `map[string]any`. The column's default is a JSON
// ARRAY, the route that writes a selection writes an array of tool names
// (internal/api/v2/toolkits/handler.go, selectedToolsPayload), and the export
// writes the column out as the database holds it. So the assertion failed on
// every selection a user had actually made, the import stored `{}` in its
// place, and an export followed by an import lost the tool selection with no
// message anywhere.
//
// The repair keeps the value verbatim rather than requiring an array. The
// import's job is to reproduce the document it was given: a file written by an
// older installation may carry a different shape, and refusing it would trade
// one kind of loss for another. `empty` is what an absent key means, and for
// this column it is `[]`, which is the column's own default.
func importedJSONValue(source map[string]any, key, empty string) (string, error) {
	raw, present := source[key]
	if !present || raw == nil {
		return empty, nil
	}
	return importedJSONEncode(key, raw)
}

// importedJSONEncode is the one place the encode failure is reported from.
//
// A value that `encoding/json` decoded can always be encoded again, so no
// request body can reach this error. It is returned rather than swallowed
// because the callers cannot know that, and because the four fallbacks it
// replaces are how the type faults above stayed invisible: an `e == nil` guard
// reads as care and behaves as silence.
func importedJSONEncode(key string, value any) (string, error) {
	encoded, err := json.Marshal(value)
	if err != nil {
		return "", fmt.Errorf("unable to encode %s: %w", key, err)
	}
	return string(encoded), nil
}
