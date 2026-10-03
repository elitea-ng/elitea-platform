package admin

// The admin Roles surface: the permission matrix behind `admin_ui`'s RolesPage.
//
// ## What was here before unit A14
//
// One route — `GET /admin/permissions/{scope}/{mode}` — and it had two defects
// that no status code would have revealed:
//
//  1. It IGNORED `{scope}` entirely. `/permissions/public/default` and
//     `/permissions/support/default` returned the CENTRAL administration matrix,
//     so two of the page's four tabs showed another scope's data while looking
//     perfectly healthy.
//  2. It listed only permissions that were ALREADY granted to some role
//     (`allPerms` was accumulated from `rp.permission`). A permission granted to
//     nobody never appeared as a row, so it could never be granted — the matrix
//     could lose a capability but never regain it.
//
// The PUT and the POST the page needs had no route at all.
//
// ## The contract is pylon's, not invented
//
// `legacy/plugins/admin/api/v2/permissions.py` — its `url_params` put the pylon
// MODE in the first path segment and the target mode in the second, which is why
// the segment this file calls `scope` selects a handler:
//
//	administration → AdminAPI          (central auth_core__role rows for a mode)
//	public         → PublicProjectAPI  (per-project overrides, public project)
//	support        → SupportProjectAPI (per-project overrides, support project)
//
// and the client that already speaks it is
// `frontends/admin_ui/frontend/src/api/usersApi.js`. Guessing at a shape here is
// what #137 cost.
//
// ## Deliberate divergences from the pylon original
//
//   - `local_permissions` is a pylon RUNTIME registry, accumulated as plugins
//     declare permissions. Go has no plugin runtime, so the catalogue is the
//     union of two sources: every permission any role — central or project —
//     holds, plus every permission this service declares in code. The second
//     source is what keeps a name grantable when no role holds it. On the
//     reference deployment the two implementations agree (340 names).
//   - Every write runs in ONE transaction. Pylon's PUT deletes every override
//     and then re-inserts, statement by statement; a failure between the two
//     leaves the project with no permissions at all.
//   - Unknown role names and unknown permission names are REJECTED with 400.
//     Pylon silently drops them, which reports success for a save that did not
//     save — the #130/#180 shape this unit exists to stop reproducing.
//   - `system` is not writable. Pylon lets a caller PUT it if they forge the
//     request; the admin UI has always rendered that column disabled, and the
//     server is the gate.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"os"
	"sort"
	"strconv"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/platformconfig"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/pkg/apierr"
	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

/* ── scopes ────────────────────────────────────────────────────────────── */

const (
	// scopeAdministration edits the CENTRAL matrix — auth_core__role and
	// auth_core__role_permission — for the target mode in the second segment.
	scopeAdministration = "administration"
	// scopePublic and scopeSupport edit ONE project's overrides in
	// auth_core__project_role_permission. Their target-mode segment is ignored,
	// exactly as pylon ignores it.
	scopePublic  = "public"
	scopeSupport = "support"
)

// roleSystem is never writable through this surface. It is the role the platform
// grants itself; a UI that renders its column disabled is a courtesy, and this
// constant is the enforcement.
const roleSystem = "system"

// centralFallbackMode is the mode a project with no overrides of its own shows,
// and the mode "Apply to Projects" pushes out. Pylon hardcodes 'default' in both
// places.
const centralFallbackMode = "default"

// projectUserNameLikePattern matches the personal projects a sync must skip.
//
// The backslashes are load-bearing and are NOT the same choice `users.go` makes
// for `systemUserPredicate`. That predicate reproduces SQLAlchemy `.like()`,
// where `_` really is a wildcard. This one reproduces Python
// `name.startswith('project_user_')`, which is literal — so the underscores are
// escaped and a project called `project-user-1` does not match.
const projectUserNameLikePattern = `project\_user\_%`

/* ── read ──────────────────────────────────────────────────────────────── */

// AdminPermissions serves `GET /admin/permissions/{scope}/{mode}`.
//
// Response body: {total, rows:[{name, <role>: bool, …}]} — one row per known
// permission, one boolean column per role in the selected scope.
func (h *Handler) AdminPermissions(w http.ResponseWriter, r *http.Request) {
	scope := chi.URLParam(r, "scope")
	if !isKnownScope(scope) {
		writeJSON(w, http.StatusNotFound,
			map[string]any{"error": "unknown permission scope " + strconv.Quote(scope)})
		return
	}
	if h.pool == nil {
		apierr.WriteStatus(w, http.StatusServiceUnavailable, "database unavailable")
		return
	}

	matrix, err := h.readMatrix(r.Context(), scope, chi.URLParam(r, "mode"))
	if err != nil {
		writeMatrixError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, matrix.response())
}

// permissionMatrix is the shape both the read and the writes work in: an
// ordered role list, an ordered permission catalogue, and the grants between.
type permissionMatrix struct {
	roles     []string
	catalogue []string
	granted   map[string]map[string]bool
	// inherited marks a PROJECT matrix that has no stored overrides at all and
	// is therefore displaying the central `default` matrix. Saving one has to
	// materialise the WHOLE submitted matrix, not a diff — see saveGrants.
	inherited bool
	// projectID is 0 for the central (administration) scope.
	projectID int
}

func (m permissionMatrix) response() map[string]any {
	rows := make([]map[string]any, 0, len(m.catalogue))
	for _, permission := range m.catalogue {
		row := map[string]any{"name": permission}
		for _, role := range m.roles {
			row[role] = m.granted[role][permission]
		}
		rows = append(rows, row)
	}
	// `total` counts the CATALOGUE, matching pylon's `len(all_permissions)`.
	// It is not a page count — this endpoint is never paginated.
	return map[string]any{"total": len(rows), "rows": rows}
}

func (h *Handler) readMatrix(ctx context.Context, scope, mode string) (permissionMatrix, error) {
	catalogue, err := h.permissionCatalogue(ctx)
	if err != nil {
		return permissionMatrix{}, err
	}

	if scope == scopeAdministration {
		roles, err := h.centralRoles(ctx, mode)
		if err != nil {
			return permissionMatrix{}, err
		}
		granted, err := h.centralGrants(ctx, mode)
		if err != nil {
			return permissionMatrix{}, err
		}
		return permissionMatrix{roles: roles, catalogue: catalogue, granted: granted}, nil
	}

	projectID, err := h.scopeProjectID(ctx, scope)
	if err != nil {
		return permissionMatrix{}, err
	}
	if err := h.requireProject(ctx, projectID, scope); err != nil {
		return permissionMatrix{}, err
	}
	roles, err := h.projectRoles(ctx, projectID)
	if err != nil {
		return permissionMatrix{}, err
	}
	granted, inherited, err := h.projectGrants(ctx, projectID, roles)
	if err != nil {
		return permissionMatrix{}, err
	}
	return permissionMatrix{
		roles: roles, catalogue: catalogue, granted: granted,
		inherited: inherited, projectID: projectID,
	}, nil
}

// permissionCatalogue is the Go stand-in for pylon's `auth.local_permissions`.
//
// It has two sources, and it needs both.
//
//  1. Every permission name the database has recorded a grant for, central or
//     per-project.
//  2. Every permission name this service DECLARES — see declaredPermissions.
//
// Source 1 alone repeats the defect the header of this file records. A name
// that no role holds is not a row. `parseMatrixBody` then rejects it as an
// unknown permission. The operator has no path to grant it.
//
// That happens in two ways. A permission that no migration seeds is
// ungrantable from the first boot. A permission that ONE migration seeds
// becomes ungrantable the moment an operator unchecks its last holder.
// `applyGrantChanges` deletes the row the catalogue was derived from.
//
// Pylon does not have this
// failure: `auth.local_permissions` is a declaration registry, not a set read
// back from the grants.
//
// The declared names are rows in the project matrices as well as the central
// one. A project role may hold a central configuration permission with no
// effect, which costs nothing; the alternative is a matrix whose rows change
// with the grants, which is the defect.
func (h *Handler) permissionCatalogue(ctx context.Context) ([]string, error) {
	rows, err := h.pool.Query(ctx, `
SELECT permission FROM public.auth_core__role_permission WHERE permission IS NOT NULL
UNION
SELECT permission FROM public.auth_core__project_role_permission
ORDER BY 1`)
	if err != nil {
		return nil, fmt.Errorf("read permission catalogue: %w", err)
	}
	defer rows.Close()
	granted, err := scanNames(rows, "permission")
	if err != nil {
		return nil, err
	}
	return catalogueFrom(granted), nil
}

// catalogueFrom composes the matrix catalogue from the names the database
// holds a grant for. It is the whole composition, so a test can build the
// catalogue the handler builds without a database.
//
// A retired name is left out even when a row still carries it. pylon re-seeds
// some of them on every start while it shares the database, so a row can come
// back after shared migration 0136 removed it. Leaving the name out of the
// catalogue keeps it off the Roles matrix, and parseMatrixBody then refuses a
// save that names it.
func catalogueFrom(granted []string) []string {
	live := make([]string, 0, len(granted))
	for _, name := range granted {
		if _, retired := retiredPermissions[name]; !retired {
			live = append(live, name)
		}
	}
	return mergePermissionCatalogue(live, declaredPermissions())
}

// retiredPermissions are permission strings that no code checks (#6874).
//
// The resolver matches exact strings and does no prefix matching, so each of
// these rows grants nothing. They are bare section nodes that pylon's admin
// module registered, prefix nodes the legacy Roles matrix saved when an
// operator clicked a group toggle, and strings a pylon migration seeded for
// features that never checked them.
//
// Shared migration 0136 deletes the rows. This list MUST equal the list in that
// file; internal/api/router_permission_retired_gate_test.go checks both that,
// and that no route gate checks any name here.
var retiredPermissions = map[string]struct{}{
	"projects":                              {},
	"configuration":                         {},
	"runtime":                               {},
	"modes":                                 {},
	"migration":                             {},
	"invites":                               {},
	"invites.platform":                      {},
	"admin":                                 {},
	"configurations":                        {},
	"models.chat.conversations.list_custom": {},
	"models.promptlib_shared.collection.details":        {},
	"models.promptlib_shared.collections.list":          {},
	"models.promptlib_shared.public_collection.details": {},
	"models.promptlib_shared.approve_collection.post":   {},
	"models.promptlib_shared.reject_collection.delete":  {},
}

// RetiredPermissions returns the retired names, sorted. It exists for the
// source gate in internal/api, which compares it with shared migration 0136.
func RetiredPermissions() []string {
	names := make([]string, 0, len(retiredPermissions))
	for name := range retiredPermissions {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// declaredPermissions lists the permission names this service enforces itself.
//
// Every admin Configuration section that carries `required_permission` is read
// from the section list. A new section cannot add a gate this catalogue does
// not know. `config_values.go` refuses the section to a caller who does not
// hold that name. The name must be grantable even when no role holds it
// yet: `configuration.advanced` and `configuration.service_descriptors` are
// declared here and seeded by no migration.
//
// Middleware gates are NOT here yet. Their names are constants that the gate
// call sites pass, so no runtime source lists them. The complete registry that
// both the gates and this catalogue read is the target shape; until it exists,
// a middleware-gated name stays grantable only while some role holds it.
func declaredPermissions() []string {
	declared := make([]string, 0, len(configSections()))
	for _, section := range configSections() {
		permission, ok := section["required_permission"].(string)
		if !ok || permission == "" {
			continue
		}
		declared = append(declared, permission)
	}
	return declared
}

// mergePermissionCatalogue returns the sorted union of the granted names and
// the declared names, with no duplicate.
func mergePermissionCatalogue(granted, declared []string) []string {
	merged := make([]string, 0, len(granted)+len(declared))
	seen := make(map[string]bool, len(granted)+len(declared))
	for _, name := range append(append([]string{}, granted...), declared...) {
		if name == "" || seen[name] {
			continue
		}
		seen[name] = true
		merged = append(merged, name)
	}
	sort.Strings(merged)
	return merged
}

func (h *Handler) centralRoles(ctx context.Context, mode string) ([]string, error) {
	rows, err := h.pool.Query(ctx,
		`SELECT name FROM public.auth_core__role WHERE mode = $1 ORDER BY id`, mode)
	if err != nil {
		return nil, fmt.Errorf("read roles: %w", err)
	}
	defer rows.Close()
	return scanNames(rows, "role")
}

func (h *Handler) projectRoles(ctx context.Context, projectID int) ([]string, error) {
	rows, err := h.pool.Query(ctx,
		`SELECT name FROM public.auth_core__project_role WHERE project_id = $1 ORDER BY id`, projectID)
	if err != nil {
		return nil, fmt.Errorf("read project roles: %w", err)
	}
	defer rows.Close()
	return scanNames(rows, "project role")
}

func scanNames(rows pgx.Rows, what string) ([]string, error) {
	names := []string{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, fmt.Errorf("scan %s: %w", what, err)
		}
		names = append(names, name)
	}
	return names, rows.Err()
}

// centralGrants reads role → permission set for one mode.
//
// It is a plain join over role_permission and cannot multiply rows out the way
// the pre-A14 user listing did, because the result is collected into a SET per
// role rather than into one output row per join row.
func (h *Handler) centralGrants(ctx context.Context, mode string) (map[string]map[string]bool, error) {
	rows, err := h.pool.Query(ctx, `
SELECT role.name, grant_row.permission
FROM public.auth_core__role role
JOIN public.auth_core__role_permission grant_row ON grant_row.role_id = role.id
WHERE role.mode = $1 AND grant_row.permission IS NOT NULL`, mode)
	if err != nil {
		return nil, fmt.Errorf("read central grants: %w", err)
	}
	defer rows.Close()
	return scanGrants(rows)
}

// projectGrants reads one project's overrides — and reproduces pylon's fallback
// exactly: a project with NO override rows shows the CENTRAL `default` matrix
// instead, because that is what its members effectively have.
//
// The second return value says which of the two happened. It matters: a save
// against an inherited matrix must write the whole thing (see saveGrants).
func (h *Handler) projectGrants(
	ctx context.Context, projectID int, roles []string,
) (map[string]map[string]bool, bool, error) {
	rows, err := h.pool.Query(ctx, `
SELECT role.name, override.permission
FROM public.auth_core__project_role role
JOIN public.auth_core__project_role_permission override ON override.role_id = role.id
WHERE role.project_id = $1`, projectID)
	if err != nil {
		return nil, false, fmt.Errorf("read project grants: %w", err)
	}
	defer rows.Close()

	granted, err := scanGrants(rows)
	if err != nil {
		return nil, false, err
	}
	if len(granted) > 0 {
		return granted, false, nil
	}

	central, err := h.centralGrants(ctx, centralFallbackMode)
	if err != nil {
		return nil, false, err
	}
	// Only the roles this project actually has get a column.
	inherited := make(map[string]map[string]bool, len(roles))
	for _, role := range roles {
		if permissions, ok := central[role]; ok {
			inherited[role] = permissions
		}
	}
	return inherited, true, nil
}

func scanGrants(rows pgx.Rows) (map[string]map[string]bool, error) {
	granted := map[string]map[string]bool{}
	for rows.Next() {
		var role, permission string
		if err := rows.Scan(&role, &permission); err != nil {
			return nil, fmt.Errorf("scan grant: %w", err)
		}
		if granted[role] == nil {
			granted[role] = map[string]bool{}
		}
		granted[role][permission] = true
	}
	return granted, rows.Err()
}

/* ── write: PUT ────────────────────────────────────────────────────────── */

// AdminPermissionsSave serves `PUT /admin/permissions/{scope}/{mode}`.
//
// The body is the matrix the page is showing: an array of
// `{"name": "<permission>", "<role>": true|false, …}` objects, exactly what the
// GET returned.
func (h *Handler) AdminPermissionsSave(w http.ResponseWriter, r *http.Request) {
	scope := chi.URLParam(r, "scope")
	if !isKnownScope(scope) {
		writeJSON(w, http.StatusNotFound,
			map[string]any{"error": "unknown permission scope " + strconv.Quote(scope)})
		return
	}
	if h.pool == nil {
		apierr.WriteStatus(w, http.StatusServiceUnavailable, "database unavailable")
		return
	}

	var body []map[string]json.RawMessage
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w,
			`{"error":"invalid request body: expected an array of permission rows"}`,
			http.StatusBadRequest)
		return
	}

	ctx := r.Context()
	mode := chi.URLParam(r, "mode")
	current, err := h.readMatrix(ctx, scope, mode)
	if err != nil {
		writeMatrixError(w, err)
		return
	}

	submission, err := parseMatrixBody(body, current)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}

	granted, revoked, err := h.saveGrants(ctx, scope, mode, current, submission)
	if err != nil {
		writeMatrixError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "granted": granted, "revoked": revoked,
	})
}

// grant is one (role, permission) cell of the matrix.
type grant struct {
	role       string
	permission string
}

// matrixSubmission is a parsed PUT body: the cells the caller wants TRUE, and
// which permission rows they sent at all.
type matrixSubmission struct {
	desired map[grant]bool
	// rows names every permission the body carried. Revocation is scoped to it,
	// so a client that submits a SUBSET of the matrix cannot silently revoke
	// everything it did not mention.
	rows map[string]bool
}

// parseMatrixBody turns the submitted rows into a matrixSubmission, rejecting
// anything the current matrix does not contain.
//
// Rejecting is the point. Pylon ignores an unknown role or permission and still
// answers `{"ok": true}`, so a client that misspells a role name is told its
// save succeeded. Here the save fails and says which name was wrong.
func parseMatrixBody(body []map[string]json.RawMessage, current permissionMatrix) (matrixSubmission, error) {
	knownRoles := make(map[string]bool, len(current.roles))
	for _, role := range current.roles {
		knownRoles[role] = true
	}
	knownPermissions := make(map[string]bool, len(current.catalogue))
	for _, permission := range current.catalogue {
		knownPermissions[permission] = true
	}

	submission := matrixSubmission{desired: map[grant]bool{}, rows: map[string]bool{}}
	for index, row := range body {
		name, err := rowPermissionName(row, index)
		if err != nil {
			return matrixSubmission{}, err
		}
		if !knownPermissions[name] {
			return matrixSubmission{}, fmt.Errorf("unknown permission %q", name)
		}
		submission.rows[name] = true
		if err := readRoleCells(row, index, name, knownRoles, submission.desired); err != nil {
			return matrixSubmission{}, err
		}
	}
	return submission, nil
}

func readRoleCells(
	row map[string]json.RawMessage,
	index int,
	permission string,
	knownRoles map[string]bool,
	desired map[grant]bool,
) error {
	for column, raw := range row {
		if column == "name" {
			continue
		}
		if !knownRoles[column] {
			return fmt.Errorf("unknown role %q", column)
		}
		var value bool
		if err := json.Unmarshal(raw, &value); err != nil {
			return fmt.Errorf("row %d: %q must be a boolean", index, column)
		}
		// `system` is present in every row the GET produced, so its presence is
		// not an error — but it is never applied. See roleSystem.
		if value && column != roleSystem {
			desired[grant{role: column, permission: permission}] = true
		}
	}
	return nil
}

func rowPermissionName(row map[string]json.RawMessage, index int) (string, error) {
	raw, ok := row["name"]
	if !ok {
		return "", fmt.Errorf("row %d has no \"name\"", index)
	}
	var name string
	if err := json.Unmarshal(raw, &name); err != nil || name == "" {
		return "", fmt.Errorf("row %d has an invalid \"name\"", index)
	}
	return name, nil
}

// saveGrants applies a submission and reports how many cells moved.
//
// ## The inherited-matrix trap
//
// A project with no overrides DISPLAYS the central matrix. Diffing a save
// against what is displayed would write only the cell the operator toggled — and
// because the project then HAS an override row, the next GET stops falling back
// and shows a matrix with exactly one permission in it. Every other permission
// the project appeared to have would be gone, without a single failed request.
//
// So an inherited matrix is MATERIALISED: the whole submitted set is written.
func (h *Handler) saveGrants(
	ctx context.Context, scope, mode string, current permissionMatrix, submission matrixSubmission,
) (granted, revoked int, err error) {
	baseline := current.granted
	if current.inherited {
		baseline = map[string]map[string]bool{}
	}

	toGrant, toRevoke := diffGrants(baseline, submission)
	if len(toGrant) == 0 && len(toRevoke) == 0 {
		return 0, 0, nil
	}
	if err := h.applyGrantChanges(ctx, scope, mode, toGrant, toRevoke); err != nil {
		return 0, 0, err
	}
	return len(toGrant), len(toRevoke), nil
}

// diffGrants returns the cells to add and the cells to remove.
func diffGrants(
	baseline map[string]map[string]bool, submission matrixSubmission,
) (granted, revoked []grant) {
	for cell := range submission.desired {
		if !baseline[cell.role][cell.permission] {
			granted = append(granted, cell)
		}
	}
	for role, permissions := range baseline {
		if role == roleSystem {
			continue
		}
		for permission := range permissions {
			// Only permissions the body carried are candidates for revocation.
			if !submission.rows[permission] {
				continue
			}
			if !submission.desired[grant{role: role, permission: permission}] {
				revoked = append(revoked, grant{role: role, permission: permission})
			}
		}
	}
	sortGrants(granted)
	sortGrants(revoked)
	return granted, revoked
}

func sortGrants(cells []grant) {
	sort.Slice(cells, func(a, b int) bool {
		if cells[a].role != cells[b].role {
			return cells[a].role < cells[b].role
		}
		return cells[a].permission < cells[b].permission
	})
}

// applyGrantChanges writes both halves of the diff inside ONE transaction.
//
// Pylon does not: its PUT issues an unbounded series of individual add/remove
// calls, and its public/support PUT deletes every override before inserting any.
// A failure part-way through that leaves a project with a matrix nobody asked
// for — including, in the delete-then-insert case, an empty one.
func (h *Handler) applyGrantChanges(
	ctx context.Context, scope, mode string, granted, revoked []grant,
) error {
	insertSQL, deleteSQL, scopeArg, err := h.grantStatements(ctx, scope, mode)
	if err != nil {
		return err
	}

	transaction, err := h.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin permission update: %w", err)
	}
	defer func() { _ = transaction.Rollback(ctx) }()

	for _, cell := range granted {
		if _, err := transaction.Exec(ctx, insertSQL, scopeArg, cell.role, cell.permission); err != nil {
			return fmt.Errorf("grant %s to %s: %w", cell.permission, cell.role, err)
		}
	}
	for _, cell := range revoked {
		if _, err := transaction.Exec(ctx, deleteSQL, scopeArg, cell.role, cell.permission); err != nil {
			return fmt.Errorf("revoke %s from %s: %w", cell.permission, cell.role, err)
		}
	}
	if err := transaction.Commit(ctx); err != nil {
		return fmt.Errorf("commit permission update: %w", err)
	}
	return nil
}

// grantStatements picks the pair of statements for the scope. Both are
// parameterised on ($1 scope key, $2 role name, $3 permission) so the caller
// does not branch per cell.
func (h *Handler) grantStatements(
	ctx context.Context, scope, mode string,
) (insertSQL, deleteSQL string, scopeArg any, err error) {
	if scope == scopeAdministration {
		return `
INSERT INTO public.auth_core__role_permission (role_id, permission)
SELECT role.id, $3 FROM public.auth_core__role role
WHERE role.mode = $1 AND role.name = $2
ON CONFLICT (role_id, permission) DO NOTHING`, `
DELETE FROM public.auth_core__role_permission grant_row
USING public.auth_core__role role
WHERE role.id = grant_row.role_id
  AND role.mode = $1 AND role.name = $2 AND grant_row.permission = $3`, mode, nil
	}

	projectID, err := h.scopeProjectID(ctx, scope)
	if err != nil {
		return "", "", nil, err
	}
	return `
INSERT INTO public.auth_core__project_role_permission (project_id, role_id, permission)
SELECT role.project_id, role.id, $3 FROM public.auth_core__project_role role
WHERE role.project_id = $1 AND role.name = $2
ON CONFLICT (project_id, role_id, permission) DO NOTHING`, `
DELETE FROM public.auth_core__project_role_permission override
USING public.auth_core__project_role role
WHERE role.id = override.role_id
  AND role.project_id = $1 AND role.name = $2 AND override.permission = $3`, projectID, nil
}

/* ── write: POST (sync to projects) ────────────────────────────────────── */

// AdminPermissionsSync serves `POST /admin/permissions/administration/default`
// — the page's "Apply to Projects" button.
//
// It pushes the central `default` matrix onto every SHARED project's per-project
// override tables, skipping personal projects and the public project, exactly as
// `permissions.py`'s `post` does.
//
// Where it differs is shape, not outcome: pylon walks every project over RPC and
// issues one call per (role, permission) it adds or removes — on the reference
// deployment that is 15k projects × ~1.7k cells. The three set-based statements
// below reach the same end state in one transaction, so an interrupted sync
// cannot leave half the estate on the old matrix.
func (h *Handler) AdminPermissionsSync(w http.ResponseWriter, r *http.Request) {
	if chi.URLParam(r, "scope") != scopeAdministration {
		writeJSON(w, http.StatusNotFound,
			map[string]any{"error": "sync is only defined for the administration scope"})
		return
	}
	if chi.URLParam(r, "mode") != centralFallbackMode {
		// Pylon's own guard, kept verbatim in meaning: the central matrix for
		// `administration`/`developer` has no per-project counterpart to sync to.
		writeJSON(w, http.StatusBadRequest,
			map[string]any{"error": "Sync is only supported for default mode"})
		return
	}
	if h.pool == nil {
		apierr.WriteStatus(w, http.StatusServiceUnavailable, "database unavailable")
		return
	}

	result, err := h.syncDefaultPermissionsToProjects(r.Context())
	if err != nil {
		writeMatrixError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":            true,
		"message":       "Successfully synced permissions to shared projects",
		"roles_created": result.rolesCreated,
		"granted":       result.granted,
		"revoked":       result.revoked,
	})
}

// syncScopePredicate selects the projects a sync touches. Kept in one constant
// so the three statements below cannot drift into disagreeing about which
// projects are in scope — which would show up as a project that gets roles but
// no permissions, or permissions that are never pruned.
const syncScopePredicate = `
    project.name NOT LIKE '` + projectUserNameLikePattern + `' ESCAPE '\'
    AND project.id <> $1`

type syncResult struct {
	rolesCreated int64
	granted      int64
	revoked      int64
}

func (h *Handler) syncDefaultPermissionsToProjects(ctx context.Context) (syncResult, error) {
	publicProjectID, err := h.scopeProjectID(ctx, scopePublic)
	if err != nil {
		return syncResult{}, err
	}

	transaction, err := h.pool.Begin(ctx)
	if err != nil {
		return syncResult{}, fmt.Errorf("begin sync: %w", err)
	}
	defer func() { _ = transaction.Rollback(ctx) }()

	// 1. Every in-scope project gets a project_role row for each central role.
	createdTag, err := transaction.Exec(ctx, `
INSERT INTO public.auth_core__project_role (project_id, name)
SELECT project.id, role.name
FROM centry.project project
CROSS JOIN (SELECT name FROM public.auth_core__role WHERE mode = 'default') role
WHERE `+syncScopePredicate+`
ON CONFLICT (project_id, name) DO NOTHING`, publicProjectID)
	if err != nil {
		return syncResult{}, fmt.Errorf("sync project roles: %w", err)
	}

	// 2. Grant everything the central matrix has.
	addedTag, err := transaction.Exec(ctx, `
INSERT INTO public.auth_core__project_role_permission (project_id, role_id, permission)
SELECT project_role.project_id, project_role.id, central_grant.permission
FROM public.auth_core__project_role project_role
JOIN centry.project project ON project.id = project_role.project_id
JOIN public.auth_core__role central_role
  ON central_role.name = project_role.name AND central_role.mode = 'default'
JOIN public.auth_core__role_permission central_grant
  ON central_grant.role_id = central_role.id AND central_grant.permission IS NOT NULL
WHERE `+syncScopePredicate+`
ON CONFLICT (project_id, role_id, permission) DO NOTHING`, publicProjectID)
	if err != nil {
		return syncResult{}, fmt.Errorf("sync project permissions: %w", err)
	}

	// 3. Revoke everything it does not. Without this the sync could only ever
	//    add, so a permission removed centrally would live on in every project —
	//    the failure mode that matters most here, because it is a privilege the
	//    operator believes they revoked.
	removedTag, err := transaction.Exec(ctx, `
DELETE FROM public.auth_core__project_role_permission override
USING public.auth_core__project_role project_role, centry.project project
WHERE project_role.id = override.role_id
  AND project.id = override.project_id
  AND `+syncScopePredicate+`
  AND NOT EXISTS (
    SELECT 1
    FROM public.auth_core__role central_role
    JOIN public.auth_core__role_permission central_grant ON central_grant.role_id = central_role.id
    WHERE central_role.mode = 'default'
      AND central_role.name = project_role.name
      AND central_grant.permission = override.permission
  )`, publicProjectID)
	if err != nil {
		return syncResult{}, fmt.Errorf("prune project permissions: %w", err)
	}

	if err := transaction.Commit(ctx); err != nil {
		return syncResult{}, fmt.Errorf("commit sync: %w", err)
	}
	return syncResult{
		rolesCreated: createdTag.RowsAffected(),
		granted:      addedTag.RowsAffected(),
		revoked:      removedTag.RowsAffected(),
	}, nil
}

/* ── scope resolution and errors ───────────────────────────────────────── */

func isKnownScope(scope string) bool {
	return scope == scopeAdministration || scope == scopePublic || scope == scopeSupport
}

// matrixError carries an HTTP status alongside the message, so the read and the
// two writes report "the support project is not configured" identically instead
// of each inventing a status.
type matrixError struct {
	status  int
	message string
}

func (e matrixError) Error() string { return e.message }

func writeMatrixError(w http.ResponseWriter, err error) {
	var typed matrixError
	if errors.As(err, &typed) {
		writeJSON(w, typed.status, map[string]any{"error": typed.message})
		return
	}
	apierr.WriteStatus(w, http.StatusInternalServerError, "the permission matrix could not be read or written")
}

// defaultPublicProjectID mirrors pylon's elitea_config "ai_project_id" default,
// and the identically-named constant in internal/api/middleware.
const defaultPublicProjectID = 1

// scopeProjectID resolves which project a project-scoped request edits.
//
// The PUBLIC project comes from the environment, in the same shape
// `internal/api/middleware/project.go` already uses.
//
// The SUPPORT project does NOT, and the difference is not stylistic. Pylon read
// it from the support_assistant module's `support_project_id`, and so does this
// platform now: the admin Features page writes that key into
// `centry.platform_config`, and `internal/api/v2/supportassistant` BOOTSTRAPS
// the hidden project lazily — on the first support request after an operator
// turns the assistant on — and writes the id it created back to the same key.
// Nobody types that id anywhere.
//
// An environment variable therefore cannot be the source of truth: it would be
// a second, hand-maintained copy of an id the service itself mints, and the
// moment they disagree this tab edits the permissions of a project the
// assistant is not using. It stays supported as a fallback for a deployment
// that pins the project by hand, but the stored value wins.
//
// The SUPPORT project has NO default. Pylon answers 404 "Support project not
// configured" when its id is unset, and so does this; inventing a default would
// point that tab at some unrelated project's permissions.
func (h *Handler) scopeProjectID(ctx context.Context, scope string) (int, error) {
	switch scope {
	case scopePublic:
		return envProjectID("AI_PROJECT_ID", defaultPublicProjectID)
	case scopeSupport:
		return h.supportProjectID(ctx)
	default:
		return 0, matrixError{status: http.StatusNotFound, message: "unknown permission scope"}
	}
}

// supportProjectID reads the bootstrapped hidden project, falling back to the
// environment.
//
// A store that cannot be read is NOT treated as "unset". Answering 404 there
// would tell the operator the assistant is not configured, when the truth is
// that this process could not find out — and the next thing they would do is
// set the environment variable this function is trying to make unnecessary.
func (h *Handler) supportProjectID(ctx context.Context) (int, error) {
	settings, err := platformconfig.LoadSupportAssistant(ctx, h.pool)
	switch {
	case err == nil:
		if settings.ProjectID > 0 {
			if settings.ProjectID > math.MaxInt32 {
				return 0, matrixError{
					status:  http.StatusInternalServerError,
					message: "the stored support project id is out of range",
				}
			}
			return int(settings.ProjectID), nil
		}
	case supportConfigTableMissing(err):
		// A MISSING TABLE is a schema gap, not an outage, and the two want
		// opposite answers. centry.platform_config is created by the bootstrap
		// schema (internal/infra/db/migrations/001_initial.sql) and by nothing
		// under migrations/shared/, so a deployment brought up by
		// elitea-migrate alone genuinely has no such relation. Reporting 503
		// there would replace the one message an operator can act on with an
		// outage they cannot, so this case falls through to the environment
		// exactly as an empty store does.
	default:
		return 0, matrixError{
			status:  http.StatusServiceUnavailable,
			message: "the support project could not be read",
		}
	}
	// Unset, not unreadable. The assistant has never been enabled, so its
	// project has never been created — the same answer pylon gives.
	return envProjectID("SUPPORT_PROJECT_ID", 0)
}

// supportConfigTableMissing reports the schema-gap failures, in the same shape
// and for the same reason as configurations.configurationSchemaMissing: a
// missing relation must not be read as a working store that holds nothing, nor
// as a database that has fallen over.
func supportConfigTableMissing(err error) bool {
	var postgresError *pgconn.PgError
	if !errors.As(err, &postgresError) {
		return false
	}
	// 3F000 invalid_schema_name, 42P01 undefined_table.
	return postgresError.Code == "3F000" || postgresError.Code == "42P01"
}

func envProjectID(variable string, fallback int) (int, error) {
	raw := os.Getenv(variable)
	if raw == "" {
		if fallback == 0 {
			return 0, matrixError{
				status:  http.StatusNotFound,
				message: "project not configured: set " + variable,
			}
		}
		return fallback, nil
	}
	value, err := strconv.Atoi(raw)
	if err != nil || value <= 0 {
		return 0, matrixError{
			status:  http.StatusInternalServerError,
			message: variable + " is not a valid project id",
		}
	}
	return value, nil
}

// requireProject refuses a project id that names no project. Without it a
// mis-set AI_PROJECT_ID would render an empty matrix that looks like "this
// project grants nothing" — and a PUT against it would silently write nothing.
func (h *Handler) requireProject(ctx context.Context, projectID int, scope string) error {
	var exists bool
	err := h.pool.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM centry.project WHERE id = $1)`, projectID).Scan(&exists)
	if err != nil {
		return fmt.Errorf("look up %s project: %w", scope, err)
	}
	if !exists {
		return matrixError{
			status:  http.StatusNotFound,
			message: fmt.Sprintf("the %s project (id %d) does not exist", scope, projectID),
		}
	}
	return nil
}
