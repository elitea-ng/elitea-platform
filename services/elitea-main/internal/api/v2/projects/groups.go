package projects

// Project GROUPS — the membership between `centry.project` and
// `centry.project_group`.
//
// Until this file the Go surface was read-and-echo: `GET /groups/prompt_lib`
// listed the groups (handler.go) and `PUT /groups/prompt_lib/{projectID}`
// decoded the request body and wrote it straight back out as the response
// without touching a table. So every group control in the product reported
// success and changed nothing — the #128/#130 shape — and create and delete
// had no route at all.
//
// The contract is the legacy one, not invented:
//
//	legacy/plugins/projects/api/v2/group.py   POST <project_id>            → 201, the project
//	                                          DELETE <project_id>/<group_id> → 204
//	legacy/plugins/projects/api/v2/groups.py  PUT  <project_id>            → 200, the project
//
// All three answer with the SAME serialized project (`ProjectListModel`), which
// is the `Project` struct in handler.go, so the client re-renders the project's
// group chips from the response rather than guessing.
//
// Two deliberate divergences, both the "200 that did less than it said" shape
// this package is fixing:
//
//   - The legacy DELETE swallows `ValueError` when the group is not associated
//     with the project and still answers 204. Here a group that is not attached
//     is a 404, so "detach" cannot silently mean "nothing happened".
//   - The legacy POST/PUT run several statements on an autocommitting session
//     and can leave a group row created but not associated. Each write here is
//     one transaction.

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
)

// reservedGroupName is the legacy `GroupCreateModel.check_no_group_name`
// validator: `no_group` is the sentinel the listing uses for projects with no
// group at all, so a real group by that name would make the two
// indistinguishable.
const reservedGroupName = "no_group"

// GroupCreate serves `POST /api/v2/projects/group/prompt_lib/{projectID}` —
// "create this group if it does not exist, and attach it to this project".
func (h *Handler) GroupCreate(w http.ResponseWriter, r *http.Request) {
	projectID, ok := h.groupWriteContext(w, r)
	if !ok {
		return
	}

	var body struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "Can not validate data"})
		return
	}
	name := strings.TrimSpace(body.Name)
	if name == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "Can not validate data"})
		return
	}
	if name == reservedGroupName {
		writeJSON(w, http.StatusBadRequest,
			map[string]any{"error": `Group with name "no_group" can not be created`})
		return
	}

	ctx := r.Context()
	transaction, err := h.pool.Begin(ctx)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "internal server error"})
		return
	}
	defer func() { _ = transaction.Rollback(ctx) }()

	var exists bool
	if err := transaction.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM centry.project WHERE id = $1)`, projectID).Scan(&exists); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "internal server error"})
		return
	}
	if !exists {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "Project was not found"})
		return
	}

	// Reuse the group when the name already exists — the legacy runtime looks
	// the name up before constructing one, and `centry.project_group.name` is
	// UNIQUE, so creating unconditionally would fail the second project to ask
	// for it.
	//
	// INSERT-then-SELECT rather than SELECT-then-INSERT: the latter leaves a
	// window in which two concurrent creates of the same NEW name both see no
	// row, both insert, and the loser gets a unique violation this handler can
	// only report as a 500. `ON CONFLICT DO NOTHING` closes it — a concurrent
	// uncommitted insert blocks until it commits, then returns no row and the
	// re-select finds the committed one.
	var groupID int
	err = transaction.QueryRow(ctx, `
INSERT INTO centry.project_group (name) VALUES ($1)
ON CONFLICT (name) DO NOTHING
RETURNING id`, name).Scan(&groupID)
	if errors.Is(err, pgx.ErrNoRows) {
		err = transaction.QueryRow(ctx,
			`SELECT id FROM centry.project_group WHERE name = $1`, name).Scan(&groupID)
	}
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "internal server error"})
		return
	}

	// `NOT EXISTS` rather than `ON CONFLICT`: the association table carries a
	// primary key in the bootstrap schema but none in the current legacy
	// baseline (internal/db/schema/centry_projects_baseline.sql), and an
	// `ON CONFLICT` with no matching constraint is a runtime error.
	if _, err := transaction.Exec(ctx, `
INSERT INTO centry.project_group_association (project_id, group_id)
SELECT $1, $2
WHERE NOT EXISTS (
    SELECT 1 FROM centry.project_group_association
    WHERE project_id = $1 AND group_id = $2
)`, projectID, groupID); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "internal server error"})
		return
	}

	if err := transaction.Commit(ctx); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "internal server error"})
		return
	}
	h.writeProject(w, r, projectID, http.StatusCreated)
}

// GroupDelete serves
// `DELETE /api/v2/projects/group/prompt_lib/{projectID}/{groupID}` — detach one
// group from one project. The group row itself survives: it may be attached to
// other projects, and the legacy runtime removes only the association.
func (h *Handler) GroupDelete(w http.ResponseWriter, r *http.Request) {
	projectID, ok := h.groupWriteContext(w, r)
	if !ok {
		return
	}
	groupID, err := strconv.ParseInt(chi.URLParam(r, "groupID"), 10, 32)
	if err != nil || groupID <= 0 {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid group id"})
		return
	}

	ctx := r.Context()
	var projectExists, groupExists bool
	if err := h.pool.QueryRow(ctx, `
SELECT EXISTS (SELECT 1 FROM centry.project WHERE id = $1),
       EXISTS (SELECT 1 FROM centry.project_group WHERE id = $2)`,
		projectID, groupID).Scan(&projectExists, &groupExists); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "internal server error"})
		return
	}
	if !projectExists || !groupExists {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "Project or Group not found"})
		return
	}

	tag, err := h.pool.Exec(ctx,
		`DELETE FROM centry.project_group_association WHERE project_id = $1 AND group_id = $2`,
		projectID, groupID)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "internal server error"})
		return
	}
	// The legacy runtime answers 204 here as well, having caught the
	// `ValueError` its `list.remove` raises. A detach that detached nothing is
	// reported as such.
	if tag.RowsAffected() == 0 {
		writeJSON(w, http.StatusNotFound,
			map[string]any{"error": "the group is not attached to this project"})
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// PutProjectGroups serves `PUT /api/v2/projects/groups/prompt_lib/{projectID}`
// — REPLACE this project's group set with the named groups, creating any that
// do not exist yet (the legacy groups.py `put`).
//
// It used to decode the body and echo it back, so the project's groups never
// moved. The response is the project, as the legacy runtime serializes it.
func (h *Handler) PutProjectGroups(w http.ResponseWriter, r *http.Request) {
	projectID, ok := h.groupWriteContext(w, r)
	if !ok {
		return
	}

	var body struct {
		Groups []string `json:"groups"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Groups == nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "Can not validate data"})
		return
	}

	names := make([]string, 0, len(body.Groups))
	seen := make(map[string]struct{}, len(body.Groups))
	for _, raw := range body.Groups {
		name := strings.TrimSpace(raw)
		if name == "" {
			continue
		}
		if name == reservedGroupName {
			writeJSON(w, http.StatusBadRequest,
				map[string]any{"error": `Group with name "no_group" can not be created`})
			return
		}
		if _, duplicate := seen[name]; duplicate {
			continue
		}
		seen[name] = struct{}{}
		names = append(names, name)
	}

	ctx := r.Context()
	transaction, err := h.pool.Begin(ctx)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "internal server error"})
		return
	}
	defer func() { _ = transaction.Rollback(ctx) }()

	var exists bool
	if err := transaction.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM centry.project WHERE id = $1)`, projectID).Scan(&exists); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "internal server error"})
		return
	}
	if !exists {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "Project was not found"})
		return
	}

	if len(names) > 0 {
		// `ON CONFLICT` rather than `NOT EXISTS` for the same reason GroupCreate
		// uses it: two concurrent saves naming the same new group would
		// otherwise race the unique index and one would answer 500.
		if _, err := transaction.Exec(ctx, `
INSERT INTO centry.project_group (name)
SELECT candidate
FROM unnest($1::text[]) AS candidate
ON CONFLICT (name) DO NOTHING`, names); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "internal server error"})
			return
		}
	}

	// Replace, not merge: the submitted set is the project's whole membership,
	// so a group dropped from the list is detached. Both statements run in the
	// one transaction, so an interrupted save cannot leave the project with no
	// groups at all.
	if _, err := transaction.Exec(ctx, `
DELETE FROM centry.project_group_association association
USING centry.project_group grp
WHERE grp.id = association.group_id
  AND association.project_id = $1
  AND NOT (grp.name = ANY($2::text[]))`, projectID, names); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "internal server error"})
		return
	}
	if len(names) > 0 {
		if _, err := transaction.Exec(ctx, `
INSERT INTO centry.project_group_association (project_id, group_id)
SELECT $1, grp.id
FROM centry.project_group grp
WHERE grp.name = ANY($2::text[])
  AND NOT EXISTS (
    SELECT 1 FROM centry.project_group_association existing
    WHERE existing.project_id = $1 AND existing.group_id = grp.id
)`, projectID, names); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "internal server error"})
			return
		}
	}

	if err := transaction.Commit(ctx); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "internal server error"})
		return
	}
	h.writeProject(w, r, projectID, http.StatusOK)
}

// groupWriteContext performs the two checks every group write shares and writes
// the failure response itself.
//
// The id is parsed into int32 rather than int: `centry.project.id` is a
// `serial`, so anything outside int32 names no project, and `writeProject`
// carries the value into a field of that width. Parsing wide and narrowing
// later would silently truncate — id 4294967297 would address project 1.
func (h *Handler) groupWriteContext(w http.ResponseWriter, r *http.Request) (int32, bool) {
	if h.pool == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "database unavailable"})
		return 0, false
	}
	projectID, err := strconv.ParseInt(chi.URLParam(r, "projectID"), 10, 32)
	if err != nil || projectID <= 0 {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid project id"})
		return 0, false
	}
	return int32(projectID), true
}

// writeProject answers with the project and its groups — the body the legacy
// three group writes all return, and the reason a client does not have to
// re-fetch.
func (h *Handler) writeProject(w http.ResponseWriter, r *http.Request, projectID int32, status int) {
	ctx := r.Context()
	project := Project{ID: projectID, Groups: []Group{}, Plugins: []string{}}
	var keycloakGroups string
	err := h.pool.QueryRow(ctx, `
SELECT project.name, project.owner_id, COALESCE(project.plugins, '{}'),
       COALESCE(project.keycloak_groups::text, '{}'),
       project.create_success, project.suspended
FROM centry.project project
WHERE project.id = $1`, projectID).Scan(
		&project.Name, &project.OwnerID, &project.Plugins,
		&keycloakGroups, &project.CreateSuccess, &project.Suspended)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "internal server error"})
		return
	}
	project.KeycloakGroups = json.RawMessage(keycloakGroups)

	rows, err := h.pool.Query(ctx, `
SELECT grp.id, grp.name
FROM centry.project_group_association association
JOIN centry.project_group grp ON grp.id = association.group_id
WHERE association.project_id = $1
ORDER BY grp.id`, projectID)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "internal server error"})
		return
	}
	defer rows.Close()
	for rows.Next() {
		var group Group
		if err := rows.Scan(&group.ID, &group.Name); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "internal server error"})
			return
		}
		project.Groups = append(project.Groups, group)
	}
	if err := rows.Err(); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "internal server error"})
		return
	}
	writeJSON(w, status, project)
}
