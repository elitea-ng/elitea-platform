package eliteacore

// Project-membership WRITES (#130).
//
// Until this file existed, router.go mounted GET/POST/PUT/DELETE of
// /api/v2/admin/users/{mode}/{projectID} all onto `Handler.Users`, which does
// no method branching whatsoever: every verb ran the listing SELECT and
// answered 200 with the member list. Invite, Edit role and Remove therefore
// all reported success in the UI and changed nothing — strictly worse than a
// 404, because the client's `onSuccess` path fired, a success toast rendered
// and the user only discovered the truth on the next reload.
//
// The wire contract implemented here is the pylon one the old SPA (and the new
// client, which was ported from it) actually speaks — legacy/plugins/admin/
// api/v2/users.py:
//
//	POST   {"emails": ["a@b.c"], "roles": ["editor"]}
//	       → per-email result objects, 400 when any of them failed
//	PUT    {"userId": "3", "roles": ["admin"]}   (batch: "3,4")
//	       → {"msg": "roles updated"}; the role set is REPLACED, not merged
//	DELETE ?id[]=3,4                             (one key, comma-joined)
//	       → {"msg": "users successfully removed"}
//
// The PUT key is `userId`/`ids`/`id` rather than the `{user_id, role_id}` ints
// that internal/api/oapiserver/admin.go decodes. That file is NOT on this
// route — the chi router never reaches it — and the original #130 diagnosis
// blaming its field binding was corrected in the issue's comments.
//
// `administration` mode used to answer 501 here, on the reasoning that it
// "addresses the GLOBAL (project-less) scope in pylon". That is true of the
// admin panel's OTHER routes and false of this one: `/admin/users/{mode}/{projectID}`
// carries the project id in its own path, and pylon's users.py declares BOTH
// url_params (`<int:project_id>` and `<string:mode>/<int:project_id>`) with
// `mode_handlers` mapping `default` and `administration` to the same body. So
// there is no global scope to confuse it with, and the 501 was simply a hole:
// the admin Projects page's member dialog is exactly this call, and it reached
// a Not Implemented (unit A14, #200).
//
// It is accepted here; what differs between the modes is the GATE, not the
// query. router.go registers the administration-mode verbs separately, on
// `configuration.users.users.*` resolved CENTRALLY — default mode resolves the
// same permission from the caller's membership of the target project, which a
// global administrator does not have. Both still require a concrete, positive
// project id below, so neither can write project 0's membership.

import (
	"encoding/json"
	"errors"
	"fmt"
	appmailer "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/mailer"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth"
	"log/slog"
	"net/http"
	"net/mail"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
)

const (
	userWriteModeDefault        = "default"
	userWriteModeAdministration = "administration"
)

// inviteResult mirrors one element of the array pylon's POST returns
// (add_user_to_project_or_create's return value, projects/rpc/poc.go:139).
// The client only branches on the HTTP status, but the per-email detail is
// what makes a partial failure ("two invited, one already a member")
// diagnosable at all.
type inviteResult struct {
	Msg    string `json:"msg"`
	Status string `json:"status"`
	Email  string `json:"email"`
	ID     string `json:"id,omitempty"`
	// InvitationDelivered says whether an invitation e-mail went out for this
	// address (ADR-0024 WP7); false when no mailer is configured, when the
	// relay refused, or when the row was an error.
	InvitationDelivered bool `json:"invitation_delivered"`
}

// userWriteContext performs the three checks every write verb shares and
// writes the failure response itself. The bool is false when the caller must
// return immediately.
func (h *Handler) userWriteContext(w http.ResponseWriter, r *http.Request) (int, bool) {
	switch chi.URLParam(r, "mode") {
	// Both modes address the SAME project membership — see this file's header.
	// The route-level gate is what distinguishes them, and it is applied before
	// this function runs.
	case userWriteModeDefault, userWriteModeAdministration:
	default:
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "unknown mode"})
		return 0, false
	}

	if h.pool == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "database unavailable"})
		return 0, false
	}

	projectID, err := strconv.Atoi(chi.URLParam(r, "projectID"))
	if err != nil || projectID <= 0 {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid project id"})
		return 0, false
	}
	return projectID, true
}

// resolveProjectRoleIDs maps the role NAMES the client sends onto
// auth_core__project_role ids for this project. Names the project does not
// define are returned separately so the caller can reject the whole request —
// pylon's validate_role_assignment does the same, and silently dropping them
// would produce the exact class of bug this file exists to remove (a 200 that
// did less than it said).
func (h *Handler) resolveProjectRoleIDs(
	r *http.Request, projectID int, names []string,
) (ids []int, unknown []string, err error) {
	rows, err := h.pool.Query(r.Context(),
		`SELECT id, name FROM auth_core__project_role WHERE project_id = $1`, projectID)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()

	byName := make(map[string]int)
	for rows.Next() {
		var id int
		var name string
		if err := rows.Scan(&id, &name); err != nil {
			return nil, nil, err
		}
		byName[name] = id
	}
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}

	seen := make(map[int]struct{}, len(names))
	for _, name := range names {
		trimmed := strings.TrimSpace(name)
		id, ok := byName[trimmed]
		if !ok {
			unknown = append(unknown, trimmed)
			continue
		}
		if _, dup := seen[id]; dup {
			continue
		}
		seen[id] = struct{}{}
		ids = append(ids, id)
	}
	return ids, unknown, nil
}

func normalizeEmail(raw string) string {
	return strings.ToLower(strings.TrimSpace(raw))
}

// validEmail is a backstop for InviteUserDialog's own validator, not a
// replacement for it: net/mail accepts display-name forms ("A <a@b.c>") that
// are not what this endpoint means by an address, so the parse result must
// round-trip to the input.
func validEmail(address string) bool {
	parsed, err := mail.ParseAddress(address)
	if err != nil || parsed.Address != address {
		return false
	}
	at := strings.LastIndex(address, "@")
	return at > 0 && strings.Contains(address[at+1:], ".")
}

// UsersCreate implements POST /api/v2/admin/users/{mode}/{projectID} —
// "invite users by email with these roles".
//
// Per pylon parity an address with no auth_core__user row is CREATED here
// rather than rejected: that is what makes an invite an invite. The row is
// created by email only — no auth_core__user_provider link — because the OIDC
// provisioning path already resolves an existing row by email and links the
// provider on first login (internal/db/queries/auth_provisioning.sql's
// GetAuthUserByEmailForProvisioning → LinkAuthProviderIfMissing). Writing a
// provider_ref here would guess at an identity the IdP has not asserted yet.
func (h *Handler) UsersCreate(w http.ResponseWriter, r *http.Request) {
	projectID, ok := h.userWriteContext(w, r)
	if !ok {
		return
	}

	var body struct {
		Emails []string `json:"emails"`
		Roles  []string `json:"roles"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid request body"})
		return
	}
	if len(body.Emails) == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "emails is required"})
		return
	}
	if len(body.Roles) == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "roles is required"})
		return
	}

	roleIDs, unknown, err := h.resolveProjectRoleIDs(r, projectID, body.Roles)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "failed to read project roles"})
		return
	}
	if len(unknown) > 0 {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error": fmt.Sprintf("unknown role(s) for this project: %s", strings.Join(unknown, ", ")),
		})
		return
	}

	results := make([]inviteResult, 0, len(body.Emails))
	hasErrors := false

	for _, raw := range body.Emails {
		email := normalizeEmail(raw)
		if !validEmail(email) {
			hasErrors = true
			results = append(results, inviteResult{
				Msg:    fmt.Sprintf("Invalid email: %s", strings.TrimSpace(raw)),
				Status: "error",
				Email:  strings.TrimSpace(raw),
			})
			continue
		}

		userID, alreadyMember, err := h.inviteOne(r, projectID, email, roleIDs)
		switch {
		case err != nil:
			hasErrors = true
			results = append(results, inviteResult{
				Msg:    fmt.Sprintf("failed to add %s to project %d", email, projectID),
				Status: "error",
				Email:  email,
			})
		case alreadyMember:
			hasErrors = true
			results = append(results, inviteResult{
				Msg:    fmt.Sprintf("user %s already exists in project %d", email, projectID),
				Status: "error",
				Email:  email,
			})
		default:
			delivered := h.deliverProjectInvitation(r, projectID, email)
			results = append(results, inviteResult{
				Msg:                 fmt.Sprintf("user %s added to project %d", email, projectID),
				Status:              "ok",
				Email:               email,
				ID:                  strconv.Itoa(userID),
				InvitationDelivered: delivered,
			})
		}
	}

	status := http.StatusOK
	if hasErrors {
		status = http.StatusBadRequest
	}
	writeJSON(w, status, results)
}

// deliverProjectInvitation mails the invitation when a mailer is wired
// (ADR-0024 WP7). The membership is already written; a relay failure is
// logged and reported as not delivered, never as a failed invite.
func (h *Handler) deliverProjectInvitation(r *http.Request, projectID int, email string) bool {
	if h.mailer == nil || !h.mailer.Configured(r.Context()) {
		return false
	}
	projectName := ""
	_ = h.pool.QueryRow(r.Context(), `SELECT name FROM centry.project WHERE id = $1`, projectID).Scan(&projectName)
	invitedBy := ""
	if principal, ok := auth.UserFromContext(r.Context()); ok {
		invitedBy = principal.Email
	}
	if err := h.mailer.SendInvitation(r.Context(), appmailer.Invitation{
		Email: email, InvitedBy: invitedBy, ProjectName: projectName,
	}); err != nil {
		slog.Warn("project invitation e-mail not sent", "email", email, "project_id", projectID, "reason", err.Error())
		return false
	}
	return true
}

// inviteOne runs one address in its own transaction so a rejected address
// never rolls back the ones that succeeded — the partial-success shape the
// per-email result array reports.
func (h *Handler) inviteOne(
	r *http.Request, projectID int, email string, roleIDs []int,
) (userID int, alreadyMember bool, err error) {
	ctx := r.Context()
	tx, err := h.pool.Begin(ctx)
	if err != nil {
		return 0, false, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	err = tx.QueryRow(ctx,
		`SELECT id FROM auth_core__user WHERE lower(email) = $1`, email).Scan(&userID)
	switch {
	case err == nil:
		var existing int
		if err := tx.QueryRow(ctx,
			`SELECT COUNT(*) FROM auth_core__project_user_role WHERE project_id = $1 AND user_id = $2`,
			projectID, userID).Scan(&existing); err != nil {
			return 0, false, err
		}
		if existing > 0 {
			return userID, true, nil
		}
	case errors.Is(err, pgx.ErrNoRows):
		if err := tx.QueryRow(ctx,
			`INSERT INTO auth_core__user (email, name) VALUES ($1, '') RETURNING id`,
			email).Scan(&userID); err != nil {
			return 0, false, err
		}
	default:
		return 0, false, err
	}

	for _, roleID := range roleIDs {
		if _, err := tx.Exec(ctx,
			`INSERT INTO auth_core__project_user_role (project_id, user_id, role_id)
			 VALUES ($1, $2, $3) ON CONFLICT (project_id, user_id, role_id) DO NOTHING`,
			projectID, userID, roleID); err != nil {
			return 0, false, err
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return 0, false, err
	}
	return userID, false, nil
}

// usersUpdateRequest accepts every id spelling that reaches this route.
// `userId` is what the current client sends (entities/user/model/useEditUser.ts:30
// for one user, and a COMMA-JOINED string of ids for a batch edit at :61);
// `id`/`ids` are pylon's own spellings (admin/api/v2/users.py's put) and are
// accepted so the old SPA and the API-testing suite keep working.
type usersUpdateRequest struct {
	UserID json.RawMessage `json:"userId"`
	Legacy json.RawMessage `json:"user_id"`
	ID     json.RawMessage `json:"id"`
	IDs    []json.Number   `json:"ids"`
	Roles  []string        `json:"roles"`
}

// userIDs flattens every accepted spelling into ints. A raw value may be a
// number (3), a string ("3"), or the comma-joined string the batch edit sends
// ("3,4").
func (req usersUpdateRequest) userIDs() ([]int, error) {
	ids := make([]int, 0, 2)
	seen := make(map[int]struct{})
	add := func(value int) {
		if _, dup := seen[value]; dup {
			return
		}
		seen[value] = struct{}{}
		ids = append(ids, value)
	}

	for _, raw := range []json.RawMessage{req.UserID, req.Legacy, req.ID} {
		if len(raw) == 0 || string(raw) == "null" {
			continue
		}
		text := strings.Trim(string(raw), `"`)
		for _, part := range strings.Split(text, ",") {
			part = strings.TrimSpace(part)
			if part == "" {
				continue
			}
			value, err := strconv.Atoi(part)
			if err != nil || value <= 0 {
				return nil, fmt.Errorf("user id %q is not a positive integer", part)
			}
			add(value)
		}
	}
	for _, number := range req.IDs {
		value, err := strconv.Atoi(number.String())
		if err != nil || value <= 0 {
			return nil, fmt.Errorf("user id %q is not a positive integer", number.String())
		}
		add(value)
	}
	return ids, nil
}

// UsersUpdate implements PUT /api/v2/admin/users/{mode}/{projectID} — "these
// users now hold exactly these roles in this project". Replacement, not merge:
// pylon's update_project_user_roles deletes the user's rows for the project
// before inserting the new set, and the Edit-roles dialog is a checkbox list
// whose unchecked boxes have to mean something.
//
// THE DELETE BELOW IS NOT A REMOVAL, and no token binding is revoked for it
// (spec-llm-project-scope §7 invariant 3). The rows go and come straight back
// inside one transaction, so the user never stops being a member. Revoking
// bindings here would unbind every access key of every edited user on an
// ordinary role change — a silent loss of the project a key bills, caused by
// an operation that says "roles updated". UsersDelete is the removal path, and
// it is the only place that revokes. TestUsersUpdateRoleChangeKeepsTokenProjectBindings
// pins this.
func (h *Handler) UsersUpdate(w http.ResponseWriter, r *http.Request) {
	projectID, ok := h.userWriteContext(w, r)
	if !ok {
		return
	}

	var body usersUpdateRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid request body"})
		return
	}

	userIDs, err := body.userIDs()
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	if len(userIDs) == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "user id is required"})
		return
	}
	// An empty role set would silently remove the users from the project — a
	// destructive outcome behind an "edit roles" call. DELETE is the verb for
	// that, and the dialog's Save is disabled at zero roles anyway
	// (EditUserRolesDialog.tsx: `disabled={!selectedRoleIds.length || …}`).
	if len(body.Roles) == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "roles is required"})
		return
	}

	roleIDs, unknown, err := h.resolveProjectRoleIDs(r, projectID, body.Roles)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "failed to read project roles"})
		return
	}
	if len(unknown) > 0 {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error": fmt.Sprintf("unknown role(s) for this project: %s", strings.Join(unknown, ", ")),
		})
		return
	}

	ctx := r.Context()
	tx, err := h.pool.Begin(ctx)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "failed to update user roles"})
		return
	}
	defer func() { _ = tx.Rollback(ctx) }()

	for _, userID := range userIDs {
		if _, err := tx.Exec(ctx,
			`DELETE FROM auth_core__project_user_role WHERE project_id = $1 AND user_id = $2`,
			projectID, userID); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "failed to update user roles"})
			return
		}
		for _, roleID := range roleIDs {
			if _, err := tx.Exec(ctx,
				`INSERT INTO auth_core__project_user_role (project_id, user_id, role_id)
				 VALUES ($1, $2, $3) ON CONFLICT (project_id, user_id, role_id) DO NOTHING`,
				projectID, userID, roleID); err != nil {
				writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "failed to update user roles"})
				return
			}
		}
	}

	if err := tx.Commit(ctx); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "failed to update user roles"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"msg": "roles updated"})
}

// deleteUserIDs reads the `id[]` query key. The client sends ONE occurrence
// with the ids comma-joined (`?id[]=3,4`, shared/api/generated/admin/admin.ts's
// getUserDeleteUrl stringifies the array), while pylon's own `id[]` may repeat;
// both are accepted.
func deleteUserIDs(r *http.Request) ([]int, error) {
	query := r.URL.Query()
	raw := append([]string{}, query["id[]"]...)
	raw = append(raw, query["id"]...)

	ids := make([]int, 0, len(raw))
	seen := make(map[int]struct{})
	for _, value := range raw {
		for _, part := range strings.Split(value, ",") {
			part = strings.TrimSpace(part)
			if part == "" {
				continue
			}
			parsed, err := strconv.Atoi(part)
			if err != nil || parsed <= 0 {
				return nil, fmt.Errorf("user id %q is not a positive integer", part)
			}
			if _, dup := seen[parsed]; dup {
				continue
			}
			seen[parsed] = struct{}{}
			ids = append(ids, parsed)
		}
	}
	return ids, nil
}

// revokeTokenProjectBindingsSQL enforces spec-llm-project-scope §7 invariant 3:
// "a binding MUST NOT outlive membership". The join through
// public.auth_core__token.user_id is what makes the sweep precise. It removes
// exactly the bindings that name THIS project AND belong to a token of one of
// THESE users — never another member's key for the same project, and never the
// same person's key for a project they still belong to.
//
// The spec offers two implementations and prefers this one: revoking on the
// membership write costs nothing on the request path, where the alternative
// (re-checking membership for every bound token at resolution time) adds work
// to every /llm call.
//
// This is inline SQL, not a generated sqlc query, for the same reason every
// other statement in this file is: package eliteacore holds no sqlcgen.Queries
// and no handler here constructs one. A single generated call would pull the
// sqlcgen dependency, a *sqlcgen.Queries built with WithTx, and a second
// spelling of "how this package reads the database" into one function. The
// binding write path DOES use sqlc (CreateTokenProjectBinding in
// internal/db/queries/auth_pat.sql), because internal/api/v2/auth is a
// sqlc-based package.
const revokeTokenProjectBindingsSQL = `
DELETE FROM elitea_identity.token_project_binding AS binding
USING public.auth_core__token AS token
WHERE binding.token_id = token.id
  AND binding.project_id = $1
  AND token.user_id = ANY($2)`

// UsersDelete implements DELETE /api/v2/admin/users/{mode}/{projectID} —
// remove the users from THIS project. Parity with pylon's
// remove_users_from_project: it clears the project role rows only. The
// auth_core__user row survives, because the person may be a member of other
// projects and "remove from project" is not "delete the account".
//
// It also revokes the access-token bindings that name this project for these
// users. The two writes share ONE transaction on purpose. Split apart, a
// failure between them leaves a token that still bills a project its owner has
// left — which is the exact state invariant 3 forbids, and it would persist
// until someone noticed. The handler used to run the assignment delete as a
// bare pool.Exec; a second bare Exec beside it would have been that split.
func (h *Handler) UsersDelete(w http.ResponseWriter, r *http.Request) {
	projectID, ok := h.userWriteContext(w, r)
	if !ok {
		return
	}

	userIDs, err := deleteUserIDs(r)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	if len(userIDs) == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "id[] is required"})
		return
	}

	ctx := r.Context()
	tx, err := h.pool.Begin(ctx)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "failed to remove users"})
		return
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx, revokeTokenProjectBindingsSQL, projectID, userIDs); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "failed to remove users"})
		return
	}
	if _, err := tx.Exec(ctx,
		`DELETE FROM auth_core__project_user_role WHERE project_id = $1 AND user_id = ANY($2)`,
		projectID, userIDs); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "failed to remove users"})
		return
	}
	if err := tx.Commit(ctx); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "failed to remove users"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"msg": "users successfully removed"})
}
