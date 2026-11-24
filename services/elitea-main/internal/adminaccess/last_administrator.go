package adminaccess

// The last-administrator rule (ADR-0059 decision 3, "Other rules"; story
// R0.11, issue 2500).
//
// The last administrator is the last unsuspended holder of the PERMISSION
// admin.auth.users.super_admin or, when nobody holds it, the last unsuspended
// holder of the `admin` role. The second half is for a database that came
// from legacy data, where people hold `admin` while nobody holds
// `super_admin`. When nobody holds either (a fresh database: 001_initial.sql
// gives the bootstrap account `admin` and shared 0111 takes it away) there is
// nothing to protect.
//
// That person cannot be demoted, suspended, deleted or deactivated, on any
// path: the Users route (set_admin_role, delete), the modes route, the
// suspend route and SCIM. The operator command `elitea-main admin-role revoke
// --override-last-administrator` is the only override.
//
// The check runs under LockRoleWrites, in the transaction of the change: two
// concurrent demotions of the last two holders both take the lock first, so
// the second reads the first's commit and is refused. A count without the
// lock passes that race.

import (
	"context"
	"fmt"
	"net/http"
)

// CodeLastAdministrator: the write would leave the deployment with no
// administrator. It is a 409, and binds a global administrator too.
const CodeLastAdministrator = "last_administrator"

// LastAdministratorAuditAction is the action the audit row of a request the
// rule refused carries (RefusalAuditAction is for the no-escalation rule).
const LastAdministratorAuditAction = "role.last_administrator_refused"

// SuccessorCommand is the operator command a refusal names for giving a
// successor a role when no second administrator can.
const SuccessorCommand = "elitea-main admin-role grant"

// administratorLevel says which half of the rule protects the deployment.
type administratorLevel int

const (
	// levelNone: nobody holds either; nothing is protected.
	levelNone administratorLevel = iota
	// levelAdmin: nobody holds the permission, somebody holds `admin`.
	levelAdmin
	// levelSuperAdmin: somebody holds the permission.
	levelSuperAdmin
)

// Administrators counts the unsuspended holders of each half of the rule.
type Administrators struct {
	// SuperAdmins hold admin.auth.users.super_admin through an
	// administration role.
	SuperAdmins int
	// Admins hold the administration `admin` role.
	Admins int
}

func (a Administrators) level() administratorLevel {
	switch {
	case a.SuperAdmins > 0:
		return levelSuperAdmin
	case a.Admins > 0:
		return levelAdmin
	default:
		return levelNone
	}
}

// Protected reports whether there is an administrator to protect.
func (a Administrators) Protected() bool { return a.level() != levelNone }

// CountAdministrators counts the holders of the rule, leaving out the users in
// `without` (a what-if: the deployment as it would be if those accounts were
// gone).
//
// The permission is counted through any role except an unassignable one: on
// a legacy-backed database the `system` role holds it too, and the platform
// principals that hold `system` administer nothing from the console, so
// counting them would leave the last person unprotected. Unassignable is the
// built-in record's marker; a database without the record table (before
// shared 0263) has nothing marked, and `system` is unassignable by name
// there, as the assignment routes treat it.
//
// Unsuspended is `suspended = false`, as the permission resolver reads it
// (legacyrbac): an account the resolver would not let act is no
// administrator.
func CountAdministrators(ctx context.Context, q Querier, without ...int64) (Administrators, error) {
	if without == nil {
		without = []int64{}
	}
	var seeded bool
	if err := q.QueryRow(ctx,
		`SELECT to_regclass('elitea_auth.admin_role_seeds') IS NOT NULL`).Scan(&seeded); err != nil {
		return Administrators{}, fmt.Errorf("count administrators: %w", err)
	}
	unassignable := `role.name = '` + RoleSystem + `'`
	if seeded {
		unassignable = `EXISTS (
           SELECT 1 FROM elitea_auth.admin_role_seeds seed
           WHERE seed.role_id = role.id AND seed.marker = '` + string(MarkerUnassignable) + `')`
	}
	var counts Administrators
	err := q.QueryRow(ctx, `
SELECT count(DISTINCT account.id) FILTER (WHERE EXISTS (
           SELECT 1 FROM public.auth_core__role_permission grant_row
           WHERE grant_row.role_id = role.id AND grant_row.permission = $1)
         AND NOT `+unassignable+`),
       count(DISTINCT account.id) FILTER (WHERE role.name = $2)
FROM public.auth_core__user account
JOIN public.auth_core__user_role assignment ON assignment.user_id = account.id
JOIN public.auth_core__role role ON role.id = assignment.role_id AND role.mode = 'administration'
WHERE account.suspended = false
  AND account.id <> ALL($3::bigint[])`,
		SuperAdminPermission, RoleAdmin, without).Scan(&counts.SuperAdmins, &counts.Admins)
	if err != nil {
		return Administrators{}, fmt.Errorf("count administrators: %w", err)
	}
	return counts, nil
}

// LastAdministratorGuard measures one write's effect on the administrators.
// Take it with BeginLastAdministratorGuard on the write's transaction BEFORE
// the write, and call Check on the same transaction before the commit.
type LastAdministratorGuard struct {
	before Administrators
}

// BeginLastAdministratorGuard takes LockRoleWrites (a no-op when the
// transaction holds it already; take it as the FIRST statement, see
// HoldRoleWrites) and counts the administrators.
func BeginLastAdministratorGuard(ctx context.Context, q Querier) (*LastAdministratorGuard, error) {
	if err := LockRoleWrites(ctx, q); err != nil {
		return nil, err
	}
	before, err := CountAdministrators(ctx, q)
	if err != nil {
		return nil, err
	}
	return &LastAdministratorGuard{before: before}, nil
}

// Before is the count the guard took.
func (g *LastAdministratorGuard) Before() Administrators { return g.before }

// Check counts again after the write and refuses it when it removed the last
// administrator: the permission had a holder and has none, or, on a
// database where nobody held the permission, `admin` had a holder and
// neither half has one now.
func (g *LastAdministratorGuard) Check(ctx context.Context, q Querier) (*Refusal, error) {
	return g.check(ctx, q)
}

// CheckWithout refuses when the deployment without the accounts `userIDs`
// would have lost its last administrator. It is the question a write asks
// before it suspends or deactivates them, without writing.
func (g *LastAdministratorGuard) CheckWithout(ctx context.Context, q Querier, userIDs ...int64) (*Refusal, error) {
	return g.check(ctx, q, userIDs...)
}

func (g *LastAdministratorGuard) check(ctx context.Context, q Querier, without ...int64) (*Refusal, error) {
	if g == nil || !g.before.Protected() {
		return nil, nil
	}
	after, err := CountAdministrators(ctx, q, without...)
	if err != nil {
		return nil, err
	}
	if after.level() < g.before.level() {
		return LastAdministrator(), nil
	}
	return nil, nil
}

// LastAdministrator is the 409 of a write that would remove the last
// administrator. It names the operator command for a successor.
func LastAdministrator() *Refusal {
	return &Refusal{
		Code: CodeLastAdministrator,
		Message: "this change would leave the deployment with no administrator: " +
			"give another person the role first, in the console or with `" + SuccessorCommand + "`",
		Status: http.StatusConflict,
	}
}
