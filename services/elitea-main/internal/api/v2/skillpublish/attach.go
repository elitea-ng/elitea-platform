package skillpublish

// attach_public_skill.py and agents_with_skill.py.
//
// Attaching a catalog skill to an agent is a FORK plus a mapping row, not a
// cross-project reference: the agent's project gets its own copy of the skill
// so a later unpublish cannot pull instructions out from under a running agent.
// The copy carries `parent_project_id` / `parent_entity_id` /
// `parent_version_id` in its version meta, and every lookup in this file —
// resolve-or-fork, already-attached, reverse lookup — keys off that lineage
// rather than off names.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"
)

type attachRequest struct {
	PublicSkillID   int    `json:"public_skill_id"`
	PublicVersionID int    `json:"public_version_id"`
	AgentVersionIDs []int  `json:"agent_version_ids"`
	EntityType      string `json:"entity_type"`
}

type attachResult struct {
	AgentVersionID int    `json:"agent_version_id"`
	OK             bool   `json:"ok"`
	HTTPStatus     int    `json:"http_status,omitempty"`
	Error          string `json:"error,omitempty"`
}

// AttachPublicSkill forks a published skill into a project and maps it onto the
// requested agent versions.
//
// Partial success is carried per agent in the body, not in the HTTP status —
// the reference's contract, and the reason a batch of five with one bad id is
// still a 200.
func (h *Handler) AttachPublicSkill(w http.ResponseWriter, r *http.Request) {
	projectID := chi.URLParam(r, "projectID")
	schema, ok := projectSchema(projectID)
	if !ok {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid project id"})
		return
	}
	ctx := r.Context()

	var body attachRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid request body"})
		return
	}
	if body.PublicSkillID <= 0 || body.PublicVersionID <= 0 || len(body.AgentVersionIDs) == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error": "public_skill_id, public_version_id and agent_version_ids are required",
		})
		return
	}
	entityType := body.EntityType
	if entityType == "" {
		entityType = entityTypeAgent
	}
	if entityType != entityTypeAgent {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": fmt.Sprintf("invalid entity_type '%s'", entityType)})
		return
	}

	agentVersionIDs := make([]int, 0, len(body.AgentVersionIDs))
	seen := map[int]bool{}
	for _, id := range body.AgentVersionIDs {
		if id > 0 && !seen[id] {
			seen[id] = true
			agentVersionIDs = append(agentVersionIDs, id)
		}
	}
	if len(agentVersionIDs) == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error": "public_skill_id, public_version_id and agent_version_ids are required",
		})
		return
	}

	publicID := publicProjectID()
	publicQuoted := publicSchema()
	userID := actingUserID(ctx, 1)

	localSkillID, localVersionID, ok := h.resolveOrForkLocalSkill(ctx, schema, publicQuoted, publicID, body, userID)
	if !ok {
		results := make([]attachResult, 0, len(agentVersionIDs))
		for _, id := range agentVersionIDs {
			results = append(results, attachResult{AgentVersionID: id, OK: false, HTTPStatus: http.StatusNotFound, Error: "public skill or version not found"})
		}
		writeJSON(w, http.StatusOK, map[string]any{"results": results})
		return
	}

	lineageSkillIDs := h.lineageSkillIDs(ctx, schema, publicID, body.PublicSkillID)

	results := make([]attachResult, 0, len(agentVersionIDs))
	for _, agentVersionID := range agentVersionIDs {
		var owned bool
		_ = h.pool.QueryRow(ctx, fmt.Sprintf(`
			SELECT EXISTS(
				SELECT 1 FROM %[1]s.application_versions av
				JOIN %[1]s.applications a ON a.id = av.application_id
				WHERE av.id = $1 AND COALESCE(av.agent_type, '') <> 'pipeline')`, schema),
			agentVersionID).Scan(&owned) // a failed check leaves owned=false → reported as not found
		if !owned {
			results = append(results, attachResult{AgentVersionID: agentVersionID, OK: false, HTTPStatus: http.StatusNotFound, Error: "agent not found"})
			continue
		}

		// Already attached is judged across the whole lineage, not just the
		// copy resolved above: two published versions of the same public skill
		// fork to two local skills, and attaching both to one agent would give
		// it the same skill twice under different ids.
		if len(lineageSkillIDs) > 0 {
			var attached bool
			_ = h.pool.QueryRow(ctx, fmt.Sprintf(`
				SELECT EXISTS(
					SELECT 1 FROM %s.entity_skill_mapping
					WHERE entity_version_id = $1 AND entity_type = $2 AND skill_id = ANY($3))`, schema),
				agentVersionID, entityType, lineageSkillIDs).Scan(&attached) // failure leaves false; the unique constraint below still guards
			if attached {
				results = append(results, attachResult{AgentVersionID: agentVersionID, OK: false, HTTPStatus: http.StatusConflict, Error: "already attached"})
				continue
			}
		}

		tag, err := h.pool.Exec(ctx, fmt.Sprintf(`
			INSERT INTO %s.entity_skill_mapping (entity_version_id, entity_type, skill_id, skill_version_id, created_at)
			VALUES ($1, $2, $3, $4, now() AT TIME ZONE 'UTC')
			ON CONFLICT ON CONSTRAINT _entity_skill_unique DO NOTHING`, schema),
			agentVersionID, entityType, localSkillID, localVersionID)
		switch {
		case err != nil:
			results = append(results, attachResult{AgentVersionID: agentVersionID, OK: false, HTTPStatus: http.StatusBadRequest, Error: "attach failed"})
		case tag.RowsAffected() == 0:
			results = append(results, attachResult{AgentVersionID: agentVersionID, OK: false, HTTPStatus: http.StatusConflict, Error: "already attached"})
		default:
			results = append(results, attachResult{AgentVersionID: agentVersionID, OK: true})
		}
	}

	writeJSON(w, http.StatusOK, map[string]any{"results": results})
}

// resolveOrForkLocalSkill returns the project's own copy of a published
// (skill, version), forking it in on first use.
//
// The fork is committed independently of the per-agent loop that follows, as in
// the reference. A loop that then fails leaves an unmapped local copy, which the
// next attach of the same pair reuses — self-healing, so no cleanup path is
// needed.
func (h *Handler) resolveOrForkLocalSkill(ctx context.Context, schema, publicQuoted, publicID string, body attachRequest, userID int) (int, int, bool) {
	var skillID, versionID int
	err := h.pool.QueryRow(ctx, fmt.Sprintf(`
		SELECT skill_id, id FROM %s.skill_versions
		WHERE meta->>'parent_project_id' = $1
		  AND meta->>'parent_entity_id' = $2
		  AND meta->>'parent_version_id' = $3
		ORDER BY id LIMIT 1`, schema),
		publicID, strconv.Itoa(body.PublicSkillID), strconv.Itoa(body.PublicVersionID)).Scan(&skillID, &versionID)
	if err == nil {
		return skillID, versionID, true
	}

	// PUBLISHED only. The public project also holds drafts — an admin's
	// in-place work in progress — and forking copies the instructions verbatim
	// into a project the caller can read. Without this filter, guessing a
	// (skill, version) pair is enough to pull unpublished content out of the
	// public project, which is exactly what every other read in this package
	// filters against.
	source, found := h.readSkillVersion(ctx, publicQuoted, strconv.Itoa(body.PublicSkillID), strconv.Itoa(body.PublicVersionID))
	if !found || source.Status != "published" {
		return 0, 0, false
	}

	tx, err := h.pool.Begin(ctx)
	if err != nil {
		return 0, 0, false
	}
	defer func() { _ = tx.Rollback(ctx) }()

	projectID, _ := strconv.Atoi(strings.TrimPrefix(schema, "p_"))
	var forkedSkillID int
	if err := tx.QueryRow(ctx, fmt.Sprintf(`
		INSERT INTO %s.skills (name, description, owner_id, author_id, meta, created_at)
		VALUES ($1, $2, $3, $4, '{}'::jsonb, now() AT TIME ZONE 'UTC')
		RETURNING id`, schema),
		source.SkillName, source.SkillDescription, projectID, userID).Scan(&forkedSkillID); err != nil {
		return 0, 0, false
	}

	publicNumeric, _ := strconv.Atoi(publicID)
	forkedVersionID, err := insertVersion(ctx, tx, schema, forkedSkillID, defaultVersionName, source.Instructions,
		userID, "draft", map[string]any{
			"parent_project_id": publicNumeric,
			"parent_entity_id":  body.PublicSkillID,
			"parent_version_id": body.PublicVersionID,
			"parent_author_id":  source.AuthorID,
		}, source.Tags)
	if err != nil {
		return 0, 0, false
	}
	if err := setDefaultVersionIfUnset(ctx, tx, schema, forkedSkillID, forkedVersionID); err != nil {
		return 0, 0, false
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, 0, false
	}
	return forkedSkillID, forkedVersionID, true
}

// lineageSkillIDs lists every local skill forked from one public skill, any
// version.
func (h *Handler) lineageSkillIDs(ctx context.Context, schema, publicID string, publicSkillID int) []int {
	rows, err := h.pool.Query(ctx, fmt.Sprintf(`
		SELECT DISTINCT skill_id FROM %s.skill_versions
		WHERE meta->>'parent_project_id' = $1 AND meta->>'parent_entity_id' = $2`, schema),
		publicID, strconv.Itoa(publicSkillID))
	if err != nil {
		return nil
	}
	defer rows.Close()
	var ids []int
	for rows.Next() {
		var id int
		if rows.Scan(&id) != nil {
			continue
		}
		ids = append(ids, id)
	}
	return ids
}

// AgentsWithSkill is the reverse lookup: which agent versions in this project
// use any local copy of a given public skill.
//
// Legacy: agents_with_skill.py.
func (h *Handler) AgentsWithSkill(w http.ResponseWriter, r *http.Request) {
	schema, ok := projectSchema(chi.URLParam(r, "projectID"))
	if !ok {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid project id"})
		return
	}
	skillID := chi.URLParam(r, "skillID")
	if !isPositiveInt(skillID) {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid skill id"})
		return
	}
	ctx := r.Context()
	publicSkillID, _ := strconv.Atoi(skillID)

	// Scoping by parent_project_id is required, not decorative: per-schema
	// sequences overlap, so a bare parent_entity_id match would also count
	// skills forked from a DIFFERENT project's skill that happens to share the
	// id.
	lineage := h.lineageSkillIDs(ctx, schema, publicProjectID(), publicSkillID)
	if len(lineage) == 0 {
		writeJSON(w, http.StatusOK, map[string]any{"total": 0, "rows": []any{}})
		return
	}

	rows, err := h.pool.Query(ctx, fmt.Sprintf(`
		SELECT DISTINCT a.id, a.name, av.id, COALESCE(a.meta::text, '{}')
		FROM %[1]s.entity_skill_mapping m
		JOIN %[1]s.application_versions av ON av.id = m.entity_version_id
		JOIN %[1]s.applications a ON a.id = av.application_id
		WHERE m.skill_id = ANY($1) AND m.entity_type = $2
		ORDER BY a.id, av.id`, schema), lineage, entityTypeAgent)
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"total": 0, "rows": []any{}})
		return
	}
	defer rows.Close()

	items := make([]map[string]any, 0)
	for rows.Next() {
		var applicationID, entityVersionID int
		var name, metaText string
		if rows.Scan(&applicationID, &name, &entityVersionID, &metaText) != nil {
			continue
		}
		var meta map[string]any
		_ = json.Unmarshal([]byte(metaText), &meta) // DB jsonb column
		items = append(items, map[string]any{
			"application_id":    applicationID,
			"name":              name,
			"entity_version_id": entityVersionID,
			"icon_meta":         meta["icon_meta"],
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"total": len(items), "rows": items})
}
