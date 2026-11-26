package admin

// The admin MODES surface — `/admin/modes/administration`, the registry of
// which user holds which CENTRAL role in which mode.
//
// The contract is the legacy one (legacy/plugins/admin/api/v2/modes.py):
//
//	GET    → {total, rows:[{id, user_id, user_email, user_name, mode, role}]}
//	POST   {"user_id": 3 | "a@b.c", "mode": "administration", "role": "admin"}
//	DELETE ?id=<user_id>:<mode>:<role>
//
// The composite `id` is what makes the listing a table the client can act on:
// there is no single row id behind an assignment, so the legacy runtime
// synthesises one from the triple, and DELETE takes it back apart. That is
// preserved exactly.
//
// Two deliberate divergences from the legacy original:
//
//   - The legacy DELETE is a stub. It parses the composite id, computes the
//     triple, and returns `{"ok": True}` having deleted nothing — the body of
//     the handler is a `# TODO: RPC in auth_core` comment. Every "remove role"
//     in the admin console therefore reported success and left the assignment
//     in place. It deletes here, and answers 404 when the triple names no
//     assignment rather than reporting a removal that did not happen.
//   - The legacy runtime walks `tools.theme.modes` — a runtime registry
//     populated as plugins declare modes — and asks for every (user, mode) pair
//     over RPC. There is no such registry here, so the listing is one join over
//     the assignment table itself: the modes it reports are the modes that have
//     roles, which is exactly the set that can hold an assignment.
//
// `default` mode is READ but not WRITABLE through this endpoint, and that is
// the legacy behaviour rather than a restriction added here:
// `assign_user_to_role` routes a `default`-mode assignment to
// `admin_add_user_to_project`, which asserts on a project id this endpoint has
// no path segment for (legacy/plugins/auth_core/rpc/roles.py:135-142). Project
// membership is `/admin/users/{mode}/{projectID}`.

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

	"github.com/jackc/pgx/v5"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/adminaccess"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/adminaccess/rolegrants"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/auth"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/securityaudit"
)

// modeAssignment is one row of the listing.
type modeAssignment struct {
	ID        string `json:"id"`
	UserID    int    `json:"user_id"`
	UserEmail string `json:"user_email"`
	UserName  string `json:"user_name"`
	Mode      string `json:"mode"`
	Role      string `json:"role"`
}

/* ── read ──────────────────────────────────────────────────────────────── */

// Modes serves `GET /admin/modes/administration`.
func (h *Handler) Modes(w http.ResponseWriter, r *http.Request) {
	if h.pool == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "database unavailable"})
		return
	}

	// One join over the assignment table, rather than the legacy N+1 walk
	// (every user × every mode, one RPC per pair — 15k users × 4 modes on the
	// reference deployment). The result is the same set of rows.
	rows, err := h.pool.Query(r.Context(), `
SELECT assignment.user_id,
       COALESCE(account.email, ''),
       COALESCE(account.name, ''),
       role.mode,
       role.name
FROM public.auth_core__user_role assignment
JOIN public.auth_core__role role ON role.id = assignment.role_id
JOIN public.auth_core__user account ON account.id = assignment.user_id
ORDER BY assignment.user_id, role.mode, role.name`)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError,
			map[string]any{"error": "failed to list mode role assignments"})
		return
	}
	defer rows.Close()

	assignments := make([]modeAssignment, 0)
	for rows.Next() {
		var row modeAssignment
		if err := rows.Scan(&row.UserID, &row.UserEmail, &row.UserName, &row.Mode, &row.Role); err != nil {
			writeJSON(w, http.StatusInternalServerError,
				map[string]any{"error": "failed to list mode role assignments"})
			return
		}
		row.ID = assignmentID(row.UserID, row.Mode, row.Role)
		assignments = append(assignments, row)
	}
	if err := rows.Err(); err != nil {
		writeJSON(w, http.StatusInternalServerError,
			map[string]any{"error": "failed to list mode role assignments"})
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"total": len(assignments),
		"rows":  assignments,
	})
}

func assignmentID(userID int, mode, role string) string {
	return fmt.Sprintf("%d:%s:%s", userID, mode, role)
}

/* ── write: POST ───────────────────────────────────────────────────────── */

type modeAssignRequest struct {
	// The legacy runtime accepts either an integer id or an email address here,
	// in the same field, and resolves the second form through
	// `auth.get_user(email=…)`.
	UserID json.RawMessage `json:"user_id"`
	Mode   string          `json:"mode"`
	Role   string          `json:"role"`
}

// ModesAssign serves `POST /admin/modes/administration`.
//
// The assignment ADDS one role and keeps every other role the user holds
// (ADR-0059 R0.8b, issue 2574): a person holds several administration roles.
// Until then it was single-role-per-mode, as the legacy `assign_user_to_role`
// enforced it, so assigning a second role silently took the first. DELETE
// removes one role. Re-assigning a held role changes nothing.
func (h *Handler) ModesAssign(w http.ResponseWriter, r *http.Request) {
	if h.pool == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "database unavailable"})
		return
	}

	var body modeAssignRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid request body"})
		return
	}
	mode := strings.TrimSpace(body.Mode)
	role := strings.TrimSpace(body.Role)
	if mode == "" || role == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "mode and role are required"})
		return
	}
	if mode == auth.PermissionModeDefault {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error": "default-mode roles are project membership, not a central assignment; " +
				"use POST /api/v2/admin/users/{mode}/{projectID}",
		})
		return
	}
	// Any other mode except `administration` is refused before a role is
	// looked up (#6880). Before this, `developer` and any free-form string
	// reached the INSERT, and a legacy database answered with a developer-mode
	// assignment that no route reads. The removal below still accepts a
	// developer-mode id, so an operator can clear a row the legacy runtime left
	// behind.
	if _, known := centralRoleModes[mode]; !known {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error": "unknown role mode " + strconv.Quote(mode) + ": " + centralRoleModesHint,
		})
		return
	}

	ctx := r.Context()
	userID, err := h.resolveModeUser(ctx, body.UserID)
	if err != nil {
		writeModeUserError(w, err)
		return
	}

	// The super_admin escalation guard the admin Users surface applies is
	// applied here too: this endpoint reaches the same auth_core__user_role
	// table, so without it `modes.users` would be a second, unguarded way to
	// grant the platform's highest role.
	//
	// The guard is not scoped to a mode. Only `administration` reaches this
	// point today, but a `super_admin` role can exist in any mode
	// (auth_core__role is UNIQUE (name, mode)), and a body of
	// {"mode":"developer","role":"super_admin"} once reached the INSERT with no
	// escalation check at all. It stays unscoped so that a mode added to
	// centralRoleModes later cannot reopen that path.
	//
	// The write ADDS one role (ADR-0059 R0.8b): it takes nothing, so there is
	// no demotion arm.
	caller, ok := h.resolveAssigningCaller(w, r, role == adminaccess.SuperAdminRole)
	if !ok {
		return
	}

	transaction, err := h.pool.Begin(ctx)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "failed to assign role"})
		return
	}
	defer func() { _ = transaction.Rollback(ctx) }()

	// The rules of issue 2488 and R0.8a, the same ones the admin Users
	// surface applies to each role it gives: the caller holds every
	// permission of the role, an unassignable role is given to nobody, and a
	// caller changes none of its own roles. Before issue 2488, `modes.users`
	// assigned ANY role of the mode, a custom role holding everything
	// included. They run under the role-writes lock, in this transaction.
	if err := adminaccess.LockRoleWrites(ctx, transaction); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "failed to read current role"})
		return
	}
	held, err := adminaccess.UserCentralRoles(ctx, transaction, userID, mode)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "failed to read current role"})
		return
	}
	alreadyHeld := slices.Contains(held, role)
	change := centralChange{mode: mode, targetID: userID, changes: !alreadyHeld}
	if !alreadyHeld {
		change.given = []string{role}
	}
	if refusal, err := h.checkCentralRoles(ctx, transaction, caller, change); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "failed to read current role"})
		return
	} else if refusal != nil {
		adminaccess.Write(w, r, refusal)
		return
	}

	// One role is given if the user does not hold it yet, with one history
	// row in this transaction (issue 2499). Every other role of the user
	// stays: a person holds several administration roles (issue 2574).
	// Under an elevation the role is provisional (ADR-0059 R2.12a,
	// assignCentralRoles).
	origin := grantOrigin(r, caller)
	var given []string
	if !alreadyHeld {
		given = []string{role}
	}
	provisional, err := assignCentralRoles(ctx, transaction, caller, centralAssignment{
		userID: userID, mode: mode, given: given,
	}, origin)
	if err != nil {
		// No matching role means the deployment does not define it in that
		// mode.
		if errors.Is(err, rolegrants.ErrRoleNotFound) {
			writeJSON(w, http.StatusBadRequest, map[string]any{
				"error": fmt.Sprintf("role %q is not defined in mode %q", role, mode),
			})
			return
		}
		if errors.Is(err, rolegrants.ErrReservedAccount) {
			adminaccess.Write(w, r, adminaccess.ReservedAccountRefusal())
			return
		}
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "failed to assign role"})
		return
	}
	// The security audit event, in this transaction (ADR-0059 R1.5).
	if err := h.appendRoleAssignment(ctx, transaction, r, caller.UserID, userID, mode, held, origin.Source); err != nil {
		slog.ErrorContext(ctx, "admin: the role assignment could not be recorded", "err", err)
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "failed to assign role"})
		return
	}
	// A critical change under an elevation is announced in this
	// transaction (ADR-0059 R2.8b).
	if err := securityaudit.RunTransactionHooks(ctx, transaction); err != nil {
		slog.ErrorContext(ctx, "admin: the critical role assignment could not be announced", "err", err)
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "failed to assign role"})
		return
	}

	if err := transaction.Commit(ctx); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "failed to assign role"})
		return
	}
	// `provisional` says the role was given under the caller's elevation: in
	// effect while it is active, standing only once its review confirms it.
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":          true,
		"id":          assignmentID(userID, mode, role),
		"provisional": len(provisional) > 0,
	})
}

/* ── write: DELETE ─────────────────────────────────────────────────────── */

// ModesRemove serves `DELETE /admin/modes/administration?id=<user>:<mode>:<role>`.
func (h *Handler) ModesRemove(w http.ResponseWriter, r *http.Request) {
	if h.pool == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "database unavailable"})
		return
	}

	raw := r.URL.Query().Get("id")
	// SplitN with 3: a role name cannot contain ':' but this keeps the split
	// from mangling one that somehow does, instead of silently addressing a
	// different role.
	parts := strings.SplitN(raw, ":", 3)
	if len(parts) != 3 {
		writeJSON(w, http.StatusBadRequest,
			map[string]any{"error": `id must be "<user_id>:<mode>:<role>"`})
		return
	}
	userID, err := strconv.Atoi(parts[0])
	if err != nil || userID <= 0 {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid user id"})
		return
	}
	mode, role := parts[1], parts[2]
	if mode == "" || role == "" {
		writeJSON(w, http.StatusBadRequest,
			map[string]any{"error": `id must be "<user_id>:<mode>:<role>"`})
		return
	}
	// Symmetry with ModesAssign, which refuses `default` because those roles are
	// project membership. Without this the listing offers a removal that cannot
	// be undone through the same endpoint: a fresh database seeds user 1's
	// default-mode admin role (001_initial.sql), it appears here as
	// `1:default:admin`, and once removed only SQL can put it back.
	if mode == auth.PermissionModeDefault {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error": "default-mode roles are project membership, not a central assignment; " +
				"use DELETE /api/v2/admin/users/{mode}/{projectID}",
		})
		return
	}

	ctx := r.Context()
	// The guard covers every accepted mode. A legacy database carries a
	// `super_admin` role in the `developer` mode too, because auth_core__role
	// is UNIQUE (name, mode).
	caller, ok := h.resolveAssigningCaller(w, r, role == "super_admin")
	if !ok {
		return
	}

	transaction, err := h.pool.Begin(ctx)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "failed to remove role"})
		return
	}
	defer func() { _ = transaction.Rollback(ctx) }()
	// The subset rule (issue 2488): removing an administration role needs every
	// permission of that role too. A developer-mode row needs only the
	// super_admin guard; see checkCentralRoles.
	if err := adminaccess.LockRoleWrites(ctx, transaction); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "failed to remove role"})
		return
	}
	lastAdministrator, err := adminaccess.BeginLastAdministratorGuard(ctx, transaction)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "failed to remove role"})
		return
	}
	held, err := adminaccess.UserCentralRoles(ctx, transaction, userID, mode)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "failed to remove role"})
		return
	}
	refusal, err := h.checkCentralRoles(ctx, transaction, caller, centralChange{
		mode: mode, taken: []string{role}, targetID: userID, changes: slices.Contains(held, role),
	})
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "failed to remove role"})
		return
	}
	if refusal != nil {
		adminaccess.Write(w, r, refusal)
		return
	}

	origin := grantOrigin(r, caller)
	removed, err := rolegrants.Revoke(ctx, transaction, int64(userID), mode, role, origin)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "failed to remove role"})
		return
	}
	if !removed {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "assignment not found"})
		return
	}
	// The last-administrator rule (issue 2500), measured after the removal
	// in this transaction.
	if refusal, err := lastAdministrator.Check(ctx, transaction); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "failed to remove role"})
		return
	} else if refusal != nil {
		adminaccess.Write(w, r, refusal)
		return
	}
	// The security audit event, in this transaction (ADR-0059 R1.5).
	if err := h.appendRoleAssignment(ctx, transaction, r, caller.UserID, userID, mode, held, origin.Source); err != nil {
		slog.ErrorContext(ctx, "admin: the role removal could not be recorded", "err", err)
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "failed to remove role"})
		return
	}
	// A critical change under an elevation is announced in this
	// transaction (ADR-0059 R2.8b).
	if err := securityaudit.RunTransactionHooks(ctx, transaction); err != nil {
		slog.ErrorContext(ctx, "admin: the critical role removal could not be announced", "err", err)
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "failed to remove role"})
		return
	}
	if err := transaction.Commit(ctx); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "failed to remove role"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "removed": 1})
}

/* ── helpers ───────────────────────────────────────────────────────────── */

// modeUserError distinguishes "you sent nonsense" from "no such user".
type modeUserError struct {
	status  int
	message string
}

func (e modeUserError) Error() string { return e.message }

func writeModeUserError(w http.ResponseWriter, err error) {
	var typed modeUserError
	if errors.As(err, &typed) {
		writeJSON(w, typed.status, map[string]any{"error": typed.message})
		return
	}
	writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "failed to resolve user"})
}

// resolveModeUser accepts the two spellings the legacy runtime accepts in
// `user_id`: an integer id, or an email address to look one up by.
func (h *Handler) resolveModeUser(ctx context.Context, raw json.RawMessage) (int, error) {
	if len(raw) == 0 {
		return 0, modeUserError{status: http.StatusBadRequest, message: "user_id is required"}
	}

	var numeric int
	if err := json.Unmarshal(raw, &numeric); err == nil {
		if numeric <= 0 {
			return 0, modeUserError{status: http.StatusBadRequest, message: "invalid user id"}
		}
		var exists bool
		if err := h.pool.QueryRow(ctx,
			`SELECT EXISTS (SELECT 1 FROM public.auth_core__user WHERE id = $1)`,
			numeric).Scan(&exists); err != nil {
			return 0, err
		}
		if !exists {
			return 0, modeUserError{status: http.StatusNotFound, message: "user not found"}
		}
		return numeric, nil
	}

	var text string
	if err := json.Unmarshal(raw, &text); err != nil {
		return 0, modeUserError{
			status:  http.StatusBadRequest,
			message: "user_id must be a user id or an email address",
		}
	}
	// A numeric STRING is an id, not an address — the admin client sends form
	// values, and the legacy `int(data["user_id"])` accepts "3".
	if numeric, err := strconv.Atoi(strings.TrimSpace(text)); err == nil {
		return h.resolveModeUser(ctx, json.RawMessage(strconv.Itoa(numeric)))
	}

	email := strings.ToLower(strings.TrimSpace(text))
	if email == "" {
		return 0, modeUserError{status: http.StatusBadRequest, message: "user_id is required"}
	}
	var userID int
	err := h.pool.QueryRow(ctx,
		`SELECT id FROM public.auth_core__user WHERE lower(email) = $1`, email).Scan(&userID)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, modeUserError{status: http.StatusNotFound, message: "user not found"}
	}
	if err != nil {
		return 0, err
	}
	return userID, nil
}
