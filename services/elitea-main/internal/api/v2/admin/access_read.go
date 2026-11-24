package admin

// The Access reads (ADR-0059 R0.10b, issue 2588, decision 8 "Recording role
// use"): who holds which administration role, and the grant history.
//
// Both are gated on `access.roles.view` (or the names it narrows,
// `admin.auth.users` and `modes.users`), so a security auditor reads them
// without the user directory, which stays on `admin.auth.users`. Decision 6:
// a list of administrators by name is a target list for phishing, so the
// names are shown only here, to a holder of that permission.
//
//   - GET /admin/access/roles lists the holders of administration roles and
//     nobody else: no pagination over every user, no search over
//     non-administrators. It is NOT the user directory.
//   - GET /admin/access/history lists elitea_auth.admin_role_grants (shared
//     0260), newest first, filterable by user and role, paged.

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/auth"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/pkg/apierr"
)

// accessRoleHolderRole is one administration role a holder holds.
type accessRoleHolderRole struct {
	Name string `json:"name"`
	// BuiltIn is true when the role has a row in elitea_auth.admin_role_seeds.
	BuiltIn bool `json:"built_in"`
	// Marker is the seed row's marker (declared, as_found, unassignable),
	// null for a custom role.
	Marker *string `json:"marker"`
}

// accessRoleHolder is one person who holds at least one administration role.
type accessRoleHolder struct {
	UserID    int                    `json:"user_id"`
	Name      string                 `json:"name"`
	Email     string                 `json:"email"`
	Suspended bool                   `json:"suspended"`
	Roles     []accessRoleHolderRole `json:"roles"`
}

// AccessRoles serves `GET /admin/access/roles`.
func (h *Handler) AccessRoles(w http.ResponseWriter, r *http.Request) {
	if h.pool == nil {
		apierr.WriteStatus(w, http.StatusServiceUnavailable, "database unavailable")
		return
	}
	ctx := r.Context()
	holders, err := h.accessRoleHolders(ctx)
	if err != nil {
		apierr.WriteStatus(w, http.StatusInternalServerError, "failed to list role holders")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"total": len(holders), "rows": holders})
}

func (h *Handler) accessRoleHolders(ctx context.Context) ([]accessRoleHolder, error) {
	seeded, err := h.seedRecordPresent(ctx, h.pool)
	if err != nil {
		return nil, err
	}
	// The seed record is absent on a database whose auth_core tables arrived
	// after shared 0263; nothing is built-in there.
	marker := `NULL::text`
	seedJoin := ``
	if seeded {
		marker = `seed.marker`
		seedJoin = `LEFT JOIN elitea_auth.admin_role_seeds seed ON seed.role_id = role.id`
	}
	rows, err := h.pool.Query(ctx, `
SELECT account.id,
       COALESCE(account.name, account.email, ''),
       COALESCE(account.email, ''),
       account.suspended,
       role.name,
       `+marker+`
FROM public.auth_core__user_role assignment
JOIN public.auth_core__role role ON role.id = assignment.role_id AND role.mode = $1
JOIN public.auth_core__user account ON account.id = assignment.user_id
`+seedJoin+`
ORDER BY lower(COALESCE(account.name, account.email, '')), account.id, role.name`,
		auth.PermissionModeAdministration)
	if err != nil {
		return nil, fmt.Errorf("list role holders: %w", err)
	}
	defer rows.Close()
	holders := make([]accessRoleHolder, 0)
	for rows.Next() {
		var holder accessRoleHolder
		var role accessRoleHolderRole
		if err := rows.Scan(&holder.UserID, &holder.Name, &holder.Email, &holder.Suspended,
			&role.Name, &role.Marker); err != nil {
			return nil, fmt.Errorf("scan role holder: %w", err)
		}
		role.BuiltIn = role.Marker != nil
		if last := len(holders) - 1; last >= 0 && holders[last].UserID == holder.UserID {
			holders[last].Roles = append(holders[last].Roles, role)
			continue
		}
		holder.Roles = []accessRoleHolderRole{role}
		holders = append(holders, holder)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list role holders: %w", err)
	}
	return holders, nil
}

// accessHistoryRow is one row of elitea_auth.admin_role_grants, with the
// names of the account and the actor where the accounts still exist.
type accessHistoryRow struct {
	ID          int64     `json:"id"`
	UserID      int       `json:"user_id"`
	UserName    *string   `json:"user_name"`
	UserEmail   *string   `json:"user_email"`
	RoleName    string    `json:"role_name"`
	RoleMode    string    `json:"role_mode"`
	Action      string    `json:"action"`
	ActorUserID *int      `json:"actor_user_id"`
	ActorName   *string   `json:"actor_name"`
	ActorEmail  *string   `json:"actor_email"`
	OccurredAt  time.Time `json:"occurred_at"`
	Reason      *string   `json:"reason"`
	Source      string    `json:"source"`
	SourceRef   *string   `json:"source_ref"`
}

// accessHistoryMaxLimit bounds one page of the history.
const accessHistoryMaxLimit = 200

// AccessHistory serves `GET /admin/access/history`.
//
// Query: `user_id` and `role` filter (both optional), `limit` (1..200,
// default 50) and `offset` page. Newest first.
func (h *Handler) AccessHistory(w http.ResponseWriter, r *http.Request) {
	if h.pool == nil {
		apierr.WriteStatus(w, http.StatusServiceUnavailable, "database unavailable")
		return
	}
	query := r.URL.Query()
	limit, offset := 50, 0
	if raw := query.Get("limit"); raw != "" {
		value, err := strconv.Atoi(raw)
		if err != nil || value < 1 || value > accessHistoryMaxLimit {
			apierr.WriteStatus(w, http.StatusBadRequest,
				fmt.Sprintf("limit must be between 1 and %d", accessHistoryMaxLimit))
			return
		}
		limit = value
	}
	if raw := query.Get("offset"); raw != "" {
		value, err := strconv.Atoi(raw)
		if err != nil || value < 0 {
			apierr.WriteStatus(w, http.StatusBadRequest, "offset must be a non-negative integer")
			return
		}
		offset = value
	}
	conditions := []string{"TRUE"}
	args := []any{}
	if raw := query.Get("user_id"); raw != "" {
		value, err := strconv.Atoi(raw)
		if err != nil || value <= 0 {
			apierr.WriteStatus(w, http.StatusBadRequest, "user_id must be a positive integer")
			return
		}
		args = append(args, value)
		conditions = append(conditions, "grant_row.user_id = $"+strconv.Itoa(len(args)))
	}
	if role := strings.TrimSpace(query.Get("role")); role != "" {
		args = append(args, role)
		conditions = append(conditions, "grant_row.role_name = $"+strconv.Itoa(len(args)))
	}
	where := strings.Join(conditions, " AND ")

	ctx := r.Context()
	var total int
	if err := h.pool.QueryRow(ctx,
		`SELECT count(*) FROM elitea_auth.admin_role_grants grant_row WHERE `+where, args...,
	).Scan(&total); err != nil {
		apierr.WriteStatus(w, http.StatusInternalServerError, "failed to read the role history")
		return
	}
	pageArgs := append(append([]any{}, args...), limit, offset)
	rows, err := h.pool.Query(ctx, `
SELECT grant_row.id, grant_row.user_id, account.name, account.email,
       grant_row.role_name, grant_row.role_mode, grant_row.action,
       grant_row.actor_user_id, actor.name, actor.email,
       grant_row.occurred_at, grant_row.reason, grant_row.source, grant_row.source_ref
FROM elitea_auth.admin_role_grants grant_row
LEFT JOIN public.auth_core__user account ON account.id = grant_row.user_id
LEFT JOIN public.auth_core__user actor ON actor.id = grant_row.actor_user_id
WHERE `+where+`
ORDER BY grant_row.occurred_at DESC, grant_row.id DESC
LIMIT $`+strconv.Itoa(len(args)+1)+` OFFSET $`+strconv.Itoa(len(args)+2), pageArgs...)
	if err != nil {
		apierr.WriteStatus(w, http.StatusInternalServerError, "failed to read the role history")
		return
	}
	history, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (accessHistoryRow, error) {
		var entry accessHistoryRow
		err := row.Scan(&entry.ID, &entry.UserID, &entry.UserName, &entry.UserEmail,
			&entry.RoleName, &entry.RoleMode, &entry.Action,
			&entry.ActorUserID, &entry.ActorName, &entry.ActorEmail,
			&entry.OccurredAt, &entry.Reason, &entry.Source, &entry.SourceRef)
		return entry, err
	})
	if err != nil {
		apierr.WriteStatus(w, http.StatusInternalServerError, "failed to read the role history")
		return
	}
	if history == nil {
		history = []accessHistoryRow{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"total": total, "rows": history})
}
