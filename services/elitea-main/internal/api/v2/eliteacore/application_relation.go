package eliteacore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// The agent-as-tool relation, stored where every reader joins it: a row in
// `elitea_tools` (type='application', settings carrying the child pair) plus a
// row in `entity_tool_mapping` binding it to the parent version. This file is
// the ONE place that reads and writes that pair for sub-agent references —
// the publish/embed/unpublish flows and the PATCH route all go through it,
// because the previous arrangement (each site hand-writing statements against
// `application_tools`, a table that exists in no schema) made every one of
// them a silently-swallowed no-op.
type applicationToolReference struct {
	ToolID        int64
	Name          string
	ApplicationID string
	VersionID     string
}

// relationQuerier is satisfied by both *pgxpool.Pool and pgx.Tx, so the same
// statements serve the transactional attach and the best-effort embed copies.
type relationQuerier interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// applicationToolSettingsVersionSQL reads the child version id out of a tool
// row's settings. Two spellings exist in the wild: this branch writes
// `application_version_id` (the key the freeze admits), and rows written by
// the legacy runtime carry `version_id`. Readers accept both; writers emit
// only the new one.
const applicationToolSettingsVersionSQL = `COALESCE(tool.settings->>'application_version_id', tool.settings->>'version_id')`

// legacyApplicationToolsExists probes for the pylon-owned `application_tools`
// table. No migration in this repository creates it — it exists only in
// pylon-migrated tenants — so every touch of it is probe-guarded, the same
// arrangement internal/infra/db/repos/applications.go uses for its delete.
// to_regclass accepts the quoted identifier this package already carries.
func legacyApplicationToolsExists(ctx context.Context, q relationQuerier, schema string) (bool, error) {
	var name *string
	if err := q.QueryRow(ctx, `SELECT to_regclass($1)::text`,
		schema+".application_tools").Scan(&name); err != nil {
		return false, fmt.Errorf("probe application_tools: %w", err)
	}
	return name != nil, nil
}

// listApplicationToolReferences returns the sub-agent references attached to
// one parent version: the canonical rows (`elitea_tools` +
// `entity_tool_mapping`, what this service writes) first, then — on a
// pylon-migrated tenant only — the rows the legacy runtime left in
// `application_tools`.
func listApplicationToolReferences(ctx context.Context, q relationQuerier, schema, parentVersionID string) ([]applicationToolReference, error) {
	rows, err := q.Query(ctx, fmt.Sprintf(`
		SELECT tool.id, tool.name,
		       COALESCE(tool.settings->>'application_id', ''),
		       COALESCE(%s, '')
		FROM %s.entity_tool_mapping AS mapping
		JOIN %s.elitea_tools AS tool ON tool.id = mapping.tool_id
		WHERE mapping.entity_version_id = $1
		  AND mapping.entity_type = 'agent'
		  AND tool.type = 'application'
		ORDER BY tool.id`, applicationToolSettingsVersionSQL, schema, schema), parentVersionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var refs []applicationToolReference
	for rows.Next() {
		var ref applicationToolReference
		if err := rows.Scan(&ref.ToolID, &ref.Name, &ref.ApplicationID, &ref.VersionID); err != nil {
			return nil, err
		}
		refs = append(refs, ref)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	hasLegacy, err := legacyApplicationToolsExists(ctx, q, schema)
	if err != nil || !hasLegacy {
		return refs, err
	}
	legacyRows, err := q.Query(ctx, fmt.Sprintf(`
		SELECT tool.id, tool.name,
		       COALESCE(tool.settings->>'application_id', ''),
		       COALESCE(%s, '')
		FROM %s.application_tools AS tool
		WHERE tool.application_version_id = $1
		  AND tool.type = 'application'
		ORDER BY tool.id`, applicationToolSettingsVersionSQL, schema), parentVersionID)
	if err != nil {
		return nil, err
	}
	defer legacyRows.Close()
	for legacyRows.Next() {
		var ref applicationToolReference
		if err := legacyRows.Scan(&ref.ToolID, &ref.Name, &ref.ApplicationID, &ref.VersionID); err != nil {
			return nil, err
		}
		refs = append(refs, ref)
	}
	return refs, legacyRows.Err()
}

// eliteaToolsHasOwnerID reports whether this tenant's `elitea_tools` carries
// the owner_id column. A schema built by 001_initial.sql has it NOT NULL; a
// schema built by the legacy runtime has no such column, so the INSERT must
// name it conditionally — the same probe-and-branch the toolkit repository
// uses (internal/api/v2/toolkits, createToolkitInsertSQL). The probe compares
// information_schema's RAW schema name, never the quoted identifier.
func eliteaToolsHasOwnerID(ctx context.Context, q relationQuerier, ownerProjectID int) (bool, error) {
	var exists bool
	err := q.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM information_schema.columns
			WHERE table_schema = $1
			  AND table_name = 'elitea_tools'
			  AND column_name = 'owner_id')`, fmt.Sprintf("p_%d", ownerProjectID)).Scan(&exists)
	if err != nil {
		return false, fmt.Errorf("read elitea_tools shape: %w", err)
	}
	return exists, nil
}

// insertApplicationToolReference writes one sub-agent reference: the tool row
// and its mapping to the parent version. The settings shape is the freeze's
// contract (freezeCurrentStoredApplicationReference admits exactly
// {application_id, application_version_id}, as NUMBERS).
func insertApplicationToolReference(
	ctx context.Context, q relationQuerier, schema string,
	parentVersionID int64, name string,
	childAppID, childVersionID int64,
	ownerProjectID int, includeOwnerID bool, authorID int,
) error {
	settingsJSON, err := json.Marshal(map[string]any{
		"application_id":         childAppID,
		"application_version_id": childVersionID,
	})
	if err != nil {
		return fmt.Errorf("encode sub-agent settings: %w", err)
	}
	var toolID int64
	if includeOwnerID {
		err = q.QueryRow(ctx, fmt.Sprintf(`
			INSERT INTO %s.elitea_tools (name, type, description, settings, meta, owner_id, author_id)
			VALUES ($1, 'application', '', $2, '{}'::jsonb, $3, $4)
			RETURNING id`, schema),
			name, settingsJSON, ownerProjectID, authorID).Scan(&toolID)
	} else {
		err = q.QueryRow(ctx, fmt.Sprintf(`
			INSERT INTO %s.elitea_tools (name, type, description, settings, meta, author_id)
			VALUES ($1, 'application', '', $2, '{}'::jsonb, $3)
			RETURNING id`, schema),
			name, settingsJSON, authorID).Scan(&toolID)
	}
	if err != nil {
		return fmt.Errorf("insert sub-agent tool row: %w", err)
	}
	if _, err := q.Exec(ctx, fmt.Sprintf(`
		INSERT INTO %s.entity_tool_mapping (entity_version_id, entity_type, tool_id, entity_id)
		SELECT version.id, 'agent', $2, version.application_id
		FROM %s.application_versions AS version
		WHERE version.id = $1`, schema, schema),
		parentVersionID, toolID); err != nil {
		return fmt.Errorf("insert sub-agent mapping: %w", err)
	}
	return nil
}

// deleteApplicationToolReferences removes sub-agent references from one
// parent version and returns how many mappings it removed. When childAppID
// and childVersionID are non-empty only that pair is detached; empty strings
// detach every sub-agent reference of the version.
//
// The mapping goes first and the tool row goes second, and the tool row goes
// ONLY when nothing references it any more: publish CLONES mappings onto the
// published version reusing the same tool_id, and `entity_tool_mapping.tool_id`
// cascades on tool deletion — so the old single-statement "delete the tool
// row" detach silently stripped the child from every published clone as well.
func deleteApplicationToolReferences(ctx context.Context, q relationQuerier, schema, parentVersionID, childAppID, childVersionID string) (int64, error) {
	rows, err := q.Query(ctx, fmt.Sprintf(`
		DELETE FROM %s.entity_tool_mapping AS mapping
		USING %s.elitea_tools AS tool
		WHERE mapping.tool_id = tool.id
		  AND mapping.entity_version_id = $1
		  AND mapping.entity_type = 'agent'
		  AND tool.type = 'application'
		  AND ($2 = '' OR tool.settings->>'application_id' = $2)
		  AND ($3 = '' OR %s = $3)
		RETURNING mapping.tool_id`, schema, schema, applicationToolSettingsVersionSQL),
		parentVersionID, childAppID, childVersionID)
	if err != nil {
		return 0, err
	}
	toolIDs := []int64{}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return 0, err
		}
		toolIDs = append(toolIDs, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	removed := int64(len(toolIDs))
	if removed > 0 {
		// A second statement, not a CTE: a data-modifying CTE's outer query
		// reads the pre-delete snapshot, so it would still see the mappings
		// removed above and never collect the now-orphaned tool rows.
		if _, err := q.Exec(ctx, fmt.Sprintf(`
			DELETE FROM %s.elitea_tools AS tool
			WHERE tool.id = ANY($1)
			  AND NOT EXISTS (
				SELECT 1 FROM %s.entity_tool_mapping AS other
				WHERE other.tool_id = tool.id)`, schema, schema), toolIDs); err != nil {
			return 0, err
		}
	}

	// The legacy leg, probe-guarded like the reads: a pylon-migrated tenant
	// may hold the same reference in `application_tools`, and leaving it
	// there would resurrect the relation through the union read.
	hasLegacy, err := legacyApplicationToolsExists(ctx, q, schema)
	if err != nil {
		return removed, err
	}
	if hasLegacy {
		tag, err := q.Exec(ctx, fmt.Sprintf(`
			DELETE FROM %s.application_tools AS tool
			WHERE tool.application_version_id = $1
			  AND tool.type = 'application'
			  AND ($2 = '' OR tool.settings->>'application_id' = $2)
			  AND ($3 = '' OR %s = $3)`, schema, applicationToolSettingsVersionSQL),
			parentVersionID, childAppID, childVersionID)
		if err != nil {
			return removed, err
		}
		removed += tag.RowsAffected()
	}
	return removed, nil
}

// applicationRelationRequest is the PATCH body, typed so a numeric id is an
// integer end to end. The previous map[string]any body put ids through
// fmt.Sprintf("%v", float64), which renders 1234567 as "1.234567e+06" and a
// missing id as "<nil>" — both refused by Postgres, both surfacing as 500s.
//
// The contract (v2.yaml ApplicationRelationUpdateRequest) and legacy pylon
// both accept an id as a JSON number OR a numeric string ("42"). A plain
// *int64 refused the string form with 400, so each id decodes through
// relationID, which takes either form and still refuses anything else.
type applicationRelationRequest struct {
	ApplicationID *relationID `json:"application_id"`
	VersionID     *relationID `json:"version_id"`
	HasRelation   bool        `json:"has_relation"`
}

// relationID is a positive id sent as a JSON number or a numeric string. A
// JSON null leaves the pointer field nil (encoding/json never calls the
// method for null), so the "required" check below still sees it as missing.
type relationID int64

var errInvalidRelationID = errors.New("relation id must be a positive integer or numeric string")

func (id *relationID) UnmarshalJSON(raw []byte) error {
	value, present, err := decodeOptionalPositiveInt32(raw)
	if err != nil || !present {
		return errInvalidRelationID
	}
	*id = relationID(value)
	return nil
}

// UpdateApplicationRelation attaches (or detaches) the CHILD agent named by
// the URL as a tool of the PARENT version named by the body.
//
// The old body of this handler wrote to `application_tools` — a table that
// exists in NO tenant schema — with `_, _ =` best-effort execs, so it
// answered 201 while inserting nothing; it was also unreachable, because the
// router bound PATCH to the READ handler. Both halves are fixed together.
func (h *Handler) UpdateApplicationRelation(w http.ResponseWriter, r *http.Request) {
	projectID := chi.URLParam(r, "projectID")
	childAppID := chi.URLParam(r, "appID")
	childVersionID := chi.URLParam(r, "versionID")
	s, schemaOK := tenantSchema(w, projectID)
	if !schemaOK {
		return
	}
	ctx := r.Context()

	var body applicationRelationRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid request body"})
		return
	}
	// Both directions address one parent version. Refusing the incomplete
	// body here matters for the attach in particular: the old guard sent an
	// attach with a missing application_id down the DETACH branch, which
	// deleted the relation and answered success.
	if body.VersionID == nil || body.ApplicationID == nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error": "application_id and version_id are required",
		})
		return
	}
	parentVersionID := int64(*body.VersionID)

	ownerProjectID, err := strconv.Atoi(projectID)
	if err != nil || ownerProjectID <= 0 {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid project id"})
		return
	}

	transaction, err := h.pool.Begin(ctx)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "relation write failed"})
		return
	}
	defer func() { _ = transaction.Rollback(ctx) }()

	// One row lock serves three purposes: it proves the parent version
	// exists, it blocks changes to published/embedded versions, and it
	// serializes concurrent attaches of the same parent — the duplicate
	// check below runs under this lock, which is what makes it effective
	// (every attach mints a fresh tool_id, so no unique constraint can
	// catch the race after the fact).
	var parentStatus string
	var parentApplicationID int64
	err = transaction.QueryRow(ctx, fmt.Sprintf(
		`SELECT COALESCE(status, ''), application_id FROM %s.application_versions WHERE id = $1 FOR UPDATE`, s),
		parentVersionID).Scan(&parentStatus, &parentApplicationID)
	if err != nil {
		if err == pgx.ErrNoRows {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "the parent version does not exist"})
			return
		}
		slog.ErrorContext(ctx, "application relation: parent version read failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "relation lookup failed"})
		return
	}
	// The body names the parent twice. A version of ANOTHER application must
	// not be edited because a client sent a stale or wrong application_id.
	if parentApplicationID != int64(*body.ApplicationID) {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error": "the parent version does not belong to application_id",
		})
		return
	}
	if parentStatus == "published" || parentStatus == "embedded" {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error": "Cannot change relation on a published version. Unpublish first.",
		})
		return
	}

	if body.HasRelation {
		// The union read, not a bespoke EXISTS: a pylon-migrated tenant can
		// hold the same reference in the legacy table, and attaching a second
		// copy on top of it would double the child in every reader.
		existing, err := listApplicationToolReferences(
			ctx, transaction, s, strconv.FormatInt(parentVersionID, 10))
		if err != nil {
			slog.ErrorContext(ctx, "application relation: duplicate check failed", "error", err)
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "relation lookup failed"})
			return
		}
		for _, ref := range existing {
			if ref.ApplicationID == childAppID && ref.VersionID == childVersionID {
				writeJSON(w, http.StatusBadRequest, map[string]any{"error": "relation already exists"})
				return
			}
		}

		// The child's name and author, read rather than synthesised: the
		// resolver projection serves `name`/`toolkit_name` straight from
		// this row, and the freeze refuses a reference whose names disagree.
		var childName string
		var childAuthorID int
		err = transaction.QueryRow(ctx, fmt.Sprintf(`
			SELECT app.name, version.author_id
			FROM %s.applications AS app
			JOIN %s.application_versions AS version ON version.application_id = app.id
			WHERE app.id = $1 AND version.id = $2`, s, s),
			childAppID, childVersionID).Scan(&childName, &childAuthorID)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "the selected agent version does not exist"})
			return
		}

		childAppNumber, err := strconv.ParseInt(childAppID, 10, 64)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid agent id"})
			return
		}
		childVersionNumber, err := strconv.ParseInt(childVersionID, 10, 64)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid agent version id"})
			return
		}

		includeOwnerID, err := eliteaToolsHasOwnerID(ctx, transaction, ownerProjectID)
		if err != nil {
			slog.ErrorContext(ctx, "application relation: table shape probe failed", "error", err)
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "relation write failed"})
			return
		}
		if err := insertApplicationToolReference(
			ctx, transaction, s, parentVersionID, childName,
			childAppNumber, childVersionNumber,
			ownerProjectID, includeOwnerID, childAuthorID,
		); err != nil {
			slog.ErrorContext(ctx, "application relation: attach failed", "error", err)
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "relation write failed"})
			return
		}
	} else {
		removed, err := deleteApplicationToolReferences(
			ctx, transaction, s, strconv.FormatInt(parentVersionID, 10), childAppID, childVersionID)
		if err != nil {
			slog.ErrorContext(ctx, "application relation: detach failed", "error", err)
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "relation delete failed"})
			return
		}
		if removed == 0 {
			// A zero-row detach used to answer success; the row the client
			// believed it removed then reappeared on reload.
			writeJSON(w, http.StatusNotFound, map[string]any{"error": "no such relation on this version"})
			return
		}
	}

	if err := transaction.Commit(ctx); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "relation write failed"})
		return
	}

	writeJSON(w, http.StatusCreated, map[string]any{
		"application_id": childAppID,
		"version_id":     childVersionID,
		"has_relation":   body.HasRelation,
	})
}

// jsonNumberIfNumeric turns a stored id string into the NUMBER the freeze's
// settings contract expects, passing non-numeric input through untouched so a
// malformed legacy row degrades to the string it actually holds.
func jsonNumberIfNumeric(value string) any {
	if number, err := strconv.ParseInt(value, 10, 64); err == nil {
		return number
	}
	return value
}
