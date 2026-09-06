package admin

// Role DEFINITIONS — create, rename and delete (gap G9).
//
// roles.go serves the permission MATRIX: which role holds which permission. It
// can only ever edit the cells of a table whose columns already exist. Nothing
// in this service could add a column, rename one, or take one away.
//
// pylon can. `legacy/plugins/admin/api/v2/roles.py` is full CRUD over
// `auth_core__role` for any mode, and over `auth_core__project_role` for one
// project. Go had the two GET routes only
// (`GET /admin/roles/{mode}/{projectID}`, eliteacore.Roles), so a deployment
// that wanted a `security_reviewer` role had to INSERT it by hand — and because
// `projectprovisioning.createProjectPermissions` copies the central default-mode
// role NAMES at project creation, a hand-inserted role reached projects created
// AFTERWARDS and no existing one.
//
// # The URL shape
//
// The three writes are `POST | PUT | DELETE /admin/roles/{scope}/{mode}`, the
// same two-segment shape `/admin/permissions/{scope}/{mode}` already uses, and
// for the same reason: the admin Roles page has four tabs, each of which is one
// (scope, target mode) pair, and the role columns it renders come from exactly
// that pair. A write addressed any other way could not name the tab it belongs
// to.
//
//	administration + {mode}   central auth_core__role rows for that mode
//	public                    the public project's auth_core__project_role rows
//	support                   the support project's rows
//
// `public` and `support` ignore the second segment, exactly as `readMatrix`
// does, because a project role has no mode.
//
// chi resolves this next to the existing `GET /admin/roles/{mode}/{projectID}`
// without ambiguity: the two patterns are different param nodes, and the static
// `administration` node registered for the GET carries no POST/PUT/DELETE, so
// those three fall through to `{scope}`. `TestAdminRoleWriteRoutesResolve`
// pins that, because it is a property of chi's trie and not of this file.
//
// # What the server refuses, and why the UI is not the place for it
//
//   - A BUILT-IN role name is never a rename or delete target. `super_admin`,
//     `admin`, `editor`, `viewer` and `system` are not conventions: they are
//     string literals in this service's own SQL and Go. `users.go`'s
//     `adminRolePriority`, `eliteacore`'s `usersPageQuery` (`r.name =
//     'super_admin' AND r.mode = 'administration'`),
//     `middleware/project_authorization.go`, `sqlcgen/auth_provisioning.sql.go`
//     and `projectprovisioning`'s `systemProjectRole` all name them. Renaming
//     one does not rename those, so the platform would keep looking for a role
//     that no longer exists — the failure is silent and looks like a
//     permission bug.
//   - A role may not be called `name`. The matrix response puts the permission
//     under the key `name` and every role under its own key
//     (`permissionMatrix.response`), so a role with that name would overwrite
//     the permission name in every row of the matrix.
//   - A role that still holds members is never deleted. The count comes back in
//     the 409 body, so the operator is told how many assignments to clear
//     rather than being told "no".
//
// # Propagation to existing projects
//
// Creating a central `default`-mode role does NOT reach existing projects on
// its own, and this file deliberately does not invent a second mechanism for
// that. The existing "Apply to Projects" control
// (`POST /admin/permissions/administration/default`, AdminPermissionsSync)
// already CROSS JOINs every central default-mode role into every in-scope
// project as step 1 of its transaction, so it picks a new role up unchanged.
// `projectprovisioning` picks it up for new projects for the same reason.
// The integration test asserts both, because "the existing control happens to
// cover this" is a claim, not a fact.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strconv"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/pkg/apierr"
	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

/* ── permission names ──────────────────────────────────────────────────── */

// The three permissions pylon's roles.py declares, transcribed verbatim. They
// are separate from `configuration.roles.permissions.*`, which governs the
// MATRIX: an operator may be trusted to move a checkbox without being trusted
// to delete the role the column belongs to.
//
// migrations/shared/0113_role_definition_permissions.sql grants them. They were
// in no migration before it, which router_permission_grant_gate_test.go would
// have reported as three routes that answer 403 to every caller on a clean
// database.
const (
	RolesCreatePermission = "configuration.roles.roles.create"
	RolesEditPermission   = "configuration.roles.roles.edit"
	RolesDeletePermission = "configuration.roles.roles.delete"
)

/* ── the names this surface will not touch ─────────────────────────────── */

// builtInRoles are the role names this service names in its own code. See the
// file header for the call sites; the list is not a style rule.
//
// `system` is already `roleSystem` in roles.go, where it is the column the
// matrix refuses to write. The same role is refused here for a wider reason:
// `projectprovisioning` assigns it to the per-project machine identity, so
// deleting it takes that identity's only role with it.
var builtInRoles = map[string]struct{}{
	"super_admin": {},
	"admin":       {},
	"editor":      {},
	"viewer":      {},
	roleSystem:    {},
}

// matrixRowNameKey is the key `permissionMatrix.response` puts the PERMISSION
// under. A role of the same name would occupy the same key.
const matrixRowNameKey = "name"

// roleNamePattern is deliberately narrow. A role name is a JSON object key in
// the matrix response, a path-free identifier in every client, and a value
// joined on by name across `auth_core__role` and `auth_core__project_role`.
// pylon validates nothing at all, which is how a role called `admin ` (with a
// trailing space) becomes a role that looks like `admin` and grants nothing.
var roleNamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,63}$`)

// centralRoleModes are the modes a central role can be created in.
//
// pylon accepts any string. A role in a mode no resolver reads is invisible
// work: `legacyrbac.PostgresResolver` resolves `default`, `administration` and
// `developer` and nothing else, so a role in a fourth mode can never grant
// anything to anybody. It is refused rather than stored.
var centralRoleModes = map[string]struct{}{
	"default":        {},
	"administration": {},
	"developer":      {},
}

/* ── request bodies ────────────────────────────────────────────────────── */

// roleWriteRequest is the body all three writes share. pylon reads
// `request.json["name"]` in all of them and `["new_name"]` in the PUT, so the
// field names are its own.
type roleWriteRequest struct {
	Name    string `json:"name"`
	NewName string `json:"new_name"`
}

// decodeRoleRequest reads the body, falling back to the `name` query parameter.
//
// The fallback is for DELETE alone in practice: a request body on DELETE is
// legal and is what pylon's client sends, but intermediaries drop it often
// enough that a delete which silently becomes "no name given" is a real
// failure mode. An explicit body always wins.
func decodeRoleRequest(r *http.Request) (roleWriteRequest, error) {
	var body roleWriteRequest
	err := json.NewDecoder(r.Body).Decode(&body)
	switch {
	case err == nil:
	case errors.Is(err, io.EOF):
		// An empty body is not a malformed one. Fall through to the query.
	default:
		return roleWriteRequest{}, matrixError{
			status:  http.StatusBadRequest,
			message: "invalid request body: expected an object with a role name",
		}
	}
	if body.Name == "" {
		body.Name = r.URL.Query().Get("name")
	}
	if body.NewName == "" {
		body.NewName = r.URL.Query().Get("new_name")
	}
	return body, nil
}

/* ── the target a write addresses ──────────────────────────────────────── */

// roleTarget is the resolved (scope, mode) pair: either a central mode or one
// project id, never both.
type roleTarget struct {
	scope string
	// mode is the central role mode. Empty for a project scope.
	mode string
	// projectID is 0 for the central scope.
	projectID int
}

func (t roleTarget) central() bool { return t.scope == scopeAdministration }

// describes the target in an error message, so a 404 says which list was
// searched rather than only that something was missing.
func (t roleTarget) describe() string {
	if t.central() {
		return "the central " + t.mode + " roles"
	}
	return fmt.Sprintf("the %s project (id %d)", t.scope, t.projectID)
}

func (h *Handler) resolveRoleTarget(ctx context.Context, scope, mode string) (roleTarget, error) {
	if !isKnownScope(scope) {
		return roleTarget{}, matrixError{
			status:  http.StatusNotFound,
			message: "unknown permission scope " + strconv.Quote(scope),
		}
	}
	if scope == scopeAdministration {
		if _, ok := centralRoleModes[mode]; !ok {
			return roleTarget{}, matrixError{
				status: http.StatusBadRequest,
				message: "unknown role mode " + strconv.Quote(mode) +
					": expected default, administration or developer",
			}
		}
		return roleTarget{scope: scope, mode: mode}, nil
	}
	projectID, err := h.scopeProjectID(ctx, scope)
	if err != nil {
		return roleTarget{}, err
	}
	if err := h.requireProject(ctx, projectID, scope); err != nil {
		return roleTarget{}, err
	}
	return roleTarget{scope: scope, projectID: projectID}, nil
}

// validateRoleName is the one place a name is checked, so create and rename
// cannot disagree about what a role may be called.
func validateRoleName(name string) error {
	if name == "" {
		return matrixError{status: http.StatusBadRequest, message: "the role name is required"}
	}
	if name == matrixRowNameKey {
		return matrixError{
			status: http.StatusBadRequest,
			message: `"name" is reserved: the permission matrix uses it for the permission ` +
				"of each row, so a role by that name would hide every permission name",
		}
	}
	if !roleNamePattern.MatchString(name) {
		return matrixError{
			status: http.StatusBadRequest,
			message: "invalid role name " + strconv.Quote(name) +
				": use 1 to 64 letters, digits, underscores or hyphens, starting with a letter or digit",
		}
	}
	return nil
}

// refuseBuiltIn guards the two destructive writes. Creation is NOT guarded by
// it: a deployment whose central role list is missing `editor` must be able to
// put it back, and the unique constraint already refuses a duplicate.
func refuseBuiltIn(name, what string) error {
	if _, built := builtInRoles[name]; !built {
		return nil
	}
	return matrixError{
		status: http.StatusConflict,
		message: strconv.Quote(name) + " is a built-in role and cannot be " + what +
			": this service names it in its own SQL and Go, which a rename would not follow",
	}
}

/* ── create ────────────────────────────────────────────────────────────── */

// AdminRoleCreate serves `POST /admin/roles/{scope}/{mode}`.
//
// Body: `{"name": "security_reviewer"}`. Answers 201 `{"ok": true, "name": …}`.
// pylon's project handler answers 201 and its central handler 200 for the same
// act; one status is used for both here, and `ok` is kept so the legacy admin
// client reads the result it expects.
//
// A new role starts with NO permissions. That is deliberate and is pylon's
// behaviour too: the operator grants them on the matrix this page already
// shows, where every grant is one auditable change.
func (h *Handler) AdminRoleCreate(w http.ResponseWriter, r *http.Request) {
	target, body, ok := h.beginRoleWrite(w, r)
	if !ok {
		return
	}
	if err := validateRoleName(body.Name); err != nil {
		writeMatrixError(w, err)
		return
	}

	if err := h.insertRole(r.Context(), target, body.Name); err != nil {
		writeMatrixError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"ok": true, "name": body.Name})
}

func (h *Handler) insertRole(ctx context.Context, target roleTarget, name string) error {
	var err error
	if target.central() {
		_, err = h.pool.Exec(ctx,
			`INSERT INTO public.auth_core__role (name, mode) VALUES ($1, $2)`, name, target.mode)
	} else {
		_, err = h.pool.Exec(ctx,
			`INSERT INTO public.auth_core__project_role (project_id, name) VALUES ($1, $2)`,
			target.projectID, name)
	}
	if isUniqueViolation(err) {
		return matrixError{
			status:  http.StatusConflict,
			message: strconv.Quote(name) + " already exists in " + target.describe(),
		}
	}
	if err != nil {
		return fmt.Errorf("create role: %w", err)
	}
	return nil
}

/* ── rename ────────────────────────────────────────────────────────────── */

// AdminRoleRename serves `PUT /admin/roles/{scope}/{mode}`.
//
// Body: `{"name": "old", "new_name": "new"}` — pylon's field names.
//
// # Why one UPDATE is enough, and what it is NOT enough for
//
// Nothing in the schema references a role by NAME.
// `auth_core__role_permission`, `auth_core__user_role`,
// `auth_core__project_role_permission` and `auth_core__project_user_role` all
// carry a `role_id` foreign key (internal/infra/db/migrations/001_initial.sql,
// internal/db/schema/auth_core_baseline.sql). So the grants and the member
// assignments follow the rename by staying exactly where they are, and a
// cascade over them would be a no-op looking for work.
//
// One place DOES reference by name, and it is not a foreign key: a central
// `default`-mode role is copied into every project as an
// `auth_core__project_role` row of the SAME NAME, by
// `projectprovisioning.createProjectPermissions` and by
// `syncDefaultPermissionsToProjects`. Renaming only the central row would leave
// every project holding the old name, and the next sync would ADD the new name
// beside it — one role split into two, with the members on the stale one. So
// the same transaction renames the project copies.
//
// A project that already has a role under the new name is left alone rather
// than merged: merging would move members between two roles that a deployment
// deliberately kept apart. The response reports how many projects were renamed,
// so a shortfall is visible instead of assumed.
func (h *Handler) AdminRoleRename(w http.ResponseWriter, r *http.Request) {
	target, body, ok := h.beginRoleWrite(w, r)
	if !ok {
		return
	}
	if body.Name == "" {
		writeMatrixError(w, matrixError{
			status: http.StatusBadRequest, message: "the role name is required",
		})
		return
	}
	if err := validateRoleName(body.NewName); err != nil {
		writeMatrixError(w, err)
		return
	}
	if err := refuseBuiltIn(body.Name, "renamed"); err != nil {
		writeMatrixError(w, err)
		return
	}
	// Renaming a role INTO a built-in name would produce a second role the
	// platform's own literals then match by accident.
	if err := refuseBuiltIn(body.NewName, "used as a new name"); err != nil {
		writeMatrixError(w, err)
		return
	}
	// A rename to the SAME name is deliberately NOT short-circuited here. The
	// UPDATE below matches one row and writes it unchanged, so an unknown role
	// still answers 404 rather than a 200 that claims a role was renamed. The
	// project cascade finds nothing, because every candidate row already holds
	// the new name and its own NOT EXISTS guard excludes it.

	renamed, err := h.renameRole(r.Context(), target, body.Name, body.NewName)
	if err != nil {
		writeMatrixError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "projects_renamed": renamed})
}

func (h *Handler) renameRole(
	ctx context.Context, target roleTarget, name, newName string,
) (projectsRenamed int64, err error) {
	transaction, err := h.pool.Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("begin role rename: %w", err)
	}
	defer func() { _ = transaction.Rollback(ctx) }()

	var tag pgconn.CommandTag
	if target.central() {
		tag, err = transaction.Exec(ctx,
			`UPDATE public.auth_core__role SET name = $3 WHERE mode = $1 AND name = $2`,
			target.mode, name, newName)
	} else {
		tag, err = transaction.Exec(ctx,
			`UPDATE public.auth_core__project_role SET name = $3 WHERE project_id = $1 AND name = $2`,
			target.projectID, name, newName)
	}
	if isUniqueViolation(err) {
		return 0, matrixError{
			status:  http.StatusConflict,
			message: strconv.Quote(newName) + " already exists in " + target.describe(),
		}
	}
	if err != nil {
		return 0, fmt.Errorf("rename role: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return 0, matrixError{
			status:  http.StatusNotFound,
			message: strconv.Quote(name) + " is not a role in " + target.describe(),
		}
	}

	if target.central() && target.mode == centralFallbackMode {
		// See this function's header: the per-project copies are joined by name.
		projectTag, projectErr := transaction.Exec(ctx, `
UPDATE public.auth_core__project_role stale
SET name = $2
WHERE stale.name = $1
  AND NOT EXISTS (
    SELECT 1 FROM public.auth_core__project_role existing
    WHERE existing.project_id = stale.project_id AND existing.name = $2
  )`, name, newName)
		if projectErr != nil {
			return 0, fmt.Errorf("rename project role copies: %w", projectErr)
		}
		projectsRenamed = projectTag.RowsAffected()
	}

	if err := transaction.Commit(ctx); err != nil {
		return 0, fmt.Errorf("commit role rename: %w", err)
	}
	return projectsRenamed, nil
}

/* ── delete ────────────────────────────────────────────────────────────── */

// AdminRoleDelete serves `DELETE /admin/roles/{scope}/{mode}`.
//
// Body: `{"name": "security_reviewer"}`, or `?name=` when an intermediary drops
// the body. Answers 200 `{"ok": true, …}`, not 204: pylon's project handler
// returns 204 WITH a body, which no client can read, and the counts below are
// the part an operator acts on.
//
// # The 409
//
// A role that is still assigned is refused, with the number of assignments. The
// alternative is a cascade: `auth_core__project_user_role.role_id` is
// `ON DELETE CASCADE`, so deleting the role would silently strip that role from
// every member who holds it. On a project role that is the whole membership of
// the project for those users. The count is the fact the operator needs, and it
// is measured INSIDE the deleting transaction so a membership added in between
// cannot slip past it.
//
// A central `default`-mode role counts its per-project copies' members too,
// for the reason the rename cascades: those copies exist only because the
// central role does.
func (h *Handler) AdminRoleDelete(w http.ResponseWriter, r *http.Request) {
	target, body, ok := h.beginRoleWrite(w, r)
	if !ok {
		return
	}
	if body.Name == "" {
		writeMatrixError(w, matrixError{
			status: http.StatusBadRequest, message: "the role name is required",
		})
		return
	}
	if err := refuseBuiltIn(body.Name, "deleted"); err != nil {
		writeMatrixError(w, err)
		return
	}

	removed, err := h.deleteRole(r.Context(), target, body.Name)
	if err != nil {
		var assigned roleInUseError
		if errors.As(err, &assigned) {
			writeJSON(w, http.StatusConflict, map[string]any{
				"error":   assigned.Error(),
				"members": assigned.members,
			})
			return
		}
		writeMatrixError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "project_roles_removed": removed})
}

// roleInUseError carries the assignment count into the 409 body. It is its own
// type rather than a matrixError so the count cannot be lost by the generic
// error writer.
type roleInUseError struct {
	name    string
	where   string
	members int64
}

func (e roleInUseError) Error() string {
	return fmt.Sprintf(
		"%s is still assigned to %d user(s) in %s: remove the assignments before deleting the role",
		strconv.Quote(e.name), e.members, e.where)
}

func (h *Handler) deleteRole(
	ctx context.Context, target roleTarget, name string,
) (projectRolesRemoved int64, err error) {
	transaction, err := h.pool.Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("begin role delete: %w", err)
	}
	defer func() { _ = transaction.Rollback(ctx) }()

	members, err := countRoleAssignments(ctx, transaction, target, name)
	if err != nil {
		return 0, err
	}
	if members > 0 {
		return 0, roleInUseError{name: name, where: target.describe(), members: members}
	}

	var tag pgconn.CommandTag
	if target.central() {
		tag, err = transaction.Exec(ctx,
			`DELETE FROM public.auth_core__role WHERE mode = $1 AND name = $2`, target.mode, name)
	} else {
		tag, err = transaction.Exec(ctx,
			`DELETE FROM public.auth_core__project_role WHERE project_id = $1 AND name = $2`,
			target.projectID, name)
	}
	if err != nil {
		return 0, fmt.Errorf("delete role: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return 0, matrixError{
			status:  http.StatusNotFound,
			message: strconv.Quote(name) + " is not a role in " + target.describe(),
		}
	}

	if target.central() && target.mode == centralFallbackMode {
		// The per-project copies exist because this role did. Leaving them
		// would leave every project offering a role the platform no longer
		// defines, which `GET /admin/roles/{mode}/{projectID}` would keep
		// listing as assignable. Their members were counted above, so none of
		// them has any.
		//
		// This is by NAME, and the schema records no provenance, so a project
		// role somebody created independently under the same name goes with
		// them. That row is already indistinguishable from a copy — the sync
		// would have adopted it on its next run — and the count above proves
		// nobody holds it. Deleting it is the lesser of the two wrong answers:
		// keeping it leaves a role the platform no longer defines, still
		// assignable, with no way to reach it from this page.
		copyTag, copyErr := transaction.Exec(ctx,
			`DELETE FROM public.auth_core__project_role WHERE name = $1`, name)
		if copyErr != nil {
			return 0, fmt.Errorf("delete project role copies: %w", copyErr)
		}
		projectRolesRemoved = copyTag.RowsAffected()
	}

	if err := transaction.Commit(ctx); err != nil {
		return 0, fmt.Errorf("commit role delete: %w", err)
	}
	return projectRolesRemoved, nil
}

// countRoleAssignments answers "how many users hold this role".
//
// The central and project halves read DIFFERENT tables, and both are needed for
// a central `default`-mode role: `auth_core__user_role` for the central
// assignment, and `auth_core__project_user_role` through the per-project copies
// this role is the template for. A central `administration` or `developer` role
// has no such copies — a project role of the same name there is an unrelated
// role — so the second half is asked only for the `default` mode.
func countRoleAssignments(
	ctx context.Context, transaction pgx.Tx, target roleTarget, name string,
) (int64, error) {
	if !target.central() {
		var members int64
		err := transaction.QueryRow(ctx, `
SELECT COUNT(*)
FROM public.auth_core__project_user_role assignment
JOIN public.auth_core__project_role role ON role.id = assignment.role_id
WHERE role.project_id = $1 AND role.name = $2`, target.projectID, name).Scan(&members)
		if err != nil {
			return 0, fmt.Errorf("count project role assignments: %w", err)
		}
		return members, nil
	}

	var central int64
	err := transaction.QueryRow(ctx, `
SELECT COUNT(*)
FROM public.auth_core__user_role assignment
JOIN public.auth_core__role role ON role.id = assignment.role_id
WHERE role.mode = $1 AND role.name = $2`, target.mode, name).Scan(&central)
	if err != nil {
		return 0, fmt.Errorf("count central role assignments: %w", err)
	}
	if target.mode != centralFallbackMode {
		return central, nil
	}

	var copies int64
	err = transaction.QueryRow(ctx, `
SELECT COUNT(*)
FROM public.auth_core__project_user_role assignment
JOIN public.auth_core__project_role role ON role.id = assignment.role_id
WHERE role.name = $1`, name).Scan(&copies)
	if err != nil {
		return 0, fmt.Errorf("count project role assignments: %w", err)
	}
	return central + copies, nil
}

/* ── shared entry ──────────────────────────────────────────────────────── */

// beginRoleWrite does what all three writes do first: check the pool, resolve
// the target and decode the body. It reports failure to the client itself and
// returns ok=false, so a handler cannot forget one of the three.
func (h *Handler) beginRoleWrite(
	w http.ResponseWriter, r *http.Request,
) (roleTarget, roleWriteRequest, bool) {
	if h.pool == nil {
		apierr.WriteStatus(w, http.StatusServiceUnavailable, "database unavailable")
		return roleTarget{}, roleWriteRequest{}, false
	}
	target, err := h.resolveRoleTarget(r.Context(), chi.URLParam(r, "scope"), chi.URLParam(r, "mode"))
	if err != nil {
		writeMatrixError(w, err)
		return roleTarget{}, roleWriteRequest{}, false
	}
	body, err := decodeRoleRequest(r)
	if err != nil {
		writeMatrixError(w, err)
		return roleTarget{}, roleWriteRequest{}, false
	}
	return target, body, true
}

func isUniqueViolation(err error) bool {
	var postgresError *pgconn.PgError
	return errors.As(err, &postgresError) && postgresError.Code == "23505"
}
