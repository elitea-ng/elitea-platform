package admin

// The admin Users surface: one read and two writes.
//
// Until unit A14 this package registered `GET /admin/auth_users/{mode}` and
// nothing else, so three of the four actions the admin Users page offers had no
// server at all. Rather than ship controls that no-op (the #130 / #180 defect —
// a handler that answers 200 and writes nothing is worse than a 404, because
// the UI reports success), the writes are implemented here for real and covered
// by tests that WRITE and then RE-READ.
//
// The contract is not invented: it mirrors the legacy handlers the existing
// admin_ui client already speaks to —
//   legacy/plugins/admin/api/v2/auth_users.py   (GET list, POST action)
//   legacy/plugins/admin/api/v2/user_suspend.py (PUT suspend)
//   legacy/plugins/auth_core/rpc/{users,roles}.py (the RPCs those call)
// — because guessing at a shape is how #137 broke elitea-sdk, admin_ui and the
// QA suite at once.
//
// One deliberate DIVERGENCE from the legacy original, called out so it is not
// mistaken for a porting slip: the legacy POST accepts any `action` string and
// falls through to `{"ok": true}` for the ones it does not implement (notably
// `toggle_admin`, which the client's `usersApi.js` declares). That is precisely
// the "green toast that lies" shape, so unknown actions here are rejected with
// 400 instead.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/adminaccess"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/adminaccess/rolegrants"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/audit"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/auth"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/nativeauth"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/pkg/apierr"
)

// adminRolePriority orders the administration-mode roles from most to least
// privileged. A user may hold several rows in auth_core__user_role; the listing
// reports the highest, matching `list_users_paginated`'s `role_priority`.
var adminRolePriority = []string{"super_admin", "admin", "editor", "viewer"}

// validAdminRoles is the set `set_admin_role` accepts, mirroring
// auth_users.py's `valid_roles`. `system` is deliberately NOT assignable.
var validAdminRoles = map[string]struct{}{
	"super_admin": {},
	"admin":       {},
	"editor":      {},
	"viewer":      {},
}

// systemUserPredicate classifies "system" users. Copied verbatim from
// `list_users_paginated`'s `is_system`, underscores included: SQLAlchemy's
// `.like()` treats `_` as a single-character wildcard just as SQL LIKE does, so
// this is the same predicate and not a stricter one.
const systemUserPredicate = `(u.email LIKE 'system_user_%@centry.user' OR u.email = 'system@centry.user')`

// sortableUserColumns mirrors `list_users_paginated`'s `allowed_sort`. The
// value is interpolated into SQL, so this allow-list is also what keeps the
// query injection-free — an unknown column falls back to `name`.
var sortableUserColumns = map[string]string{
	"name":       "u.name",
	"email":      "u.email",
	"last_login": "u.last_login",
	"id":         "u.id",
	"suspended":  "u.suspended",
}

/* ── read ──────────────────────────────────────────────────────────────── */

// AuthUsers serves `GET /admin/auth_users/{mode}`.
//
// Response body: {rows, total, counts:{platform,system}} — `counts` is computed
// over ALL users, unfiltered, because it labels the page's two tabs.
func (h *Handler) AuthUsers(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	limit, offset := paginationParams(r)
	query := r.URL.Query()

	listing, err := h.listUsers(ctx, userListingParams{
		limit:     limit,
		offset:    offset,
		search:    query.Get("search"),
		userType:  query.Get("user_type"),
		sortBy:    query.Get("sort_by"),
		sortOrder: query.Get("sort_order"),
	})
	if err != nil {
		// A read failure is reported as one. The previous implementation
		// swallowed every error into an empty page, which renders exactly like
		// "this deployment has no users".
		apierr.WriteStatus(w, http.StatusInternalServerError, "failed to list users")
		return
	}
	writeJSON(w, http.StatusOK, listing)
}

type userListingParams struct {
	limit     int
	offset    int
	search    string
	userType  string
	sortBy    string
	sortOrder string
}

func (h *Handler) listUsers(ctx context.Context, params userListingParams) (map[string]any, error) {
	if h.pool == nil {
		return map[string]any{
			"rows":   []map[string]any{},
			"total":  0,
			"counts": map[string]int{"platform": 0, "system": 0},
		}, nil
	}

	var totalUsers, systemUsers int
	err := h.pool.QueryRow(ctx, `
SELECT COUNT(*), COUNT(*) FILTER (WHERE `+systemUserPredicate+`)
FROM public.auth_core__user u`).Scan(&totalUsers, &systemUsers)
	if err != nil {
		return nil, fmt.Errorf("count users: %w", err)
	}

	conditions := make([]string, 0, 2)
	args := make([]any, 0, 3)
	switch params.userType {
	case "system":
		conditions = append(conditions, systemUserPredicate)
	case "platform":
		conditions = append(conditions, "NOT "+systemUserPredicate)
	}
	if params.search != "" {
		args = append(args, "%"+params.search+"%")
		conditions = append(conditions,
			fmt.Sprintf("(u.name ILIKE $%d OR u.email ILIKE $%d)", len(args), len(args)))
	}
	where := ""
	if len(conditions) > 0 {
		where = " WHERE " + strings.Join(conditions, " AND ")
	}

	var total int
	if err := h.pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM public.auth_core__user u`+where, args...,
	).Scan(&total); err != nil {
		return nil, fmt.Errorf("count filtered users: %w", err)
	}

	sortColumn, ok := sortableUserColumns[params.sortBy]
	if !ok {
		sortColumn = sortableUserColumns["name"]
	}
	direction := "ASC"
	if strings.EqualFold(params.sortOrder, "desc") {
		direction = "DESC"
	}

	// The filter placeholders occupy $1..$len(args); the three below follow.
	rolePlaceholder := "$" + strconv.Itoa(len(args)+1)
	limitPlaceholder := "$" + strconv.Itoa(len(args)+2)
	offsetPlaceholder := "$" + strconv.Itoa(len(args)+3)
	listArgs := append(append([]any{}, args...), adminRolePriority, params.limit, params.offset)

	// `admin_role` is resolved by a LATERAL sub-select rather than a LEFT JOIN.
	// A join multiplies the row out once per administration role the user holds
	// — which the pre-A14 listing did, so a user with two roles appeared twice
	// and `total` (a separate COUNT) disagreed with the page it labelled.
	rows, err := h.pool.Query(ctx, `
SELECT u.id,
       COALESCE(u.name, u.email, '') AS name,
       COALESCE(u.email, '')         AS email,
       to_char(u.last_login, 'YYYY-MM-DD"T"HH24:MI:SS') AS last_login,
       u.suspended,
       admin_role.name
FROM public.auth_core__user u
LEFT JOIN LATERAL (
    SELECT role.name
    FROM public.auth_core__user_role assignment
    JOIN public.auth_core__role role ON role.id = assignment.role_id
    WHERE assignment.user_id = u.id
      AND role.mode = 'administration'
      AND role.name = ANY(`+rolePlaceholder+`::text[])
    ORDER BY array_position(`+rolePlaceholder+`::text[], role.name)
    LIMIT 1
) AS admin_role ON TRUE`+where+`
ORDER BY `+sortColumn+` `+direction+` NULLS LAST, u.id `+direction+`
LIMIT `+limitPlaceholder+` OFFSET `+offsetPlaceholder, listArgs...)
	if err != nil {
		return nil, fmt.Errorf("list users: %w", err)
	}
	defer rows.Close()

	items := make([]map[string]any, 0, params.limit)
	for rows.Next() {
		var id int
		var name, email string
		var lastLogin, adminRole *string
		var suspended bool
		if err := rows.Scan(&id, &name, &email, &lastLogin, &suspended, &adminRole); err != nil {
			return nil, fmt.Errorf("scan user row: %w", err)
		}
		items = append(items, map[string]any{
			"id":         id,
			"name":       name,
			"email":      email,
			"last_login": lastLogin,
			"suspended":  suspended,
			// `is_admin` and `is_active` are the legacy-era fields; both are
			// kept so existing clients (admin_ui, the QA suite) keep working.
			"is_admin":   adminRole != nil,
			"is_active":  !suspended,
			"admin_role": adminRole,
		})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate user rows: %w", err)
	}

	return map[string]any{
		"rows":  items,
		"total": total,
		"counts": map[string]int{
			"platform": totalUsers - systemUsers,
			"system":   systemUsers,
		},
	}, nil
}

/* ── writes ────────────────────────────────────────────────────────────── */

type authUsersActionRequest struct {
	Action string `json:"action"`
	Users  []struct {
		ID int `json:"id"`
	} `json:"users"`
	UserID   *int    `json:"user_id"`
	RoleName *string `json:"role_name"`
}

// AuthUsersAction serves `POST /admin/auth_users/{mode}` — the single endpoint
// admin_ui's `usersApi.js` posts every user mutation to, discriminated by the
// `action` field of the body.
func (h *Handler) AuthUsersAction(w http.ResponseWriter, r *http.Request) {
	if !isAdministrationMode(r) {
		apierr.WriteStatus(w, http.StatusNotFound, "not found")
		return
	}
	if h.pool == nil {
		apierr.WriteStatus(w, http.StatusServiceUnavailable, "database unavailable")
		return
	}

	var body authUsersActionRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		apierr.WriteStatus(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if body.Action == "" {
		apierr.WriteStatus(w, http.StatusBadRequest, "action not set")
		return
	}

	switch body.Action {
	case "delete":
		h.deleteUsers(w, r, body)
	case "set_admin_role":
		h.setAdminRole(w, r, body)
	default:
		// Unlike the legacy runtime, an unimplemented action is NOT answered
		// with {"ok": true}. See this file's header.
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error": fmt.Sprintf("unsupported action %q", body.Action),
		})
	}
}

func (h *Handler) deleteUsers(w http.ResponseWriter, r *http.Request, body authUsersActionRequest) {
	if len(body.Users) == 0 {
		apierr.WriteStatus(w, http.StatusBadRequest, "users not set")
		return
	}
	ids := make([]int, 0, len(body.Users))
	for _, user := range body.Users {
		if user.ID <= 0 {
			apierr.WriteStatus(w, http.StatusBadRequest, "invalid user id")
			return
		}
		ids = append(ids, user.ID)
	}

	// One transaction: a partial delete would leave the operator unable to tell
	// which half of a bulk selection went through, and the journal row for the
	// users' personal memories (every project: no membership lookup, it would
	// be gone with the cascade) commits or rolls back with the delete itself.
	// auth_core__user's dependents all cascade (user_role, token, user_group,
	// project_user_role, …), so the row delete is sufficient.
	ctx := r.Context()
	tx, err := h.pool.Begin(ctx)
	if err != nil {
		apierr.WriteStatus(w, http.StatusInternalServerError, "failed to delete users")
		return
	}
	defer func() { _ = tx.Rollback(ctx) }()
	tag, err := tx.Exec(ctx,
		`DELETE FROM public.auth_core__user WHERE id = ANY($1::int[])`, ids)
	if err != nil {
		apierr.WriteStatus(w, http.StatusInternalServerError, "failed to delete users")
		return
	}
	if h.memoryUsers != nil {
		memoryIDs := make([]int64, len(ids))
		for i, id := range ids {
			memoryIDs[i] = int64(id)
		}
		if _, err := h.memoryUsers.EnqueueMemoryUsers(ctx, tx, 0, memoryIDs...); err != nil {
			slog.ErrorContext(ctx, "could not journal the deleted users' personal-memory cleanup", "err", err)
			apierr.WriteStatus(w, http.StatusInternalServerError, "failed to delete users")
			return
		}
	}
	if err := tx.Commit(ctx); err != nil {
		apierr.WriteStatus(w, http.StatusInternalServerError, "failed to delete users")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "deleted": tag.RowsAffected()})
}

func (h *Handler) setAdminRole(w http.ResponseWriter, r *http.Request, body authUsersActionRequest) {
	ctx := r.Context()
	if body.UserID == nil || *body.UserID <= 0 {
		apierr.WriteStatus(w, http.StatusBadRequest, "user_id not set")
		return
	}
	targetID := *body.UserID

	roleName := ""
	if body.RoleName != nil {
		roleName = *body.RoleName
	}
	if roleName != "" {
		if _, ok := validAdminRoles[roleName]; !ok {
			writeJSON(w, http.StatusBadRequest, map[string]any{
				"error": "Invalid role_name. Must be one of: [super_admin admin editor viewer] or null",
			})
			return
		}
	}

	currentRole, err := h.currentAdminRole(ctx, targetID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			apierr.WriteStatus(w, http.StatusNotFound, "user not found")
			return
		}
		apierr.WriteStatus(w, http.StatusInternalServerError, "failed to read current role")
		return
	}

	// Escalation guard, mirroring auth_users.py: granting super_admin, and
	// revoking it from someone who has it, both require the caller to hold
	// `admin.auth.users.super_admin` in administration mode. Enforced against
	// the DATABASE, never against anything the client sent — the admin UI's
	// `window.admin_ui_config.permissions` is presentation state only.
	caller, ok := h.resolveAssigningCaller(w, r, roleName == "super_admin" || currentRole == "super_admin")
	if !ok {
		return
	}

	transaction, err := h.pool.Begin(ctx)
	if err != nil {
		apierr.WriteStatus(w, http.StatusInternalServerError, "failed to update role")
		return
	}
	defer func() { _ = transaction.Rollback(ctx) }()

	// The subset rule (issue 2488): the caller holds every permission of the
	// role given AND of every administration role taken away. The write
	// below takes every other administration role of the target, custom ones
	// included, so all of them count. It is checked under the role-writes
	// lock, inside this transaction, so no concurrent write outdates it.
	if refusal, err := h.checkCentralAssignment(
		ctx, transaction, caller, auth.PermissionModeAdministration, roleName, targetID,
	); err != nil {
		apierr.WriteStatus(w, http.StatusInternalServerError, "failed to read current role")
		return
	} else if refusal != nil {
		adminaccess.Write(w, r, refusal)
		return
	}

	// Single-role-per-mode, exactly as `assign_user_to_role` enforces it, as a
	// difference (issue 2499): every other administration role is taken, the
	// new one (if any) is given, and each change writes one history row in
	// this transaction. A role the user already holds is left untouched.
	if _, err := rolegrants.Set(
		ctx, transaction, int64(targetID), auth.PermissionModeAdministration, roleName, grantOrigin(r, caller),
	); err != nil {
		// No matching administration role row means the deployment does not
		// define that role. Reporting success here would revoke the user's
		// role and grant nothing, the silent-data-loss shape this endpoint
		// exists to avoid, so the transaction is rolled back instead.
		if errors.Is(err, rolegrants.ErrRoleNotFound) {
			apierr.WriteStatus(w, http.StatusBadRequest, "administration role not found")
			return
		}
		apierr.WriteStatus(w, http.StatusInternalServerError, "failed to update role")
		return
	}

	if err := transaction.Commit(ctx); err != nil {
		apierr.WriteStatus(w, http.StatusInternalServerError, "failed to update role")
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "user_id": targetID, "role_name": body.RoleName})
}

// UserSuspend serves `PUT /admin/user_suspend/{mode}/{userID}`.
//
// This handler already existed in this package before unit A14 but was never
// mounted on any route — dead code with no caller, the pattern #126/#129/#134
// keep producing. A14 mounts it and hardens it: the id is validated, a missing
// `suspended` field is a 400 rather than a silent `false`, and an id that
// matches no user is a 404 rather than a 200 that changed nothing.
func (h *Handler) UserSuspend(w http.ResponseWriter, r *http.Request) {
	if !isAdministrationMode(r) {
		apierr.WriteStatus(w, http.StatusNotFound, "not found")
		return
	}
	if h.pool == nil {
		apierr.WriteStatus(w, http.StatusServiceUnavailable, "database unavailable")
		return
	}

	userID, err := strconv.Atoi(chi.URLParam(r, "userID"))
	if err != nil || userID <= 0 {
		apierr.WriteStatus(w, http.StatusBadRequest, "invalid user id")
		return
	}

	var body struct {
		Suspended *bool `json:"suspended"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		apierr.WriteStatus(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if body.Suspended == nil {
		apierr.WriteStatus(w, http.StatusBadRequest, "suspended field is required")
		return
	}

	// One transaction: a suspension and the revocation of the account's
	// native devices and browser sessions (ADR-0025 WP3) commit together or
	// not at all.
	tx, err := h.pool.Begin(r.Context())
	if err != nil {
		apierr.WriteStatus(w, http.StatusInternalServerError, "failed to update user")
		return
	}
	defer func() { _ = tx.Rollback(r.Context()) }()
	tag, err := tx.Exec(r.Context(),
		`UPDATE public.auth_core__user SET suspended = $1 WHERE id = $2`, *body.Suspended, userID)
	if err != nil {
		apierr.WriteStatus(w, http.StatusInternalServerError, "failed to update user")
		return
	}
	if tag.RowsAffected() == 0 {
		apierr.WriteStatus(w, http.StatusNotFound, "user not found")
		return
	}
	if *body.Suspended {
		var revokedBy *int64
		if caller, ok := auth.UserFromContext(r.Context()); ok {
			if id, resolved := caller.OwningUserID(); resolved {
				revokedBy = &id
			}
		}
		if _, err := nativeauth.RevokeUserSessions(r.Context(), tx, int64(userID),
			nativeauth.ReasonUserDeactivated, revokedBy); err != nil {
			apierr.WriteStatus(w, http.StatusInternalServerError, "failed to update user")
			return
		}
	}
	if err := tx.Commit(r.Context()); err != nil {
		apierr.WriteStatus(w, http.StatusInternalServerError, "failed to update user")
		return
	}

	// Domain meaning for the audit row the middleware is already going to write
	// for this request (internal/api/middleware/audit.go, DECISION 1). Without
	// it the trail records "PUT /api/v2/admin/user_suspend/{mode}/{userID}",
	// which says a suspension happened but not to whom — and "which account was
	// suspended, and when" is the question this row exists to answer.
	//
	// AFTER the update, not before: an annotation on a request that then failed
	// would describe a suspension that did not happen. The row's `is_error` and
	// `status_code` still record the failed attempt either way, at route
	// granularity.
	action := "user.unsuspend"
	if *body.Suspended {
		action = "user.suspend"
	}
	audit.Annotate(r.Context(), audit.Annotation{
		Action:     action,
		EntityType: "user",
		EntityID:   audit.ID(int64(userID)),
	})

	writeJSON(w, http.StatusOK, map[string]any{"id": userID, "suspended": *body.Suspended})
}

/* ── helpers ───────────────────────────────────────────────────────────── */

func isAdministrationMode(r *http.Request) bool {
	return chi.URLParam(r, "mode") == auth.PermissionModeAdministration
}

// currentAdminRole returns the target user's highest administration role, or ""
// when they hold none. `pgx.ErrNoRows` means the user does not exist.
func (h *Handler) currentAdminRole(ctx context.Context, userID int) (string, error) {
	var role *string
	err := h.pool.QueryRow(ctx, `
SELECT (
    SELECT role.name
    FROM public.auth_core__user_role assignment
    JOIN public.auth_core__role role ON role.id = assignment.role_id
    WHERE assignment.user_id = u.id
      AND role.mode = 'administration'
      AND role.name = ANY($2::text[])
    ORDER BY array_position($2::text[], role.name)
    LIMIT 1
)
FROM public.auth_core__user u
WHERE u.id = $1`, userID, adminRolePriority).Scan(&role)
	if err != nil {
		return "", err
	}
	if role == nil {
		return "", nil
	}
	return *role, nil
}

// checkCentralAssignment applies the subset rule of issue 2488 to an
// administration-plane assignment: the caller holds every permission of the
// role given (`assigned`, "" for none) and of every role taken away. A holder
// of admin.auth.users.super_admin is exempt. It fails closed.
func (h *Handler) checkCentralAssignment(
	ctx context.Context, q adminaccess.Querier, caller adminaccess.Caller, mode, assigned string, targetID int,
) (*adminaccess.Refusal, error) {
	if err := adminaccess.LockRoleWrites(ctx, q); err != nil {
		return nil, err
	}
	removed, err := adminaccess.UserCentralRoles(ctx, q, targetID, mode)
	if err != nil {
		return nil, err
	}
	// The write takes every role of the mode and gives `assigned`, so it
	// changes nothing when the target holds exactly `assigned` (or nothing,
	// and nothing is given).
	unchanged := (assigned == "" && len(removed) == 0) || (len(removed) == 1 && removed[0] == assigned)
	return h.checkCentralRoles(ctx, q, caller, centralChange{
		mode: mode, assigned: assigned, removed: removed, targetID: targetID, changes: !unchanged,
	})
}

// centralChange is one central assignment as checkCentralRoles sees it.
type centralChange struct {
	mode string
	// assigned is the role given, "" for none.
	assigned string
	// removed are the roles taken away.
	removed []string
	// targetID is the user whose roles change.
	targetID int
	// changes is false when the write would leave the target's roles as they
	// are; the not-yourself rule refuses only a change.
	changes bool
}

// checkCentralRoles is the rule for one central assignment. The caller must
// hold adminaccess.LockRoleWrites in the transaction `q` belongs to, and is
// read again here, so the check and the write see the same state. A failed
// read of the seed record is an error (500), not a refusal.
//
//   - A built-in role whose seed row is marked unassignable (the legacy
//     `system`) is given to nobody, by any caller (ADR-0059 R0.8a).
//   - super_admin, given or taken, needs admin.auth.users.super_admin, in
//     every mode.
//   - A caller who is not a standing global administrator changes no role of
//     its own, in either direction (CodeSelfAssignment). It is decided before
//     the subset rule, so the caller gets the more specific code. A write that
//     changes nothing is not refused by it.
//   - In the administration mode, the caller holds every permission of every
//     role given or taken. The retired developer mode grants nothing that a
//     route reads, so clearing a developer-mode row that the legacy runtime
//     left behind needs no subset.
func (h *Handler) checkCentralRoles(
	ctx context.Context, q adminaccess.Querier, caller adminaccess.Caller, change centralChange,
) (*adminaccess.Refusal, error) {
	caller, err := caller.Reload(ctx)
	if err != nil {
		return adminaccess.Unresolved("your own permissions could not be read, so the role change was refused"), nil
	}
	mode, assigned, removed := change.mode, change.assigned, change.removed
	if assigned != "" {
		unassignable, err := h.seededRolesMarked(ctx, q, mode, []string{assigned}, adminaccess.MarkerUnassignable)
		if err != nil {
			return nil, err
		}
		if len(unassignable) > 0 {
			return adminaccess.Unassignable(assigned), nil
		}
	}
	roles := append([]string{}, removed...)
	if assigned != "" {
		roles = append(roles, assigned)
	}
	if !caller.SuperAdmin && slices.Contains(roles, adminaccess.SuperAdminRole) {
		return adminaccess.SuperAdminRequired(), nil
	}
	role := assigned
	if role == "" && len(removed) > 0 {
		role = removed[0]
	}
	if change.changes {
		if refusal := adminaccess.CheckSelfAssignment(
			caller.UserID, caller.SuperAdmin, int64(change.targetID), role,
		); refusal != nil {
			return refusal, nil
		}
	}
	if caller.SuperAdmin || mode != adminaccess.ModeAdministration {
		return nil, nil
	}
	required, err := adminaccess.CentralRolePermissions(ctx, q, mode, roles)
	if err != nil {
		return adminaccess.Unresolved("the permissions of the role could not be read, so the role change was refused"), nil
	}
	return adminaccess.CheckAssignment(caller.Administration, false, role, required), nil
}

// resolveAssigningCaller reads the caller of an assignment once, before its
// transaction. `touchesSuperAdmin` is true when the write gives or takes the
// super_admin role as far as the request can tell before the lock; such a
// write is refused early, with the super_admin code, when the caller cannot be
// read or does not hold admin.auth.users.super_admin. checkCentralRoles checks
// it again under the lock.
func (h *Handler) resolveAssigningCaller(
	w http.ResponseWriter, r *http.Request, touchesSuperAdmin bool,
) (adminaccess.Caller, bool) {
	caller, refusal := adminaccess.ResolveCaller(r, h.resolver)
	if touchesSuperAdmin && (refusal != nil || !caller.SuperAdmin) {
		adminaccess.Write(w, r, adminaccess.SuperAdminRequired())
		return adminaccess.Caller{}, false
	}
	if refusal != nil {
		adminaccess.Write(w, r, refusal)
		return adminaccess.Caller{}, false
	}
	return caller, true
}
