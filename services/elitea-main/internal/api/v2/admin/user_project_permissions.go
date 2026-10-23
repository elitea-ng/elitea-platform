package admin

// The admin console's PERSONAL/TEAM project permission editor —
// `/admin/user_project_permissions/administration`.
//
// It is the same matrix roles.go serves, addressed differently. roles.go edits
// ONE named scope (the central matrix, or the public/support project). This
// edits the DEFAULT every ordinary project gets: the read reports what a
// personal project grants, and the write applies a matrix across EVERY personal
// project at once — or, with `?team_projects`, across every shared project.
//
// Legacy: legacy/plugins/admin/api/v2/user_project_permissions.py.
//
//	GET  [?old_format]                                        → role → permissions
//	PUT  [?team_projects][?create_role_if_not_exist][?append_user_role]
//
// Both body shapes the reference accepts are accepted here:
//
//	{"editor": ["a.b.c", …], …}                    the role map
//	[{"name": "a.b.c", "editor": true, …}, …]      the matrix rows the GET's
//	                                               ?old_format returns
//
// Deliberate divergences, each of them a "reported success, wrote nothing" the
// reference has and this does not:
//
//   - the reference's PUT walks projects one at a time on autocommitting
//     sessions, so an interruption leaves some projects on the new matrix and
//     some on the old — and, because it DELETES a role's permissions before
//     re-inserting, a project caught mid-write is left with a role that grants
//     nothing. Everything here runs in ONE transaction.
//   - a role name that some target project does not define is silently skipped
//     FOR THAT PROJECT by the reference (its
//     `session.query(Role).where(Role.name.in_(…))` simply matches nothing)
//     unless `create_role_if_not_exist` is set, so a save that reached half the
//     estate reports success. Here it is a 400 naming the role, unless
//     `create_role_if_not_exist` asks for it to be created — and the check is
//     per (project, role), not "does any project define it": a role present in
//     one personal project and absent from another is exactly the case that
//     produces a half-applied save.
//
//     `system` is the one role name that is neither written nor rejected. It is
//     the platform's own role, it appears in every body this endpoint's own GET
//     produces (every project the legacy runtime creates has one —
//     projects/utils/ project_steps.py assigns `role_name='system'` to its
//     system user), and rejecting it made the GET → edit → PUT round trip
//     impossible. roles.go drops it from the submission the same way.
//   - the reference reads the role map through its admin RPC, which returns the
//     CENTRAL default-mode grants filtered by project role name
//     (admin/rpc/roles.py `get_permissions`) — i.e. it reports the central
//     matrix even when the project has overrides of its own. The read here is
//     the project's own overrides, falling back to the central `default` matrix
//     only when the project has none, which is what its members actually have
//     (the same rule roles.go's projectGrants applies).

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"

	"github.com/jackc/pgx/v5"
)

const (
	// UserProjectPermissionsViewPermission and …EditPermission are what the
	// legacy originals declare in their check_api decorators.
	UserProjectPermissionsViewPermission = "configuration.roles.user_project_permissions.view"
	UserProjectPermissionsEditPermission = "configuration.roles.user_project_permissions.edit"
)

/* ── read ──────────────────────────────────────────────────────────────── */

// UserProjectPermissions serves
// `GET /admin/user_project_permissions/administration`.
//
// Default body: `{"<role>": ["<permission>", …], …}`, sorted. With
// `?old_format` it is the matrix `{total, rows:[{name, <role>: bool, …}]}` —
// the shape roles.go returns and the shape this endpoint's PUT accepts back.
func (h *Handler) UserProjectPermissions(w http.ResponseWriter, r *http.Request) {
	if h.pool == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "database unavailable"})
		return
	}
	ctx := r.Context()

	personal, err := h.personalProjectIDs(ctx)
	if err != nil {
		writeMatrixError(w, err)
		return
	}
	if len(personal) == 0 {
		// The reference's own status and message: with no personal project
		// there is no template to report.
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "Personal projects not set"})
		return
	}
	projectID := personal[0]

	roles, err := h.projectRoles(ctx, projectID)
	if err != nil {
		writeMatrixError(w, err)
		return
	}
	granted, _, err := h.projectGrants(ctx, projectID, roles)
	if err != nil {
		writeMatrixError(w, err)
		return
	}

	if _, wantsMatrix := r.URL.Query()["old_format"]; !wantsMatrix {
		roleMap := make(map[string][]string, len(roles))
		for _, role := range roles {
			permissions := make([]string, 0, len(granted[role]))
			for permission := range granted[role] {
				permissions = append(permissions, permission)
			}
			sort.Strings(permissions)
			roleMap[role] = permissions
		}
		writeJSON(w, http.StatusOK, roleMap)
		return
	}

	catalogue, err := h.permissionCatalogue(ctx)
	if err != nil {
		writeMatrixError(w, err)
		return
	}
	matrix := permissionMatrix{
		roles: roles, catalogue: catalogue, granted: granted, projectID: projectID,
	}
	writeJSON(w, http.StatusOK, matrix.response())
}

/* ── write ─────────────────────────────────────────────────────────────── */

// UserProjectPermissionsSave serves
// `PUT /admin/user_project_permissions/administration`.
func (h *Handler) UserProjectPermissionsSave(w http.ResponseWriter, r *http.Request) {
	if h.pool == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "database unavailable"})
		return
	}
	ctx := r.Context()
	query := r.URL.Query()
	_, forTeamProjects := query["team_projects"]
	_, createMissingRoles := query["create_role_if_not_exist"]
	_, appendUserRole := query["append_user_role"]

	roleMap, submittedRows, err := decodeRoleMap(r)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	// `system` is dropped rather than rejected — see this file's header. It is
	// in every body the GET above produces, so rejecting it made the endpoint's
	// own output unsavable; and it is never written, so a caller cannot use
	// this route to grant the platform's own role anything.
	delete(roleMap, roleSystem)
	if len(roleMap) == 0 {
		writeJSON(w, http.StatusBadRequest,
			map[string]any{"error": "the submitted matrix names no writable roles"})
		return
	}

	catalogue, err := h.permissionCatalogue(ctx)
	if err != nil {
		writeMatrixError(w, err)
		return
	}
	known := make(map[string]bool, len(catalogue))
	for _, permission := range catalogue {
		known[permission] = true
	}
	for _, permissions := range roleMap {
		for _, permission := range permissions {
			if !known[permission] {
				writeJSON(w, http.StatusBadRequest,
					map[string]any{"error": fmt.Sprintf("unknown permission %q", permission)})
				return
			}
		}
	}

	targets, err := h.targetProjectIDs(ctx, forTeamProjects)
	if err != nil {
		writeMatrixError(w, err)
		return
	}
	if len(targets) == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "Personal projects not set"})
		return
	}

	result, err := h.applyRoleMap(
		ctx, targets, roleMap, submittedRows, createMissingRoles, appendUserRole)
	if err != nil {
		writeMatrixError(w, err)
		return
	}

	// `role_map` is the reference's response field, kept so its clients keep
	// working; the counters are what make a no-op distinguishable from a save.
	writeJSON(w, http.StatusOK, map[string]any{
		"role_map":       roleMap,
		"projects":       len(targets),
		"roles_created":  result.rolesCreated,
		"granted":        result.granted,
		"revoked":        result.revoked,
		"roles_assigned": result.rolesAssigned,
	})
}

type roleMapResult struct {
	rolesCreated  int64
	granted       int64
	revoked       int64
	rolesAssigned int64
}

// applyRoleMap writes the whole submission across every target project in ONE
// transaction.
//
// `submittedRows` bounds REVOCATION to the permissions the body actually
// carried, and is nil for the role-map body shape, which is unbounded by
// definition (the map IS the role's whole permission set — that is what the
// reference's delete-then-insert means). For the MATRIX shape it is the set of
// permission names the rows named, so a client that submits a filtered or
// paged view of the matrix cannot silently revoke every permission it did not
// mention. roles.go's diffGrants scopes the identically-shaped body the same
// way, and two adjacent endpoints disagreeing about that would be a trap for
// any client code shared between them.
func (h *Handler) applyRoleMap(
	ctx context.Context,
	projects []int,
	roleMap map[string][]string,
	submittedRows []string,
	createMissingRoles bool,
	appendUserRole bool,
) (roleMapResult, error) {
	roleNames := make([]string, 0, len(roleMap))
	for role := range roleMap {
		roleNames = append(roleNames, role)
	}
	sort.Strings(roleNames)

	transaction, err := h.pool.Begin(ctx)
	if err != nil {
		return roleMapResult{}, fmt.Errorf("begin user-project permission update: %w", err)
	}
	defer func() { _ = transaction.Rollback(ctx) }()

	var result roleMapResult
	if createMissingRoles {
		tag, err := transaction.Exec(ctx, `
INSERT INTO public.auth_core__project_role (project_id, name)
SELECT project_id, role_name
FROM unnest($1::int[]) AS project_id
CROSS JOIN unnest($2::text[]) AS role_name
ON CONFLICT (project_id, name) DO NOTHING`, projects, roleNames)
		if err != nil {
			return roleMapResult{}, fmt.Errorf("create project roles: %w", err)
		}
		result.rolesCreated = tag.RowsAffected()
	} else {
		// Without the flag, a role some target project does not define would be
		// silently dropped FOR THAT PROJECT while the save reported success for
		// the whole estate. Report it instead.
		missing, err := missingRoleNames(ctx, transaction, projects, roleNames)
		if err != nil {
			return roleMapResult{}, err
		}
		if len(missing) > 0 {
			return roleMapResult{}, matrixError{
				status: http.StatusBadRequest,
				message: fmt.Sprintf(
					"not every target project defines the role(s) %v; "+
						"pass ?create_role_if_not_exist to create them",
					missing),
			}
		}
	}

	// Revoke first, then grant — the submitted set is the role's WHOLE
	// permission set, exactly as the reference's delete-then-insert intends.
	// Unlike the reference, both halves commit together, so a role can never be
	// observed with an empty permission set it was not given.
	for _, role := range roleNames {
		permissions := roleMap[role]
		removed, err := transaction.Exec(ctx, `
DELETE FROM public.auth_core__project_role_permission override
USING public.auth_core__project_role role
WHERE role.id = override.role_id
  AND role.project_id = ANY($1::int[])
  AND role.name = $2
  AND NOT (override.permission = ANY($3::text[]))
  AND ($4::text[] IS NULL OR override.permission = ANY($4::text[]))`,
			projects, role, permissions, submittedRows)
		if err != nil {
			return roleMapResult{}, fmt.Errorf("revoke permissions for %s: %w", role, err)
		}
		result.revoked += removed.RowsAffected()

		if len(permissions) == 0 {
			continue
		}
		added, err := transaction.Exec(ctx, `
INSERT INTO public.auth_core__project_role_permission (project_id, role_id, permission)
SELECT role.project_id, role.id, permission
FROM public.auth_core__project_role role
CROSS JOIN unnest($3::text[]) AS permission
WHERE role.project_id = ANY($1::int[])
  AND role.name = $2
ON CONFLICT (project_id, role_id, permission) DO NOTHING`, projects, role, permissions)
		if err != nil {
			return roleMapResult{}, fmt.Errorf("grant permissions for %s: %w", role, err)
		}
		result.granted += added.RowsAffected()
	}

	if appendUserRole {
		// Every existing member of a target project gains any role in the map
		// they do not already hold — the reference's `append_user_role`, which
		// walks `get_users_roles_in_project` and re-writes each user's role
		// list. Set-based here, and it only ADDS: nobody loses a role.
		tag, err := transaction.Exec(ctx, `
INSERT INTO public.auth_core__project_user_role (project_id, user_id, role_id)
SELECT role.project_id, member.user_id, role.id
FROM public.auth_core__project_role role
JOIN (
    SELECT DISTINCT project_id, user_id
    FROM public.auth_core__project_user_role
    WHERE project_id = ANY($1::int[])
) AS member ON member.project_id = role.project_id
WHERE role.project_id = ANY($1::int[])
  AND role.name = ANY($2::text[])
ON CONFLICT (project_id, user_id, role_id) DO NOTHING`, projects, roleNames)
		if err != nil {
			return roleMapResult{}, fmt.Errorf("append user roles: %w", err)
		}
		result.rolesAssigned = tag.RowsAffected()
	}

	if err := transaction.Commit(ctx); err != nil {
		return roleMapResult{}, fmt.Errorf("commit user-project permission update: %w", err)
	}
	return result, nil
}

// missingRoleNames reports a role that is not defined by EVERY target project.
// "Defined by at least one" is not enough: the write matches role rows per
// project, so a role present in one personal project and absent from another
// produces a save that reached half the estate and reported success.
func missingRoleNames(
	ctx context.Context, transaction pgx.Tx, projects []int, roleNames []string,
) ([]string, error) {
	rows, err := transaction.Query(ctx, `
SELECT candidate
FROM unnest($2::text[]) AS candidate
WHERE (
    SELECT COUNT(DISTINCT role.project_id)
    FROM public.auth_core__project_role role
    WHERE role.project_id = ANY($1::int[]) AND role.name = candidate
) < cardinality($1::int[])
ORDER BY 1`, projects, roleNames)
	if err != nil {
		return nil, fmt.Errorf("check project roles: %w", err)
	}
	defer rows.Close()
	return scanNames(rows, "role")
}

/* ── project selection ─────────────────────────────────────────────────── */

// personalProjectIDs returns the per-user projects, `project_user_<id>`.
//
// The LIKE pattern escapes its underscores — it reproduces Python's literal
// `name.startswith('project_user_')`, not SQLAlchemy's `.like()`. See
// projectUserNameLikePattern in roles.go.
func (h *Handler) personalProjectIDs(ctx context.Context) ([]int, error) {
	rows, err := h.pool.Query(ctx, `
SELECT id FROM centry.project
WHERE name LIKE '`+projectUserNameLikePattern+`' ESCAPE '\'
ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("list personal projects: %w", err)
	}
	defer rows.Close()
	return scanIDs(rows)
}

// targetProjectIDs picks what a PUT writes to: every personal project, or —
// with `?team_projects` — every project that is neither personal nor the
// public/AI project, which is exactly the reference's split.
func (h *Handler) targetProjectIDs(ctx context.Context, forTeamProjects bool) ([]int, error) {
	if !forTeamProjects {
		return h.personalProjectIDs(ctx)
	}
	publicProjectID, err := envProjectID("AI_PROJECT_ID", defaultPublicProjectID)
	if err != nil {
		return nil, err
	}
	rows, err := h.pool.Query(ctx, `
SELECT id FROM centry.project
WHERE name NOT LIKE '`+projectUserNameLikePattern+`' ESCAPE '\'
  AND id <> $1
ORDER BY id`, publicProjectID)
	if err != nil {
		return nil, fmt.Errorf("list team projects: %w", err)
	}
	defer rows.Close()
	return scanIDs(rows)
}

func scanIDs(rows pgx.Rows) ([]int, error) {
	ids := []int{}
	for rows.Next() {
		var id int
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan project id: %w", err)
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

/* ── body decoding ─────────────────────────────────────────────────────── */

// decodeRoleMap accepts both shapes the reference accepts and normalises them
// to role → permissions.
//
// The second return value is the set of permission names the body NAMED, and
// it bounds revocation (see applyRoleMap). It is nil for the role-map shape,
// where the submitted list is the role's whole permission set by definition,
// and the submitted rows for the matrix shape, where it is not.
func decodeRoleMap(r *http.Request) (map[string][]string, []string, error) {
	var raw json.RawMessage
	if err := json.NewDecoder(r.Body).Decode(&raw); err != nil {
		return nil, nil, fmt.Errorf("invalid request body: expected a role map or an array of permission rows")
	}

	var asMap map[string][]string
	if err := json.Unmarshal(raw, &asMap); err == nil {
		normalized := make(map[string][]string, len(asMap))
		for role, permissions := range asMap {
			if role == "" {
				return nil, nil, fmt.Errorf("a role name is empty")
			}
			normalized[role] = dedupeSorted(permissions)
		}
		return normalized, nil, nil
	}

	var asRows []map[string]json.RawMessage
	if err := json.Unmarshal(raw, &asRows); err != nil {
		return nil, nil, fmt.Errorf("invalid request body: expected a role map or an array of permission rows")
	}
	roleMap := map[string][]string{}
	submitted := make([]string, 0, len(asRows))
	for index, row := range asRows {
		permission, err := rowPermissionName(row, index)
		if err != nil {
			return nil, nil, err
		}
		submitted = append(submitted, permission)
		for column, cell := range row {
			if column == "name" {
				continue
			}
			var enabled bool
			if err := json.Unmarshal(cell, &enabled); err != nil {
				return nil, nil, fmt.Errorf("row %d: %q must be a boolean", index, column)
			}
			// Every role COLUMN is registered even when the cell is false, so a
			// matrix that revokes a role's last permission still reaches the
			// write as "this role, empty set" rather than being dropped from
			// the submission entirely.
			if _, seen := roleMap[column]; !seen {
				roleMap[column] = []string{}
			}
			if enabled {
				roleMap[column] = append(roleMap[column], permission)
			}
		}
	}
	for role, permissions := range roleMap {
		roleMap[role] = dedupeSorted(permissions)
	}
	return roleMap, dedupeSorted(submitted), nil
}

func dedupeSorted(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	unique := make([]string, 0, len(values))
	for _, value := range values {
		if value == "" {
			continue
		}
		if _, duplicate := seen[value]; duplicate {
			continue
		}
		seen[value] = struct{}{}
		unique = append(unique, value)
	}
	sort.Strings(unique)
	return unique
}
