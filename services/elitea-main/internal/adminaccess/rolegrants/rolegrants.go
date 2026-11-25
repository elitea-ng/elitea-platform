// Package rolegrants is the single writer of central role assignments and of
// their history, elitea_auth.admin_role_grants (shared 0260, ADR-0059 decision
// 8, issue #2499).
//
// Every source that gives or takes a central role calls it: the admin Users
// route and the modes route (console or api), the first-login grant of
// identity.initial_global_admins (bootstrap), an account delete (RevokeAll),
// and later the operator command,
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
	"slices"

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

// ErrReservedAccount means the user is the reserved break-glass account
// (ADR-0059 decision 5, story R2.6b), which holds no role on any path. The
// caller rolls its transaction back.
var ErrReservedAccount = errors.New("rolegrants: the reserved break-glass account holds no role")

// ReservedAccountEmail is the address of the reserved break-glass account
// (adminaccess.ReservedBreakGlassEmail; this package cannot import that one,
// and adminaccess's reserved_account_test.go keeps the two equal).
const ReservedAccountEmail = "break-glass@centry.user"

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
// elitea_auth.admin_role_grants; Grant and Revoke call it after the
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
//
// It refuses the reserved break-glass account with ErrReservedAccount, in
// every mode and from every source: this is the one INSERT into
// auth_core__user_role, so no assignment path can give that account a role.
func Grant(ctx context.Context, q Querier, userID int64, mode, role string, origin Origin) (bool, error) {
	var reserved bool
	if err := q.QueryRow(ctx, `
SELECT EXISTS (SELECT 1 FROM public.auth_core__user WHERE id = $1 AND lower(email) = $2)`,
		userID, ReservedAccountEmail).Scan(&reserved); err != nil {
		return false, fmt.Errorf("rolegrants: read the user of %s/%s: %w", mode, role, err)
	}
	if reserved {
		return false, ErrReservedAccount
	}
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

// Replace makes `roles` the user's set in `mode` (ADR-0059 R0.8b, issue
// 2574); an empty set takes every role of the mode. It takes each held role
// of the mode that is not in `roles`, then gives each role of `roles` that the
// user does not hold yet. A role in both is not touched: no history row, and
// its assignment row keeps its id. It returns the changes it wrote,
// revocations first, each in name order; none when the user already holds
// exactly `roles`. ErrRoleNotFound leaves the caller to roll back.
func Replace(ctx context.Context, q Querier, userID int64, mode string, roles []string, origin Origin) ([]Change, error) {
	held, err := heldRoles(ctx, q, userID, mode)
	if err != nil {
		return nil, err
	}
	wanted := make(map[string]bool, len(roles))
	for _, role := range roles {
		if role != "" {
			wanted[role] = true
		}
	}
	holds := make(map[string]bool, len(held))
	var changes []Change
	for _, name := range held {
		holds[name] = true
		if wanted[name] {
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
	given := make([]string, 0, len(wanted))
	for name := range wanted {
		if !holds[name] {
			given = append(given, name)
		}
	}
	slices.Sort(given)
	for _, name := range given {
		granted, err := Grant(ctx, q, userID, mode, name, origin)
		if err != nil {
			return nil, err
		}
		if granted {
			changes = append(changes, Change{UserID: userID, RoleName: name, RoleMode: mode, Action: ActionGranted})
		}
	}
	return changes, nil
}

// RevokeAll takes every central role the user holds, in every mode, and
// writes one history row per role (issue 2922). An account delete calls it
// before the account row goes: auth_core__user_role cascades from
// auth_core__user, so without it the roles would vanish with no history row
// and the history would show them as still granted. It returns the changes
// it wrote, in (mode, name) order.
//
// Every mode, the default one included, on purpose: rolegrants writes every
// row of auth_core__user_role, so every removal is recorded. A role granted
// before the history existed (or by a migration) gets a revoked row with no
// granted one, as any revocation of such a role does.
func RevokeAll(ctx context.Context, q Querier, userID int64, origin Origin) ([]Change, error) {
	rows, err := q.Query(ctx, `
SELECT role.mode, role.name
FROM public.auth_core__user_role assignment
JOIN public.auth_core__role role ON role.id = assignment.role_id
WHERE assignment.user_id = $1
ORDER BY role.mode, role.name
FOR UPDATE OF assignment`, userID)
	if err != nil {
		return nil, fmt.Errorf("rolegrants: read held roles: %w", err)
	}
	held, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Change, error) {
		change := Change{UserID: userID, Action: ActionRevoked}
		err := row.Scan(&change.RoleMode, &change.RoleName)
		return change, err
	})
	if err != nil {
		return nil, fmt.Errorf("rolegrants: read held roles: %w", err)
	}
	changes := make([]Change, 0, len(held))
	for _, change := range held {
		removed, err := Revoke(ctx, q, userID, change.RoleMode, change.RoleName, origin)
		if err != nil {
			return nil, err
		}
		if removed {
			changes = append(changes, change)
		}
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
