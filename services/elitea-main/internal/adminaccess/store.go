package adminaccess

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"

	"github.com/jackc/pgx/v5"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/auth"
)

// Querier is the part of a pool or a transaction these reads need. A guard
// that measures a write's effect runs them on the write's own transaction.
type Querier interface {
	Query(context.Context, string, ...any) (pgx.Rows, error)
	QueryRow(context.Context, string, ...any) pgx.Row
}

/* ── the caller ────────────────────────────────────────────────────────── */

// Caller is the caller's own authority, read from the database for each
// request. Nothing the client sent takes part in it.
//
// It is read through the standing-set call (auth.ResolveStandingAdministration;
// ADR-0059 decision 3, "The exemption is read from standing roles only",
// story R2.1b): an elevation takes no part in it, so a super_admin-level
// elevation neither carries the exemption nor widens the set the subset rule
// measures against.
type Caller struct {
	UserID int64
	// Administration is the caller's STANDING administration-mode set.
	Administration Set
	// SuperAdmin is true when Administration holds SuperAdminPermission.
	SuperAdmin bool
	// Elevated is true when an active elevation bound to the request's
	// browser session takes part in the caller's FULL administration
	// resolution (ADR-0059 decision 5, "What an elevation cannot do"; story
	// R2.8b). The role rules still measure against Administration; the
	// named rules of an elevation (ElevationProtectedTargets, the
	// provisional grants) read this.
	Elevated bool
	// Elevations are the active elevations of the session, when Elevated.
	Elevations []auth.ElevationGrant
	// Effective is the caller's full administration set: Administration
	// and what the elevations add. It equals Administration when the
	// caller is not elevated.
	Effective Set

	principal auth.User
	resolver  auth.PermissionResolver
}

// ResolveCaller reads the caller of a request. It fails closed: with no
// resolver, no principal or a failed read, it returns a refusal.
func ResolveCaller(r *http.Request, resolver auth.PermissionResolver) (Caller, *Refusal) {
	unresolved := Unresolved("your own permissions could not be read, so the role change was refused")
	if resolver == nil {
		return Caller{}, unresolved
	}
	principal, ok := auth.UserFromContext(r.Context())
	if !ok {
		return Caller{}, unresolved
	}
	resolution, err := auth.ResolveStandingAdministration(r.Context(), resolver, principal)
	if err != nil || resolution.UserID <= 0 {
		return Caller{}, unresolved
	}
	administration := SetOf(resolution.Permissions...)
	caller := Caller{
		UserID:         resolution.UserID,
		Administration: administration,
		SuperAdmin:     administration[SuperAdminPermission],
		Effective:      administration,
		principal:      principal,
		resolver:       resolver,
	}
	if err := caller.readElevation(r.Context()); err != nil {
		return Caller{}, unresolved
	}
	return caller, nil
}

// readElevation reads whether an elevation takes part in the caller's full
// administration resolution. Only a credential that can hold an elevation
// (a browser session with a server-side row) is read twice; for any other
// the caller is not elevated with no second query. A failed read is an
// error: the caller of a write fails closed.
func (c *Caller) readElevation(ctx context.Context) error {
	c.Elevated, c.Elevations, c.Effective = false, nil, c.Administration
	if c.resolver == nil || c.principal.Credential().ElevationSessionRef() <= 0 {
		return nil
	}
	resolution, err := c.resolver.ResolvePermissions(ctx, c.principal, ModeAdministration, "")
	if errors.Is(err, auth.ErrPermissionDenied) {
		return nil
	}
	if err != nil {
		return err
	}
	if resolution.UserID != c.UserID || !resolution.Elevated() {
		return nil
	}
	c.Elevated = true
	c.Elevations = append([]auth.ElevationGrant(nil), resolution.Elevations...)
	effective := SetOf(resolution.Permissions...)
	for name := range c.Administration {
		effective[name] = true
	}
	c.Effective = effective
	return nil
}

// ElevationIDs answers the ids of the caller's active elevations, sorted as
// the resolver sorts them.
func (c Caller) ElevationIDs() []string {
	ids := make([]string, 0, len(c.Elevations))
	for _, elevation := range c.Elevations {
		ids = append(ids, elevation.ID)
	}
	return ids
}

// Reload reads the caller again. A write calls it after LockRoleWrites, so
// that the set it checks against is the one no concurrent role write can
// change before the commit.
func (c Caller) Reload(ctx context.Context) (Caller, error) {
	if c.resolver == nil {
		return Caller{}, errors.New("adminaccess: the caller has no resolver")
	}
	resolution, err := auth.ResolveStandingAdministration(ctx, c.resolver, c.principal)
	if err != nil {
		return Caller{}, err
	}
	if resolution.UserID != c.UserID {
		return Caller{}, errors.New("adminaccess: the caller resolved to another user")
	}
	administration := SetOf(resolution.Permissions...)
	c.Administration = administration
	c.SuperAdmin = administration[SuperAdminPermission]
	if err := c.readElevation(ctx); err != nil {
		return Caller{}, err
	}
	return c, nil
}

// roleWritesLock is the key of the transaction-scoped advisory lock that every
// write of role contents or role assignments takes (LockRoleWrites). It is an
// arbitrary constant; "2488" ends it, for the issue.
const roleWritesLock int64 = 0x726f6c6532488

// LockRoleWrites serialises the writes that change role contents or role
// assignments, until the transaction ends. Take it as the FIRST statement of
// the transaction (see HoldRoleWrites for why). A check that runs after it, inside
// the same transaction, cannot be outdated by a concurrent role write before
// the commit: a matrix save that widens a role cannot slip between the
// assignment's check and its INSERT. Role writes are rare, so one lock is
// cheaper than reasoning about row locks across four tables.
func LockRoleWrites(ctx context.Context, q Querier) error {
	var locked bool
	if err := q.QueryRow(ctx, `SELECT true FROM pg_advisory_xact_lock($1)`, roleWritesLock).Scan(&locked); err != nil {
		return fmt.Errorf("lock role writes: %w", err)
	}
	return nil
}

// Beginner begins a transaction; *pgxpool.Pool is one.
type Beginner interface {
	Begin(ctx context.Context) (pgx.Tx, error)
}

// HoldRoleWrites begins a transaction that holds LockRoleWrites and writes
// nothing. It is for a write that commits in transactions of its own (one per
// invited address, one per bulk-invite pair, the SCIM store's): the caller
// runs its check on the returned transaction, then its writes, then release.
// No other role write can start between the check and the last commit.
//
// It cannot deadlock against another role write because every transaction
// that takes the lock takes it as its FIRST statement: a waiter holds no row,
// so the writes that run while the lock is held never wait on one.
func HoldRoleWrites(ctx context.Context, b Beginner) (pgx.Tx, func(), error) {
	tx, err := b.Begin(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("begin role-writes hold: %w", err)
	}
	release := func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }
	if err := LockRoleWrites(ctx, tx); err != nil {
		release()
		return nil, nil, err
	}
	return tx, release, nil
}

// CentralSet is the caller's set in a central mode.
func (c Caller) CentralSet(ctx context.Context, mode string) (Set, error) {
	if mode == ModeAdministration {
		return c.Administration, nil
	}
	resolution, err := c.resolver.ResolvePermissions(ctx, c.principal, mode, "")
	if errors.Is(err, auth.ErrPermissionDenied) {
		return Set{}, nil
	}
	if err != nil {
		return nil, err
	}
	return SetOf(resolution.Permissions...), nil
}

// ProjectSet is the caller's set in one project, as the route gates resolve
// it. A project the caller cannot act in (suspended, unknown) grants nothing.
func (c Caller) ProjectSet(ctx context.Context, projectID int) (Set, error) {
	resolution, err := c.resolver.ResolvePermissions(ctx, c.principal, ModeDefault, strconv.Itoa(projectID))
	if errors.Is(err, auth.ErrPermissionDenied) {
		return Set{}, nil
	}
	if err != nil {
		return nil, err
	}
	return SetOf(resolution.Permissions...), nil
}

/* ── reads ─────────────────────────────────────────────────────────────── */

// HeldCentralRoles are the roles of one central mode that the user holds.
func HeldCentralRoles(ctx context.Context, q Querier, userID int64, mode string) (map[string]bool, error) {
	rows, err := q.Query(ctx, `
SELECT role.name
FROM public.auth_core__user_role assignment
JOIN public.auth_core__role role ON role.id = assignment.role_id
WHERE assignment.user_id = $1 AND role.mode = $2`, userID, mode)
	if err != nil {
		return nil, fmt.Errorf("read held roles: %w", err)
	}
	names, err := scanStrings(rows)
	if err != nil {
		return nil, err
	}
	held := make(map[string]bool, len(names))
	for _, name := range names {
		held[name] = true
	}
	return held, nil
}

// CentralRolePermissions is the union of the sets of central roles in a mode.
func CentralRolePermissions(ctx context.Context, q Querier, mode string, roles []string) ([]string, error) {
	if len(roles) == 0 {
		return []string{}, nil
	}
	rows, err := q.Query(ctx, `
SELECT DISTINCT grant_row.permission
FROM public.auth_core__role role
JOIN public.auth_core__role_permission grant_row ON grant_row.role_id = role.id
WHERE role.mode = $1 AND role.name = ANY($2::text[]) AND grant_row.permission IS NOT NULL
ORDER BY 1`, mode, roles)
	if err != nil {
		return nil, fmt.Errorf("read central role permissions: %w", err)
	}
	return scanStrings(rows)
}

// UserCentralRoles are the roles of one central mode that a user holds.
func UserCentralRoles(ctx context.Context, q Querier, userID int, mode string) ([]string, error) {
	rows, err := q.Query(ctx, `
SELECT DISTINCT role.name
FROM public.auth_core__user_role assignment
JOIN public.auth_core__role role ON role.id = assignment.role_id
WHERE assignment.user_id = $1 AND role.mode = $2
ORDER BY 1`, userID, mode)
	if err != nil {
		return nil, fmt.Errorf("read held roles: %w", err)
	}
	return scanStrings(rows)
}

// ProjectRolesPermissions is what a member who holds exactly these project
// roles resolves in the project. It follows legacyrbac.projectPermissions: the
// project's own grants for the roles, or, when those roles have none, the
// central default-mode grants of the roles of the same name.
func ProjectRolesPermissions(ctx context.Context, q Querier, projectID int, roleIDs []int) ([]string, error) {
	if len(roleIDs) == 0 {
		return []string{}, nil
	}
	rows, err := q.Query(ctx, `
WITH roles AS (
    SELECT id, name FROM public.auth_core__project_role
    WHERE project_id = $1 AND id = ANY($2::int[])
), own AS (
    SELECT DISTINCT project_grant.permission
    FROM roles
    JOIN public.auth_core__project_role_permission project_grant
      ON project_grant.project_id = $1 AND project_grant.role_id = roles.id
)
SELECT permission FROM own
UNION
SELECT central_grant.permission
FROM roles
JOIN public.auth_core__role central_role
  ON central_role.name = roles.name AND central_role.mode = 'default'
JOIN public.auth_core__role_permission central_grant
  ON central_grant.role_id = central_role.id AND central_grant.permission IS NOT NULL
WHERE NOT EXISTS (SELECT 1 FROM own)
ORDER BY 1`, projectID, roleIDs)
	if err != nil {
		return nil, fmt.Errorf("read project role permissions: %w", err)
	}
	return scanStrings(rows)
}

// MembersProjectRoleIDs are the project roles the users hold in the project.
func MembersProjectRoleIDs(ctx context.Context, q Querier, projectID int, userIDs []int) ([]int, error) {
	if len(userIDs) == 0 {
		return []int{}, nil
	}
	rows, err := q.Query(ctx, `
SELECT DISTINCT role_id FROM public.auth_core__project_user_role
WHERE project_id = $1 AND user_id = ANY($2::int[])
ORDER BY 1`, projectID, userIDs)
	if err != nil {
		return nil, fmt.Errorf("read member roles: %w", err)
	}
	defer rows.Close()
	ids := []int{}
	for rows.Next() {
		var id int
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan member role: %w", err)
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// CallerProjectSets is the user's effective set in every project the user is
// a member of, per project. It is legacyrbac.membershipPermissions without the
// final union, so a gain in one project is not hidden by a grant in another.
// Run it on a write's transaction before and after the write to measure the
// write's effect on the caller.
func CallerProjectSets(ctx context.Context, q Querier, userID int64) (map[int64]Set, error) {
	rows, err := q.Query(ctx, `
WITH assigned_roles AS (
    SELECT assignment.project_id, project_role.id, project_role.name
    FROM public.auth_core__project_user_role AS assignment
    JOIN public.auth_core__project_role AS project_role
      ON project_role.id = assignment.role_id
     AND project_role.project_id = assignment.project_id
    JOIN centry.project AS project
      ON project.id = assignment.project_id
     AND project.suspended = false
    WHERE assignment.user_id = $1
), project_permissions AS (
    SELECT DISTINCT assigned_roles.project_id, project_grant.permission
    FROM assigned_roles
    JOIN public.auth_core__project_role_permission AS project_grant
      ON project_grant.project_id = assigned_roles.project_id
     AND project_grant.role_id = assigned_roles.id
)
SELECT project_id, permission FROM project_permissions
UNION
SELECT assigned_roles.project_id, central_grant.permission
FROM assigned_roles
JOIN public.auth_core__role AS central_role
  ON central_role.name = assigned_roles.name
 AND central_role.mode = 'default'
JOIN public.auth_core__role_permission AS central_grant
  ON central_grant.role_id = central_role.id
 AND central_grant.permission IS NOT NULL
WHERE NOT EXISTS (
    SELECT 1 FROM project_permissions
    WHERE project_permissions.project_id = assigned_roles.project_id
)`, userID)
	if err != nil {
		return nil, fmt.Errorf("read caller project permissions: %w", err)
	}
	defer rows.Close()
	sets := map[int64]Set{}
	for rows.Next() {
		var projectID int64
		var permission string
		if err := rows.Scan(&projectID, &permission); err != nil {
			return nil, fmt.Errorf("scan caller project permission: %w", err)
		}
		if sets[projectID] == nil {
			sets[projectID] = Set{}
		}
		sets[projectID][permission] = true
	}
	return sets, rows.Err()
}

/* ── the effect guard ──────────────────────────────────────────────────── */

// SelfGuard measures one project-plane write's effect on the caller. Take a
// snapshot with BeginSelfGuard on the write's transaction BEFORE the write,
// and call Check on the same transaction before the commit.
type SelfGuard struct {
	caller Caller
	before map[int64]Set
}

// BeginSelfGuard snapshots the caller's per-project sets. A super_admin caller
// is exempt, and gets a guard that never refuses.
//
// It takes LockRoleWrites first, so the before-and-after compare cannot be
// skewed by a concurrent role write.
func BeginSelfGuard(ctx context.Context, q Querier, caller Caller) (*SelfGuard, error) {
	if err := LockRoleWrites(ctx, q); err != nil {
		return nil, err
	}
	guard := &SelfGuard{caller: caller}
	if caller.SuperAdmin {
		return guard, nil
	}
	before, err := CallerProjectSets(ctx, q, caller.UserID)
	if err != nil {
		return nil, err
	}
	guard.before = before
	return guard, nil
}

// Check returns a refusal when the write widened the caller's own set in any
// project. It returns an error for a failed read.
func (g *SelfGuard) Check(ctx context.Context, q Querier) error {
	if g == nil || g.caller.SuperAdmin {
		return nil
	}
	after, err := CallerProjectSets(ctx, q, g.caller.UserID)
	if err != nil {
		return err
	}
	if gain, widened := Widened(g.before, after); widened {
		return SelfEscalation(gain)
	}
	return nil
}

func scanStrings(rows pgx.Rows) ([]string, error) {
	defer rows.Close()
	values := []string{}
	for rows.Next() {
		var value string
		if err := rows.Scan(&value); err != nil {
			return nil, fmt.Errorf("scan: %w", err)
		}
		values = append(values, value)
	}
	return values, rows.Err()
}
