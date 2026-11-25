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

// maxAssignedRoles bounds the `role_names` set of one request. The built-in
// catalogue has nine roles; a request naming more than this many is not a
// person's role set.
const maxAssignedRoles = 32

// systemUserPredicate classifies "system" users. Copied verbatim from
// `list_users_paginated`'s `is_system`, underscores included: SQLAlchemy's
// `.like()` treats `_` as a single-character wildcard just as SQL LIKE does, so
// this is the same predicate and not a stricter one.
const systemUserPredicate = `(u.email LIKE 'system_user_%@centry.user' OR u.email = 'system@centry.user')`

// listedUserPredicate is the accounts the Users listing returns at all: every
// account but the reserved break-glass account (ADR-0059 R2.6b). That account
// is not a system user either: it must never be shown with suspend, delete
// and role controls, on either tab, and the Access page (R2.17, R3.9) is the
// one place that shows it. It is matched without regard to case, as every
// reserved-account check is.
const listedUserPredicate = `lower(u.email) IS DISTINCT FROM '` + adminaccess.ReservedBreakGlassEmail + `'`

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
FROM public.auth_core__user u
WHERE `+listedUserPredicate).Scan(&totalUsers, &systemUsers)
	if err != nil {
		return nil, fmt.Errorf("count users: %w", err)
	}

	conditions := make([]string, 0, 3)
	conditions = append(conditions, listedUserPredicate)
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

	// The filter placeholders occupy $1..$len(args); the two below follow.
	limitPlaceholder := "$" + strconv.Itoa(len(args)+1)
	offsetPlaceholder := "$" + strconv.Itoa(len(args)+2)
	listArgs := append(append([]any{}, args...), params.limit, params.offset)

	// The roles are resolved by a LATERAL aggregate rather than a LEFT JOIN.
	// A join multiplies the row out once per administration role the user holds
	// — which the pre-A14 listing did, so a user with two roles appeared twice
	// and `total` (a separate COUNT) disagreed with the page it labelled. The
	// aggregate lists every role by name: no name ranks above another
	// (ADR-0059 R0.13), so a narrow or custom role is listed like any other.
	rows, err := h.pool.Query(ctx, `
SELECT u.id,
       COALESCE(u.name, u.email, '') AS name,
       COALESCE(u.email, '')         AS email,
       to_char(u.last_login, 'YYYY-MM-DD"T"HH24:MI:SS') AS last_login,
       u.suspended,
       COALESCE(admin_roles.names, '{}'::text[])
FROM public.auth_core__user u
LEFT JOIN LATERAL (
    SELECT array_agg(role.name ORDER BY role.name COLLATE "C") AS names
    FROM public.auth_core__user_role assignment
    JOIN public.auth_core__role role ON role.id = assignment.role_id
    WHERE assignment.user_id = u.id
      AND role.mode = 'administration'
) AS admin_roles ON TRUE`+where+`
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
		var lastLogin *string
		var suspended bool
		var adminRoles []string
		if err := rows.Scan(&id, &name, &email, &lastLogin, &suspended, &adminRoles); err != nil {
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
			"is_admin":  len(adminRoles) > 0, // any administration role
			"is_active": !suspended,
			// Every administration role held, sorted by name (ADR-0059
			// R0.8b). The single `admin_role` went with R0.8c (issue 2586).
			"admin_roles": adminRoles,
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
	UserID *int `json:"user_id"`
	// RoleName is the single form of set_admin_role: "replace every
	// administration role of the user with this one", null or "" for none.
	// Kept for existing automation (ADR-0059 R0.8b). Raw, so that a present
	// null can be told from an absent key.
	RoleName json.RawMessage `json:"role_name"`
	// RoleNames is the set form: the user's administration roles after the
	// request, [] for none. A request with both forms is a 400.
	RoleNames json.RawMessage `json:"role_names"`
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
	// project_user_role, …), so the row delete removes them, except that a
	// central role assignment is revoked first, through rolegrants (issue
	// 2922): the cascade would drop it with no history row, and the grant
	// history would show the role as still granted.
	ctx := r.Context()
	caller, ruled, ok := h.accountDeleter(r)
	if !ok {
		adminaccess.Write(w, r, adminaccess.Unresolved(
			"your own account could not be read, so the users were not deleted"))
		return
	}
	tx, err := h.pool.Begin(ctx)
	if err != nil {
		apierr.WriteStatus(w, http.StatusInternalServerError, "failed to delete users")
		return
	}
	defer func() { _ = tx.Rollback(ctx) }()
	// The role-writes lock is the transaction's first statement, as every
	// role write takes it (adminaccess.HoldRoleWrites says why).
	if err := adminaccess.LockRoleWrites(ctx, tx); err != nil {
		apierr.WriteStatus(w, http.StatusInternalServerError, "failed to delete users")
		return
	}
	// The last-administrator rule (issue 2500): the delete is measured after
	// it runs, in this transaction, and a delete that removed the last
	// administrator rolls back whole, the other accounts of a bulk selection
	// included.
	lastAdministrator, err := adminaccess.BeginLastAdministratorGuard(ctx, tx)
	if err != nil {
		apierr.WriteStatus(w, http.StatusInternalServerError, "failed to delete users")
		return
	}
	origin := grantOrigin(r, caller)
	for _, id := range ids {
		heldBefore, err := adminaccess.UserCentralRoles(ctx, tx, id, adminaccess.ModeAdministration)
		if err != nil {
			apierr.WriteStatus(w, http.StatusInternalServerError, "failed to delete users")
			return
		}
		// Deleting an account takes its administration roles, so the role
		// rules of a removal apply: deleting a super_admin needs super_admin,
		// the caller holds every permission of each role taken, and changes
		// no role of its own. One refused account refuses the whole delete.
		if ruled {
			refusal, err := h.checkAccountDeletion(ctx, tx, caller, id)
			if err != nil {
				apierr.WriteStatus(w, http.StatusInternalServerError, "failed to delete users")
				return
			}
			if refusal != nil {
				adminaccess.Write(w, r, refusal)
				return
			}
		}
		if _, err := rolegrants.RevokeAll(ctx, tx, int64(id), origin); err != nil {
			slog.ErrorContext(ctx, "could not revoke the roles of a deleted user", "user_id", id, "err", err)
			apierr.WriteStatus(w, http.StatusInternalServerError, "failed to delete users")
			return
		}
		// The administration roles the delete took are a role removal, and
		// a security audit event like any other (ADR-0059 R1.5).
		if err := h.appendRoleAssignment(ctx, tx, r, caller.UserID, id, adminaccess.ModeAdministration,
			heldBefore, origin.Source); err != nil {
			slog.ErrorContext(ctx, "could not record the roles of a deleted user", "user_id", id, "err", err)
			apierr.WriteStatus(w, http.StatusInternalServerError, "failed to delete users")
			return
		}
	}
	tag, err := tx.Exec(ctx,
		`DELETE FROM public.auth_core__user WHERE id = ANY($1::int[])`, ids)
	if err != nil {
		apierr.WriteStatus(w, http.StatusInternalServerError, "failed to delete users")
		return
	}
	if refusal, err := lastAdministrator.Check(ctx, tx); err != nil {
		apierr.WriteStatus(w, http.StatusInternalServerError, "failed to delete users")
		return
	} else if refusal != nil {
		adminaccess.Write(w, r, refusal)
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

// accountDeleter is the caller of an account delete, the actor of the
// revocations it writes. It is resolved as the role routes resolve it; then
// `ruled` is true and the role rules apply (checkAccountDeletion). A handler
// built without a permission resolver (unit fixtures only) reads the actor
// from the principal's own user id and applies no role rule. The history
// needs an actor for a console or api change, so a caller who resolves to no
// account deletes nothing (ok false).
func (h *Handler) accountDeleter(r *http.Request) (caller adminaccess.Caller, ruled, ok bool) {
	if h.resolver != nil {
		caller, refusal := adminaccess.ResolveCaller(r, h.resolver)
		return caller, true, refusal == nil
	}
	principal, found := auth.UserFromContext(r.Context())
	if !found {
		return adminaccess.Caller{}, false, false
	}
	id, found := principal.OwningUserID()
	if !found {
		return adminaccess.Caller{}, false, false
	}
	return adminaccess.Caller{UserID: id}, false, true
}

// checkAccountDeletion applies the rules of a role removal to the
// administration roles an account delete takes from `id`, under the
// role-writes lock the delete holds.
func (h *Handler) checkAccountDeletion(
	ctx context.Context, tx pgx.Tx, caller adminaccess.Caller, id int,
) (*adminaccess.Refusal, error) {
	held, err := adminaccess.UserCentralRoles(ctx, tx, id, adminaccess.ModeAdministration)
	if err != nil || len(held) == 0 {
		return nil, err
	}
	return h.checkCentralRoles(ctx, tx, caller, centralChange{
		mode: adminaccess.ModeAdministration, taken: held, targetID: id, changes: true,
	})
}

func (h *Handler) setAdminRole(w http.ResponseWriter, r *http.Request, body authUsersActionRequest) {
	ctx := r.Context()
	if body.UserID == nil || *body.UserID <= 0 {
		apierr.WriteStatus(w, http.StatusBadRequest, "user_id not set")
		return
	}
	targetID := *body.UserID
	mode := auth.PermissionModeAdministration

	requested, single, problem := requestedAdminRoles(body)
	if problem != "" {
		apierr.WriteStatus(w, http.StatusBadRequest, problem)
		return
	}

	held, err := h.userAdministrationRoles(ctx, targetID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			apierr.WriteStatus(w, http.StatusNotFound, "user not found")
			return
		}
		apierr.WriteStatus(w, http.StatusInternalServerError, "failed to read current role")
		return
	}
	// A role the deployment does not define in the mode is a 400 that names
	// it, before anything else. A role deleted between here and the write
	// makes rolegrants answer ErrRoleNotFound, which rolls the write back.
	// Reporting success would take the user's other roles and give nothing,
	// the silent-data-loss shape this endpoint exists to avoid.
	undefined, err := undefinedCentralRoles(ctx, h.pool, mode, requested)
	if err != nil {
		apierr.WriteStatus(w, http.StatusInternalServerError, "failed to read current role")
		return
	}
	if len(undefined) > 0 {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error": fmt.Sprintf("administration role %q not found", undefined[0]),
			"role":  undefined[0],
		})
		return
	}

	// Escalation guard, mirroring auth_users.py: granting super_admin, and
	// revoking it from someone who has it, both require the caller to hold
	// `admin.auth.users.super_admin` in administration mode. Enforced against
	// the DATABASE, never against anything the client sent — the admin UI's
	// `window.admin_ui_config.permissions` is presentation state only. This
	// is the early answer; checkCentralRoles decides again under the lock.
	given, taken := roleDifference(held, requested)
	touchesSuperAdmin := slices.Contains(given, adminaccess.SuperAdminRole) ||
		slices.Contains(taken, adminaccess.SuperAdminRole)
	caller, ok := h.resolveAssigningCaller(w, r, touchesSuperAdmin)
	if !ok {
		return
	}

	transaction, err := h.pool.Begin(ctx)
	if err != nil {
		apierr.WriteStatus(w, http.StatusInternalServerError, "failed to update role")
		return
	}
	defer func() { _ = transaction.Rollback(ctx) }()

	// The rules of issue 2488 and of R0.8a apply to EACH role of the
	// difference (ADR-0059 R0.8b): the caller holds every permission of every
	// role given and of every role taken, an unassignable role is given to
	// nobody, and a caller who is not a standing global administrator changes
	// none of its own roles. One role that fails refuses the whole request and
	// nothing is written. The difference is computed again under the
	// role-writes lock, inside this transaction, so no concurrent write
	// outdates it.
	if err := adminaccess.LockRoleWrites(ctx, transaction); err != nil {
		apierr.WriteStatus(w, http.StatusInternalServerError, "failed to read current role")
		return
	}
	lastAdministrator, err := adminaccess.BeginLastAdministratorGuard(ctx, transaction)
	if err != nil {
		apierr.WriteStatus(w, http.StatusInternalServerError, "failed to read current role")
		return
	}
	held, err = adminaccess.UserCentralRoles(ctx, transaction, targetID, mode)
	if err != nil {
		apierr.WriteStatus(w, http.StatusInternalServerError, "failed to read current role")
		return
	}
	given, taken = roleDifference(held, requested)
	refusal, err := h.checkCentralRoles(ctx, transaction, caller, centralChange{
		mode: mode, given: given, taken: taken, targetID: targetID,
		changes: len(given)+len(taken) > 0,
	})
	if err != nil {
		apierr.WriteStatus(w, http.StatusInternalServerError, "failed to read current role")
		return
	}
	if refusal != nil {
		adminaccess.Write(w, r, refusal)
		return
	}

	// The difference, as rolegrants applies it (issues 2499 and 2574): each
	// role taken and each role given writes one history row in this
	// transaction, and a role in both sets is not touched.
	origin := grantOrigin(r, caller)
	if _, err := rolegrants.Replace(
		ctx, transaction, int64(targetID), mode, requested, origin,
	); err != nil {
		if errors.Is(err, rolegrants.ErrRoleNotFound) {
			apierr.WriteStatus(w, http.StatusBadRequest, "administration role not found")
			return
		}
		if errors.Is(err, rolegrants.ErrReservedAccount) {
			adminaccess.Write(w, r, adminaccess.ReservedAccountRefusal())
			return
		}
		apierr.WriteStatus(w, http.StatusInternalServerError, "failed to update role")
		return
	}
	// The last-administrator rule (issue 2500), measured after the change in
	// this transaction: a set that takes the last holder's role is a 409.
	if refusal, err := lastAdministrator.Check(ctx, transaction); err != nil {
		apierr.WriteStatus(w, http.StatusInternalServerError, "failed to update role")
		return
	} else if refusal != nil {
		adminaccess.Write(w, r, refusal)
		return
	}
	// The security audit event of the change, in this transaction: a failed
	// append rolls the change back (ADR-0059 R1.5).
	if err := h.appendRoleAssignment(ctx, transaction, r, caller.UserID, targetID, mode, held, origin.Source); err != nil {
		slog.ErrorContext(ctx, "admin: the role change could not be recorded", "err", err)
		apierr.WriteStatus(w, http.StatusInternalServerError, "failed to update role")
		return
	}

	if err := transaction.Commit(ctx); err != nil {
		apierr.WriteStatus(w, http.StatusInternalServerError, "failed to update role")
		return
	}

	// `role_name` echoes the single form, as before, and is null in the set
	// form. `role_names` is the user's administration set after the request.
	var roleName any
	if single && len(requested) == 1 {
		roleName = requested[0]
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "user_id": targetID, "role_name": roleName, "role_names": requested,
	})
}

// requestedAdminRoles reads the role set of a set_admin_role request: the
// `role_names` set form, or the single `role_name` form ("this one role", null
// or "" for none). A request with neither is the single form with null, as it
// always was. It returns the set sorted and without duplicates, whether it
// came from the single form, and the text of a 400 when the request is
// malformed.
func requestedAdminRoles(body authUsersActionRequest) ([]string, bool, string) {
	hasSet := len(body.RoleNames) > 0 && string(body.RoleNames) != "null"
	if hasSet && len(body.RoleName) > 0 {
		return nil, false, "send role_name or role_names, not both"
	}
	if !hasSet {
		var name *string
		if len(body.RoleName) > 0 {
			if err := json.Unmarshal(body.RoleName, &name); err != nil {
				return nil, true, "role_name must be a role name or null"
			}
		}
		if name == nil || strings.TrimSpace(*name) == "" {
			return []string{}, true, ""
		}
		return []string{strings.TrimSpace(*name)}, true, ""
	}
	var names []string
	if err := json.Unmarshal(body.RoleNames, &names); err != nil {
		return nil, false, "role_names must be an array of role names"
	}
	if len(names) > maxAssignedRoles {
		return nil, false, fmt.Sprintf("role_names holds at most %d roles", maxAssignedRoles)
	}
	set := make([]string, 0, len(names))
	for _, name := range names {
		trimmed := strings.TrimSpace(name)
		if trimmed == "" {
			return nil, false, "role_names must not hold an empty name"
		}
		set = append(set, trimmed)
	}
	slices.Sort(set)
	return slices.Compact(set), false, ""
}

// roleDifference is what a request for `requested` changes on a user who
// holds `held`: the roles given and the roles taken, each sorted.
func roleDifference(held, requested []string) (given, taken []string) {
	for _, name := range requested {
		if !slices.Contains(held, name) {
			given = append(given, name)
		}
	}
	for _, name := range held {
		if !slices.Contains(requested, name) {
			taken = append(taken, name)
		}
	}
	slices.Sort(given)
	slices.Sort(taken)
	return given, taken
}

// undefinedCentralRoles returns the names of `roles` that the deployment does
// not define in `mode`, sorted.
func undefinedCentralRoles(ctx context.Context, q adminaccess.Querier, mode string, roles []string) ([]string, error) {
	if len(roles) == 0 {
		return nil, nil
	}
	rows, err := q.Query(ctx, `
SELECT requested.name
FROM unnest($2::text[]) AS requested(name)
WHERE NOT EXISTS (
    SELECT 1 FROM public.auth_core__role role WHERE role.mode = $1 AND role.name = requested.name
)
ORDER BY 1`, mode, roles)
	if err != nil {
		return nil, fmt.Errorf("read defined roles: %w", err)
	}
	return pgx.CollectRows(rows, pgx.RowTo[string])
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
	// The last-administrator rule (issue 2500) runs under the role-writes
	// lock, taken first, like every write that can remove an administrator.
	lastAdministrator, err := adminaccess.BeginLastAdministratorGuard(r.Context(), tx)
	if err != nil {
		apierr.WriteStatus(w, http.StatusInternalServerError, "failed to update user")
		return
	}
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
	if refusal, err := lastAdministrator.Check(r.Context(), tx); err != nil {
		apierr.WriteStatus(w, http.StatusInternalServerError, "failed to update user")
		return
	} else if refusal != nil {
		adminaccess.Write(w, r, refusal)
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

// userAdministrationRoles returns every administration role the user holds,
// sorted; `pgx.ErrNoRows` means the user does not exist. It is the read before
// the transaction; the write reads the set again under the role-writes lock.
func (h *Handler) userAdministrationRoles(ctx context.Context, userID int) ([]string, error) {
	var roles []string
	err := h.pool.QueryRow(ctx, `
SELECT COALESCE((
    SELECT array_agg(role.name ORDER BY role.name COLLATE "C")
    FROM public.auth_core__user_role assignment
    JOIN public.auth_core__role role ON role.id = assignment.role_id
    WHERE assignment.user_id = u.id AND role.mode = 'administration'
), '{}'::text[])
FROM public.auth_core__user u
WHERE u.id = $1`, userID).Scan(&roles)
	if err != nil {
		return nil, err
	}
	return roles, nil
}

// centralChange is one central assignment as checkCentralRoles sees it.
type centralChange struct {
	mode string
	// given are the roles the write gives, sorted.
	given []string
	// taken are the roles the write takes away, sorted.
	taken []string
	// targetID is the user whose roles change.
	targetID int
	// changes is false when the write would leave the target's roles as they
	// are; the not-yourself rule refuses only a change.
	changes bool
}

// checkCentralRoles is the rule for one central assignment, applied to EACH
// role it gives or takes (ADR-0059 R0.8b). The caller must hold
// adminaccess.LockRoleWrites in the transaction `q` belongs to, and is read
// again here, so the check and the write see the same state. A failed read of
// the seed record is an error (500), not a refusal. A refusal names the role
// that failed.
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
	// The reserved break-glass account holds no role (ADR-0059 R2.6b), for a
	// global administrator too; rolegrants.Grant refuses it again at the
	// write.
	if len(change.given) > 0 {
		reserved, err := adminaccess.IsReservedAccount(ctx, q, int64(change.targetID))
		if err != nil {
			return nil, err
		}
		if reserved {
			return adminaccess.ReservedAccountRefusal(), nil
		}
	}
	mode := change.mode
	unassignable, err := h.seededRolesMarked(ctx, q, mode, change.given, adminaccess.MarkerUnassignable)
	if err != nil {
		return nil, err
	}
	if len(unassignable) > 0 {
		return adminaccess.Unassignable(unassignable[0]), nil
	}
	// Without the seed record nothing is marked, but `system` is still the
	// service accounts' role: it stays unassignable by name there.
	if mode == adminaccess.ModeAdministration && slices.Contains(change.given, adminaccess.RoleSystem) {
		present, err := h.seedRecordPresent(ctx, q)
		if err != nil {
			return nil, err
		}
		if !present {
			return adminaccess.Unassignable(adminaccess.RoleSystem), nil
		}
	}
	roles := append(append([]string{}, change.given...), change.taken...)
	if !caller.SuperAdmin && slices.Contains(roles, adminaccess.SuperAdminRole) {
		return adminaccess.SuperAdminRequired(), nil
	}
	if change.changes && len(roles) > 0 {
		if refusal := adminaccess.CheckSelfAssignment(
			caller.UserID, caller.SuperAdmin, int64(change.targetID), roles[0],
		); refusal != nil {
			return refusal, nil
		}
	}
	if caller.SuperAdmin || mode != adminaccess.ModeAdministration {
		return nil, nil
	}
	// One role at a time, so that the refusal names the role that failed.
	for _, role := range roles {
		required, err := adminaccess.CentralRolePermissions(ctx, q, mode, []string{role})
		if err != nil {
			return adminaccess.Unresolved("the permissions of the role could not be read, so the role change was refused"), nil
		}
		if refusal := adminaccess.CheckAssignment(caller.Administration, false, role, required); refusal != nil {
			return refusal, nil
		}
	}
	return nil, nil
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
