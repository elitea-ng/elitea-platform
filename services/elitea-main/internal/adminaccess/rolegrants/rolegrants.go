// Package rolegrants is the single writer of central role assignments and of
// their history, elitea_auth.admin_role_grants (shared 0260, ADR-0059 decision
// 8, issue #2499).
//
// Every source that gives or takes a central role calls it: the admin Users
// route and the modes route (console or api), the first-login grant of
// identity.initial_global_admins (bootstrap), and later the operator command,
// a confirmed provisional grant (review) and a SCIM binding (scim). Each
// change to public.auth_core__user_role writes one history row in the caller's
// transaction, so the assignment and its history cannot disagree after a
// crash. A role the user already holds is left as it is: no history row, and
// the assignment row keeps its id.
//
// The caller owns the transaction and, for a console or api change, holds
// adminaccess.LockRoleWrites in it.
package rolegrants

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// The actions a history row records.
const (
	ActionGranted = "granted"
	ActionRevoked = "revoked"
)

// The sources a history row records. The table's CHECK constraint holds the
// same list.
const (
	SourceConsole   = "console"
	SourceAPI       = "api"
	SourceSCIM      = "scim"
	SourceMigration = "migration"
	SourceBootstrap = "bootstrap"
	SourceOperator  = "operator"
	SourceReview    = "review"
)

// ErrRoleNotFound means the deployment defines no role of that name in that
// mode. The caller rolls its transaction back: committing would take roles
// away and give nothing.
var ErrRoleNotFound = errors.New("rolegrants: role is not defined in that mode")

// Querier is what the writer needs from the caller's transaction.
type Querier interface {
	Exec(ctx context.Context, sql string, arguments ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// Origin says who made a change and through what.
type Origin struct {
	// ActorUserID is the user who made the change, 0 for none (bootstrap,
	// migration, and a scim or operator change with no person behind it).
	ActorUserID int64
	Source      string
	// SourceRef is a reference within the source, such as a SCIM binding id.
	SourceRef string
	Reason    string
}

// Change is one row of the history.
type Change struct {
	UserID   int64
	RoleName string
	RoleMode string
	Action   string
}

// Record writes one history row. It is the only INSERT into
// elitea_auth.admin_role_grants; Grant, Revoke and Set call it after the
// assignment change they record.
func Record(ctx context.Context, q Querier, change Change, origin Origin) error {
	if change.Action != ActionGranted && change.Action != ActionRevoked {
		return fmt.Errorf("rolegrants: unknown action %q", change.Action)
	}
	if _, err := q.Exec(ctx, `
INSERT INTO elitea_auth.admin_role_grants
    (user_id, role_name, role_mode, action, actor_user_id, reason, source, source_ref)
VALUES ($1, $2, $3, $4, NULLIF($5::bigint, 0)::integer, NULLIF($6, ''), $7, NULLIF($8, ''))`,
		change.UserID, change.RoleName, change.RoleMode, change.Action,
		origin.ActorUserID, origin.Reason, origin.Source, origin.SourceRef,
	); err != nil {
		return fmt.Errorf("rolegrants: record %s of %s/%s: %w",
			change.Action, change.RoleMode, change.RoleName, err)
	}
	return nil
}

// Grant gives the user one role, keeping every other role of the mode. It
// reports whether anything changed: false when the user already held it, in
// which case neither the assignment nor the history is written.
func Grant(ctx context.Context, q Querier, userID int64, mode, role string, origin Origin) (bool, error) {
	var roleID int
	if err := q.QueryRow(ctx, `
SELECT id FROM public.auth_core__role WHERE name = $1 AND mode = $2`, role, mode).Scan(&roleID); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return false, ErrRoleNotFound
		}
		return false, fmt.Errorf("rolegrants: read role %s/%s: %w", mode, role, err)
	}
	tag, err := q.Exec(ctx, `
INSERT INTO public.auth_core__user_role (user_id, role_id)
VALUES ($1, $2)
ON CONFLICT (user_id, role_id) DO NOTHING`, userID, roleID)
	if err != nil {
		return false, fmt.Errorf("rolegrants: assign %s/%s: %w", mode, role, err)
	}
	if tag.RowsAffected() == 0 {
		return false, nil
	}
	return true, Record(ctx, q, Change{UserID: userID, RoleName: role, RoleMode: mode, Action: ActionGranted}, origin)
}

// Revoke takes one role from the user. It reports whether the user held it;
// false writes nothing.
func Revoke(ctx context.Context, q Querier, userID int64, mode, role string, origin Origin) (bool, error) {
	tag, err := q.Exec(ctx, `
DELETE FROM public.auth_core__user_role assignment
USING public.auth_core__role role
WHERE role.id = assignment.role_id
  AND assignment.user_id = $1
  AND role.mode = $2
  AND role.name = $3`, userID, mode, role)
	if err != nil {
		return false, fmt.Errorf("rolegrants: remove %s/%s: %w", mode, role, err)
	}
	if tag.RowsAffected() == 0 {
		return false, nil
	}
	return true, Record(ctx, q, Change{UserID: userID, RoleName: role, RoleMode: mode, Action: ActionRevoked}, origin)
}

// Set makes `role` the user's only role in `mode` ("" for none): it takes
// every other role of the mode and gives `role` when the user does not hold
// it yet. It returns the changes it wrote, none when the user already holds
// exactly that role. ErrRoleNotFound leaves the caller to roll back.
func Set(ctx context.Context, q Querier, userID int64, mode, role string, origin Origin) ([]Change, error) {
	held, err := heldRoles(ctx, q, userID, mode)
	if err != nil {
		return nil, err
	}
	var changes []Change
	alreadyHeld := false
	for _, name := range held {
		if name == role {
			alreadyHeld = true
			continue
		}
		removed, err := Revoke(ctx, q, userID, mode, name, origin)
		if err != nil {
			return nil, err
		}
		if removed {
			changes = append(changes, Change{UserID: userID, RoleName: name, RoleMode: mode, Action: ActionRevoked})
		}
	}
	if role == "" || alreadyHeld {
		return changes, nil
	}
	granted, err := Grant(ctx, q, userID, mode, role, origin)
	if err != nil {
		return nil, err
	}
	if granted {
		changes = append(changes, Change{UserID: userID, RoleName: role, RoleMode: mode, Action: ActionGranted})
	}
	return changes, nil
}

// heldRoles returns the names of the roles the user holds in the mode,
// locking their assignment rows for the rest of the transaction.
func heldRoles(ctx context.Context, q Querier, userID int64, mode string) ([]string, error) {
	rows, err := q.Query(ctx, `
SELECT role.name
FROM public.auth_core__user_role assignment
JOIN public.auth_core__role role ON role.id = assignment.role_id
WHERE assignment.user_id = $1 AND role.mode = $2
ORDER BY role.name
FOR UPDATE OF assignment`, userID, mode)
	if err != nil {
		return nil, fmt.Errorf("rolegrants: read held %s roles: %w", mode, err)
	}
	names, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, fmt.Errorf("rolegrants: read held %s roles: %w", mode, err)
	}
	return names, nil
}
