package mcp

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

type internalVersionState struct {
	id                   int64
	applicationID        int64
	name                 string
	status               string
	authorID             int64
	createdAt            time.Time
	agentType            string
	instructions         string
	welcomeMessage       string
	llmSettings          map[string]any
	conversationStarters []any
	meta                 map[string]any
	pipelineSettings     map[string]any
}

type internalVersionBackup struct {
	id   int64
	name string
}

func (executor *postgresInternalApplicationExecutor) updateVersion(
	ctx context.Context,
	projectID int64,
	actorID int64,
	arguments map[string]any,
) (internalApplicationExecution, error) {
	applicationID, versionID, err := numericApplicationVersionIDs(arguments)
	if err != nil {
		return internalApplicationBadRequest(err.Error())
	}
	schema, ok := projectSchema(fmt.Sprintf("%d", projectID))
	if !ok {
		return internalApplicationExecution{}, errors.New("internal application project is invalid")
	}

	tx, err := executor.pool.Begin(ctx)
	if err != nil {
		return internalApplicationExecution{}, fmt.Errorf("begin internal application update: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	state, err := lockInternalVersion(ctx, tx, schema, applicationID, versionID)
	if errors.Is(err, pgx.ErrNoRows) {
		return internalApplicationNotFound()
	}
	if err != nil {
		return internalApplicationExecution{}, fmt.Errorf("lock internal application version: %w", err)
	}
	if state.status == "published" || state.status == "embedded" {
		return internalApplicationConflict(fmt.Sprintf(
			"Version id %d is %s and can not be updated", versionID, state.status,
		))
	}

	update, problem := parseInternalVersionUpdate(arguments, state)
	if problem != nil {
		return *problem, nil
	}
	backup, err := cloneInternalVersion(ctx, tx, schema, projectID, state, actorID, time.Now().UTC())
	if err != nil {
		if isUniqueViolation(err) {
			return internalApplicationBadRequest("a backup version with this name already exists")
		}
		return internalApplicationExecution{}, fmt.Errorf("clone internal application version: %w", err)
	}
	if err := applyInternalVersionUpdate(ctx, tx, schema, state, actorID, update); err != nil {
		if isUniqueViolation(err) {
			return internalApplicationBadRequest("version name already exists")
		}
		return internalApplicationExecution{}, fmt.Errorf("update internal application version: %w", err)
	}
	updated, err := readInternalVersion(ctx, tx, schema, applicationID, versionID, false)
	if err != nil {
		return internalApplicationExecution{}, fmt.Errorf("read updated internal application version: %w", err)
	}
	response, err := internalVersionResponse(ctx, tx, schema, updated, backup)
	if err != nil {
		return internalApplicationExecution{}, fmt.Errorf("build internal application update response: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return internalApplicationExecution{}, fmt.Errorf("commit internal application update: %w", err)
	}
	return jsonExecution(http.StatusCreated, response)
}

func (executor *postgresInternalApplicationExecutor) patchInstructions(
	ctx context.Context,
	projectID int64,
	actorID int64,
	arguments map[string]any,
) (internalApplicationExecution, error) {
	applicationID, versionID, err := numericApplicationVersionIDs(arguments)
	if err != nil {
		return internalApplicationBadRequest(err.Error())
	}
	expected, _ := arguments["expected_instructions_sha256"].(string)
	if len(expected) != sha256.Size*2 || !isHex(expected) {
		return internalApplicationBadRequest("expected_instructions_sha256 must be a 64-character hexadecimal string")
	}
	replacement, ok := arguments["replacement"].(string)
	if !ok {
		return internalApplicationBadRequest("replacement must be a string")
	}
	replaceAll, _ := arguments["replace_all"].(bool)
	oldText, _ := arguments["old_text"].(string)
	if !replaceAll && oldText == "" {
		return internalApplicationBadRequest("old_text is required unless replace_all is true")
	}
	schema, ok := projectSchema(fmt.Sprintf("%d", projectID))
	if !ok {
		return internalApplicationExecution{}, errors.New("internal application project is invalid")
	}

	tx, err := executor.pool.Begin(ctx)
	if err != nil {
		return internalApplicationExecution{}, fmt.Errorf("begin internal instruction patch: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	state, err := lockInternalVersion(ctx, tx, schema, applicationID, versionID)
	if errors.Is(err, pgx.ErrNoRows) {
		return internalApplicationNotFound()
	}
	if err != nil {
		return internalApplicationExecution{}, fmt.Errorf("lock internal application version: %w", err)
	}
	if state.status == "published" || state.status == "embedded" {
		return internalApplicationConflict(fmt.Sprintf(
			"Version id %d is %s and can not be updated", versionID, state.status,
		))
	}
	if !strings.EqualFold(expected, instructionsSHA256(state.instructions)) {
		return internalApplicationConflict("Instructions changed after they were read. Read the version again and retry.")
	}

	updatedInstructions := replacement
	if !replaceAll {
		occurrences := strings.Count(state.instructions, oldText)
		if occurrences != 1 {
			return internalApplicationConflict(fmt.Sprintf(
				"old_text must match exactly once; found %d matches.", occurrences,
			))
		}
		updatedInstructions = strings.Replace(state.instructions, oldText, replacement, 1)
	}
	if strings.TrimSpace(updatedInstructions) == "" {
		return internalApplicationConflict("Refusing to replace non-empty instructions with empty content.")
	}
	if updatedInstructions == state.instructions {
		return internalApplicationConflict("The patch would not change the instructions.")
	}

	backup, err := cloneInternalVersion(ctx, tx, schema, projectID, state, actorID, time.Now().UTC())
	if err != nil {
		return internalApplicationExecution{}, fmt.Errorf("clone internal application version: %w", err)
	}
	if _, err := tx.Exec(ctx, fmt.Sprintf(`
		UPDATE %s.application_versions
		SET instructions = $1, author_id = $2
		WHERE application_id = $3 AND id = $4`, schema),
		updatedInstructions, actorID, applicationID, versionID); err != nil {
		return internalApplicationExecution{}, fmt.Errorf("patch internal application instructions: %w", err)
	}
	updated, err := readInternalVersion(ctx, tx, schema, applicationID, versionID, false)
	if err != nil {
		return internalApplicationExecution{}, fmt.Errorf("read patched internal application version: %w", err)
	}
	response, err := internalVersionResponse(ctx, tx, schema, updated, backup)
	if err != nil {
		return internalApplicationExecution{}, fmt.Errorf("build internal instruction patch response: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return internalApplicationExecution{}, fmt.Errorf("commit internal instruction patch: %w", err)
	}
	return jsonExecution(http.StatusCreated, response)
}

type internalVersionUpdate struct {
	setClauses []string
	values     []any
	variables  []any
	tags       []any
	hasVars    bool
	hasTags    bool
}

func parseInternalVersionUpdate(
	arguments map[string]any,
	state internalVersionState,
) (internalVersionUpdate, *internalApplicationExecution) {
	allowed := map[string]bool{
		"project_id": true, "application_id": true, "version_id": true,
		"name": true, "agent_type": true, "welcome_message": true,
		"llm_settings": true, "conversation_starters": true, "variables": true,
		"tags": true, "meta": true, "pipeline_settings": true, "notes": true,
		"instructions": true,
	}
	for name := range arguments {
		if !allowed[name] {
			problem, _ := internalApplicationBadRequest("unsupported update field: " + name)
			return internalVersionUpdate{}, &problem
		}
	}
	if requested, present := arguments["instructions"]; present {
		text, stringValue := requested.(string)
		if requested != nil && (!stringValue || (text != "" && text != state.instructions)) {
			problem, _ := internalApplicationConflict(
				"Internal MCP instruction changes must use the safe instructions patch tool. " +
					"Read the version again, then patch it using instructions_sha256.",
			)
			return internalVersionUpdate{}, &problem
		}
	}

	update := internalVersionUpdate{}
	appendValue := func(column string, value any) {
		update.values = append(update.values, value)
		update.setClauses = append(update.setClauses, fmt.Sprintf("%s = $%d", column, len(update.values)))
	}
	appendJSON := func(column string, value any) *internalApplicationExecution {
		encoded, err := json.Marshal(value)
		if err != nil {
			problem, _ := internalApplicationBadRequest(column + " is not valid JSON")
			return &problem
		}
		update.values = append(update.values, encoded)
		update.setClauses = append(update.setClauses, fmt.Sprintf("%s = $%d::jsonb", column, len(update.values)))
		return nil
	}

	if raw, present := arguments["name"]; present {
		name, ok := raw.(string)
		if !ok || strings.TrimSpace(name) == "" {
			problem, _ := internalApplicationBadRequest("name must not be empty")
			return internalVersionUpdate{}, &problem
		}
		if state.name == "base" && name != "base" {
			problem, _ := internalApplicationConflict("You cannot change the name of the base version")
			return internalVersionUpdate{}, &problem
		}
		appendValue("name", name)
	}
	if raw, present := arguments["agent_type"]; present {
		agentType, ok := raw.(string)
		if !ok || !validInternalAgentType(agentType) {
			problem, _ := internalApplicationBadRequest("agent_type is invalid")
			return internalVersionUpdate{}, &problem
		}
		appendValue("agent_type", agentType)
	}
	if raw, present := arguments["welcome_message"]; present {
		value, ok := raw.(string)
		if !ok {
			problem, _ := internalApplicationBadRequest("welcome_message must be a string")
			return internalVersionUpdate{}, &problem
		}
		appendValue("welcome_message", value)
	}
	if raw, present := arguments["llm_settings"]; present && raw != nil {
		if _, ok := raw.(map[string]any); !ok {
			problem, _ := internalApplicationBadRequest("llm_settings must be an object")
			return internalVersionUpdate{}, &problem
		}
		if problem := appendJSON("llm_settings", raw); problem != nil {
			return internalVersionUpdate{}, problem
		}
	}
	if raw, present := arguments["conversation_starters"]; present && raw != nil {
		values, ok := raw.([]any)
		if !ok || len(values) > 4 || !allNonEmptyStrings(values) {
			problem, _ := internalApplicationBadRequest("conversation_starters must contain at most four non-empty strings")
			return internalVersionUpdate{}, &problem
		}
		if problem := appendJSON("conversation_starters", values); problem != nil {
			return internalVersionUpdate{}, problem
		}
	}
	if raw, present := arguments["pipeline_settings"]; present && raw != nil {
		settings, ok := raw.(map[string]any)
		if !ok {
			problem, _ := internalApplicationBadRequest("pipeline_settings must be an object")
			return internalVersionUpdate{}, &problem
		}
		if _, hasTrigger := settings["trigger"]; !hasTrigger {
			if trigger, hadTrigger := state.pipelineSettings["trigger"]; hadTrigger {
				settings = cloneMap(settings)
				settings["trigger"] = trigger
			}
		}
		if problem := appendJSON("pipeline_settings", settings); problem != nil {
			return internalVersionUpdate{}, problem
		}
	}

	meta := state.meta
	metaChanged := false
	if raw, present := arguments["meta"]; present {
		if raw == nil {
			meta = map[string]any{}
		} else {
			var ok bool
			meta, ok = raw.(map[string]any)
			if !ok {
				problem, _ := internalApplicationBadRequest("meta must be an object")
				return internalVersionUpdate{}, &problem
			}
		}
		meta = cloneMap(meta)
		metaChanged = true
	}
	if raw, present := arguments["notes"]; present {
		if !metaChanged {
			meta = cloneMap(state.meta)
		}
		if raw == nil {
			delete(meta, "notes")
		} else {
			notes, ok := raw.(string)
			if !ok || len(notes) > 1000 {
				problem, _ := internalApplicationBadRequest("notes must contain at most 1000 characters")
				return internalVersionUpdate{}, &problem
			}
			meta["notes"] = notes
		}
		metaChanged = true
	}
	if metaChanged {
		if problem := appendJSON("meta", meta); problem != nil {
			return internalVersionUpdate{}, problem
		}
	}

	if raw, present := arguments["variables"]; present && raw != nil {
		values, ok := raw.([]any)
		if !ok || !validInternalVariables(values) {
			problem, _ := internalApplicationBadRequest("variables must contain objects with unique non-empty names and string values")
			return internalVersionUpdate{}, &problem
		}
		update.variables, update.hasVars = values, true
	}
	if raw, present := arguments["tags"]; present && raw != nil {
		values, ok := raw.([]any)
		if !ok || !validInternalTags(values) {
			problem, _ := internalApplicationBadRequest("tags must contain objects with unique non-empty names and optional object data")
			return internalVersionUpdate{}, &problem
		}
		update.tags, update.hasTags = values, true
	}
	if len(update.setClauses) == 0 && !update.hasVars && !update.hasTags {
		problem, _ := internalApplicationBadRequest("at least one non-instruction field is required")
		return internalVersionUpdate{}, &problem
	}
	return update, nil
}

func applyInternalVersionUpdate(
	ctx context.Context,
	tx pgx.Tx,
	schema string,
	state internalVersionState,
	actorID int64,
	update internalVersionUpdate,
) error {
	update.values = append(update.values, actorID, state.applicationID, state.id)
	update.setClauses = append(update.setClauses, fmt.Sprintf("author_id = $%d", len(update.values)-2))
	if _, err := tx.Exec(ctx, fmt.Sprintf(`
		UPDATE %s.application_versions SET %s
		WHERE application_id = $%d AND id = $%d`,
		schema, strings.Join(update.setClauses, ", "), len(update.values)-1, len(update.values)), update.values...); err != nil {
		return err
	}
	if update.hasVars {
		if err := replaceInternalVariables(ctx, tx, schema, state.id, update.variables); err != nil {
			return err
		}
	}
	if update.hasTags {
		if err := replaceInternalTags(ctx, tx, schema, state.id, update.tags); err != nil {
			return err
		}
	}
	return nil
}

func lockInternalVersion(
	ctx context.Context,
	tx pgx.Tx,
	schema string,
	applicationID int64,
	versionID int64,
) (internalVersionState, error) {
	return readInternalVersion(ctx, tx, schema, applicationID, versionID, true)
}

func readInternalVersion(
	ctx context.Context,
	tx pgx.Tx,
	schema string,
	applicationID int64,
	versionID int64,
	lock bool,
) (internalVersionState, error) {
	query := fmt.Sprintf(`
		SELECT v.id, v.application_id, v.name, v.status, v.author_id, v.created_at,
		       v.agent_type, COALESCE(v.instructions, ''), COALESCE(v.welcome_message, ''),
		       COALESCE(v.llm_settings::text, '{}'), COALESCE(v.conversation_starters::text, '[]'),
		       COALESCE(v.meta::text, '{}'), COALESCE(v.pipeline_settings::text, '{}')
		FROM %s.application_versions v
		JOIN %s.applications a ON a.id = v.application_id
		WHERE v.application_id = $1 AND v.id = $2`, schema, schema)
	if lock {
		query += " FOR UPDATE OF v"
	}
	var state internalVersionState
	var llmSettings, conversationStarters, meta, pipelineSettings string
	err := tx.QueryRow(ctx, query, applicationID, versionID).Scan(
		&state.id, &state.applicationID, &state.name, &state.status, &state.authorID, &state.createdAt,
		&state.agentType, &state.instructions, &state.welcomeMessage,
		&llmSettings, &conversationStarters, &meta, &pipelineSettings,
	)
	if err != nil {
		return internalVersionState{}, err
	}
	if err := json.Unmarshal([]byte(llmSettings), &state.llmSettings); err != nil {
		return internalVersionState{}, err
	}
	if err := json.Unmarshal([]byte(conversationStarters), &state.conversationStarters); err != nil {
		return internalVersionState{}, err
	}
	if err := json.Unmarshal([]byte(meta), &state.meta); err != nil {
		return internalVersionState{}, err
	}
	if err := json.Unmarshal([]byte(pipelineSettings), &state.pipelineSettings); err != nil {
		return internalVersionState{}, err
	}
	return state, nil
}

func cloneInternalVersion(
	ctx context.Context,
	tx pgx.Tx,
	schema string,
	projectID int64,
	source internalVersionState,
	actorID int64,
	now time.Time,
) (internalVersionBackup, error) {
	backup := internalVersionBackup{name: internalBackupName(source.id, now)}
	err := tx.QueryRow(ctx, fmt.Sprintf(`
		INSERT INTO %s.application_versions
			(application_id, name, status, author_id, llm_settings, instructions,
			 conversation_starters, welcome_message, agent_type, meta, pipeline_settings,
			 shared_owner_id, shared_id)
		SELECT application_id, $2, 'draft', $3, llm_settings, instructions,
		       conversation_starters, welcome_message, agent_type, meta, pipeline_settings,
		       NULL, NULL
		FROM %s.application_versions
		WHERE application_id = $1 AND id = $4
		RETURNING id`, schema, schema), source.applicationID, backup.name, actorID, source.id).Scan(&backup.id)
	if err != nil {
		return internalVersionBackup{}, err
	}
	statements := []string{
		fmt.Sprintf(`INSERT INTO %s.application_variables (application_version_id, name, value)
			SELECT $2, name, value FROM %s.application_variables WHERE application_version_id = $1`, schema, schema),
		fmt.Sprintf(`INSERT INTO %s.application_version_tag_association (version_id, tag_id)
			SELECT $2, tag_id FROM %s.application_version_tag_association WHERE version_id = $1`, schema, schema),
		fmt.Sprintf(`INSERT INTO %s.entity_skill_mapping
			(entity_version_id, entity_type, skill_id, skill_version_id)
			SELECT $2, entity_type, skill_id, skill_version_id
			FROM %s.entity_skill_mapping WHERE entity_version_id = $1`, schema, schema),
	}
	for _, statement := range statements {
		if _, err := tx.Exec(ctx, statement, source.id, backup.id); err != nil {
			return internalVersionBackup{}, err
		}
	}

	var toolMappingHasEntityID bool
	if err := tx.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM information_schema.columns
			WHERE table_schema = $1 AND table_name = 'entity_tool_mapping' AND column_name = 'entity_id'
		)`, fmt.Sprintf("p_%d", projectID)).Scan(&toolMappingHasEntityID); err != nil {
		return internalVersionBackup{}, err
	}
	toolCopy := fmt.Sprintf(`INSERT INTO %s.entity_tool_mapping
		(entity_version_id, entity_type, tool_id, selected_tools)
		SELECT $2, entity_type, tool_id, selected_tools
		FROM %s.entity_tool_mapping WHERE entity_version_id = $1`, schema, schema)
	if toolMappingHasEntityID {
		toolCopy = fmt.Sprintf(`INSERT INTO %s.entity_tool_mapping
			(entity_version_id, entity_id, entity_type, tool_id, selected_tools)
			SELECT $2, entity_id, entity_type, tool_id, selected_tools
			FROM %s.entity_tool_mapping WHERE entity_version_id = $1`, schema, schema)
	}
	if _, err := tx.Exec(ctx, toolCopy, source.id, backup.id); err != nil {
		return internalVersionBackup{}, err
	}

	var legacyApplicationTools bool
	relation := fmt.Sprintf("p_%d.application_tools", projectID)
	if err := tx.QueryRow(ctx, `SELECT to_regclass($1) IS NOT NULL`, relation).Scan(&legacyApplicationTools); err != nil {
		return internalVersionBackup{}, err
	}
	if legacyApplicationTools {
		if _, err := tx.Exec(ctx, fmt.Sprintf(`
			INSERT INTO %s.application_tools
				(application_version_id, type, name, description, settings)
			SELECT $2, type, name, description, settings
			FROM %s.application_tools WHERE application_version_id = $1`, schema, schema), source.id, backup.id); err != nil {
			return internalVersionBackup{}, err
		}
	}
	return backup, nil
}

func replaceInternalVariables(
	ctx context.Context,
	tx pgx.Tx,
	schema string,
	versionID int64,
	variables []any,
) error {
	if _, err := tx.Exec(ctx, fmt.Sprintf(
		`DELETE FROM %s.application_variables WHERE application_version_id = $1`, schema), versionID); err != nil {
		return err
	}
	for _, raw := range variables {
		entry := raw.(map[string]any)
		name := strings.TrimSpace(entry["name"].(string))
		value, _ := entry["value"].(string)
		if _, err := tx.Exec(ctx, fmt.Sprintf(`
			INSERT INTO %s.application_variables (application_version_id, name, value)
			VALUES ($1, $2, $3)`, schema), versionID, name, value); err != nil {
			return err
		}
	}
	return nil
}

func replaceInternalTags(
	ctx context.Context,
	tx pgx.Tx,
	schema string,
	versionID int64,
	tags []any,
) error {
	if _, err := tx.Exec(ctx, fmt.Sprintf(
		`DELETE FROM %s.application_version_tag_association WHERE version_id = $1`, schema), versionID); err != nil {
		return err
	}
	for _, raw := range tags {
		name, data := internalTag(raw)
		encoded, err := json.Marshal(data)
		if err != nil {
			return err
		}
		var tagID int64
		if err := tx.QueryRow(ctx, fmt.Sprintf(`
			INSERT INTO %s.tags (name, data) VALUES ($1, $2::jsonb)
			ON CONFLICT (name) DO UPDATE SET name = EXCLUDED.name
			RETURNING id`, schema), name, encoded).Scan(&tagID); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, fmt.Sprintf(`
			INSERT INTO %s.application_version_tag_association (version_id, tag_id)
			VALUES ($1, $2)`, schema), versionID, tagID); err != nil {
			return err
		}
	}
	return nil
}

func internalVersionResponse(
	ctx context.Context,
	tx pgx.Tx,
	schema string,
	state internalVersionState,
	backup internalVersionBackup,
) (map[string]any, error) {
	variables := make([]map[string]any, 0)
	rows, err := tx.Query(ctx, fmt.Sprintf(`
		SELECT name, COALESCE(value, '') FROM %s.application_variables
		WHERE application_version_id = $1 ORDER BY id`, schema), state.id)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var name, value string
		if err := rows.Scan(&name, &value); err != nil {
			rows.Close()
			return nil, err
		}
		variables = append(variables, map[string]any{"name": name, "value": value})
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()

	tags := make([]map[string]any, 0)
	rows, err = tx.Query(ctx, fmt.Sprintf(`
		SELECT tag.id, tag.name, COALESCE(tag.data::text, 'null')
		FROM %s.application_version_tag_association association
		JOIN %s.tags tag ON tag.id = association.tag_id
		WHERE association.version_id = $1 ORDER BY tag.name`, schema, schema), state.id)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var id int64
		var name, dataText string
		if err := rows.Scan(&id, &name, &dataText); err != nil {
			rows.Close()
			return nil, err
		}
		var data any
		if err := json.Unmarshal([]byte(dataText), &data); err != nil {
			rows.Close()
			return nil, err
		}
		tags = append(tags, map[string]any{"id": id, "name": name, "data": data})
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()

	return map[string]any{
		"id":                    fmt.Sprintf("%d", state.id),
		"application_id":        fmt.Sprintf("%d", state.applicationID),
		"name":                  state.name,
		"status":                state.status,
		"author_id":             fmt.Sprintf("%d", state.authorID),
		"created_at":            state.createdAt,
		"agent_type":            state.agentType,
		"instructions":          state.instructions,
		"instructions_sha256":   instructionsSHA256(state.instructions),
		"welcome_message":       state.welcomeMessage,
		"llm_settings":          state.llmSettings,
		"conversation_starters": state.conversationStarters,
		"meta":                  state.meta,
		"pipeline_settings":     state.pipelineSettings,
		"variables":             variables,
		"tags":                  tags,
		"mcp_backup_version":    map[string]any{"id": backup.id, "name": backup.name},
	}, nil
}

func numericApplicationVersionIDs(arguments map[string]any) (int64, int64, error) {
	application, version, err := applicationVersionIDs(arguments)
	if err != nil {
		return 0, 0, err
	}
	applicationID, _ := parsePositiveInt64(application)
	versionID, _ := parsePositiveInt64(version)
	return applicationID, versionID, nil
}

func parsePositiveInt64(value string) (int64, bool) {
	parsed, err := strconv.ParseInt(value, 10, 64)
	return parsed, err == nil && parsed > 0
}

func validInternalAgentType(value string) bool {
	switch value {
	case "react", "elitea", "dial", "openai", "codemie", "raw", "autogen", "llama", "pipeline", "xml":
		return true
	default:
		return false
	}
}

func internalApplicationBadRequest(message string) (internalApplicationExecution, error) {
	return jsonExecution(http.StatusBadRequest, map[string]any{"error": message})
}

func internalApplicationConflict(message string) (internalApplicationExecution, error) {
	return jsonExecution(http.StatusConflict, map[string]any{"error": message})
}

func internalApplicationNotFound() (internalApplicationExecution, error) {
	return jsonExecution(http.StatusNotFound, map[string]any{"error": "Application version not found"})
}

func instructionsSHA256(instructions string) string {
	digest := sha256.Sum256([]byte(instructions))
	return fmt.Sprintf("%x", digest[:])
}

func internalBackupName(versionID int64, now time.Time) string {
	now = now.UTC()
	timestamp := now.Format("20060102T150405") + fmt.Sprintf("%06d", now.Nanosecond()/1000) + "Z"
	return fmt.Sprintf("mcp-backup-%d-%s", versionID, timestamp)
}

func isHex(value string) bool {
	for _, char := range value {
		if (char < '0' || char > '9') && (char < 'a' || char > 'f') && (char < 'A' || char > 'F') {
			return false
		}
	}
	return true
}

func isUniqueViolation(err error) bool {
	var pgError *pgconn.PgError
	return errors.As(err, &pgError) && pgError.Code == "23505"
}

func allNonEmptyStrings(values []any) bool {
	for _, value := range values {
		text, ok := value.(string)
		if !ok || strings.TrimSpace(text) == "" {
			return false
		}
	}
	return true
}

func validInternalVariables(values []any) bool {
	seen := make(map[string]struct{}, len(values))
	for _, raw := range values {
		entry, ok := raw.(map[string]any)
		if !ok {
			return false
		}
		name, _ := entry["name"].(string)
		name = strings.TrimSpace(name)
		_, valueIsString := entry["value"].(string)
		if !valueIsString {
			return false
		}
		if name == "" {
			return false
		}
		if _, duplicate := seen[name]; duplicate {
			return false
		}
		seen[name] = struct{}{}
	}
	return true
}

func validInternalTags(values []any) bool {
	seen := make(map[string]struct{}, len(values))
	for _, raw := range values {
		entry, ok := raw.(map[string]any)
		if !ok {
			return false
		}
		name, _ := entry["name"].(string)
		name = strings.TrimSpace(name)
		if name == "" {
			return false
		}
		if data, present := entry["data"]; present && data != nil {
			if _, ok := data.(map[string]any); !ok {
				return false
			}
		}
		if id, present := entry["id"]; present && id != nil {
			text := scalarArgument(id)
			if text == "" {
				return false
			}
			parsed, err := strconv.ParseInt(text, 10, 64)
			if err != nil || parsed <= 0 {
				return false
			}
		}
		if _, duplicate := seen[name]; duplicate {
			return false
		}
		seen[name] = struct{}{}
	}
	return true
}

func internalTag(raw any) (string, any) {
	entry := raw.(map[string]any)
	name, _ := entry["name"].(string)
	return strings.TrimSpace(name), entry["data"]
}

func cloneMap(source map[string]any) map[string]any {
	clone := make(map[string]any, len(source))
	for key, value := range source {
		clone[key] = value
	}
	return clone
}
