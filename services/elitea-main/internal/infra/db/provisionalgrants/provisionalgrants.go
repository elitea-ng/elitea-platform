// Package provisionalgrants owns the SQL meaning of "a grant given under an
// elevation is in effect" (ADR-0059 decision 5, "What an elevation cannot
// do"; stories R2.12a, issue 2526, and R2.12b, issue 2606).
//
// An administration role or a project membership that an elevated person
// gives to someone else is a row of elitea_auth.admin_provisional_grants
// (shared 0284), bound to the elevation. It is in effect while that elevation
// is active: approved, started, not ended and not past its expiry. That is a
// predicate over the ledger row, evaluated at the reader's clock, never a job.
//
// Three readers union the rows, and every one of them takes its SQL from this
// package so they cannot drift:
//
//   - the resolver's administration resolution (legacyrbac), the role rows;
//   - the resolver's project queries (legacyrbac projectPermissions and
//     membershipPermissions), the membership rows;
//   - the project access predicate (projectaccess.Membership), the
//     membership rows.
//
// THE SWITCH. The fragments name two elitea_auth tables. A database built
// from the shared corpus has both; the hand-built schemas many integration
// tests use do not, and a statement that names a missing relation fails at
// parse time, whatever guards it. So the fragments read the tables only once
// Enable(true) was called: the composition root calls EnableIfPresent at
// boot, after the database is reachable, and a test that exercises the
// grants enables it for its own length. Disabled, every fragment is an empty
// row set: a provisional grant grants nothing. That is the safe direction,
// since a provisional grant only ever adds access.
package provisionalgrants

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"

	"github.com/jackc/pgx/v5"
)

// Table is the provisional grants table (shared 0284).
const Table = "elitea_auth.admin_provisional_grants"

// The kinds of a row.
const (
	KindRole       = "role"
	KindMembership = "membership"
)

var enabled atomic.Bool

// Enable switches the fragments on or off for the process, and returns the
// previous value.
func Enable(on bool) bool { return enabled.Swap(on) }

// Enabled reports whether the fragments read the tables.
func Enabled() bool { return enabled.Load() }

// Querier is what Present needs; *pgxpool.Pool and pgx.Tx are one.
type Querier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// Present reports whether the database holds the provisional grants table
// and the elevation ledger it joins.
func Present(ctx context.Context, q Querier) (bool, error) {
	var present bool
	if err := q.QueryRow(ctx, `
SELECT to_regclass('`+Table+`') IS NOT NULL
   AND to_regclass('elitea_auth.admin_elevations') IS NOT NULL`).Scan(&present); err != nil {
		return false, fmt.Errorf("read whether the provisional grants table exists: %w", err)
	}
	return present, nil
}

// EnableIfPresent switches the fragments on when the database holds the
// tables (Present), and off otherwise. The composition root calls it once
// at boot; it answers what it decided.
func EnableIfPresent(ctx context.Context, q Querier) (bool, error) {
	present, err := Present(ctx, q)
	if err != nil {
		return false, err
	}
	Enable(present)
	return present, nil
}

// ErrUnavailable refuses a provisional write while the readers are off and
// the table is still absent: a grant nobody reads would be a gift that says
// it was given and grants nothing.
var ErrUnavailable = errors.New("provisional grants are not available yet: the shared migrations have not created " + Table)

// EnsureEnabled is the writers' check: with the readers on it answers nil;
// with them off it probes again, switches them on when the table now exists
// (the migrations ran after the boot), and answers ErrUnavailable when it
// still does not. It never switches the readers off.
func EnsureEnabled(ctx context.Context, q Querier) error {
	if Enabled() {
		return nil
	}
	present, err := Present(ctx, q)
	if err != nil {
		return err
	}
	if !present {
		return ErrUnavailable
	}
	Enable(true)
	return nil
}

// ActiveElevation is the boolean SQL predicate "the elevation row aliased
// alias is active at the SQL time expression at" (a placeholder such as "$3",
// or "now()"). It is the predicate of the resolver's elevation union
// (legacyrbac elevationGrants), repeated here for the grants.
func ActiveElevation(alias, at string) string {
	return fmt.Sprintf(`(%[1]s.decision = 'approved'
       AND %[1]s.started_at IS NOT NULL
       AND %[1]s.started_at <= %[2]s
       AND %[1]s.ended_at IS NULL
       AND %[1]s.expires_at > %[2]s)`, alias, at)
}

// RoleRows is a row set (elevation_id text, level, path, user_id bigint,
// role_name text) of the role grants in effect at the SQL time expression at.
// Use it as a FROM item with an alias. Disabled, it is empty.
func RoleRows(at string) string {
	if !Enabled() {
		return `(SELECT NULL::text AS elevation_id, NULL::text AS level, NULL::text AS path,
        NULL::bigint AS user_id, NULL::text AS role_name WHERE false AND ` + at + `::timestamptz IS NOT NULL)`
	}
	return `(SELECT provisional_role.elevation_id::text AS elevation_id, provisional_elevation.level AS level,
        provisional_elevation.path AS path, provisional_role.user_id AS user_id,
        provisional_role.role_name AS role_name
    FROM ` + Table + ` AS provisional_role
    JOIN elitea_auth.admin_elevations AS provisional_elevation
      ON provisional_elevation.id = provisional_role.elevation_id
    WHERE provisional_role.kind = '` + KindRole + `'
      AND provisional_role.confirmed_at IS NULL
      AND ` + ActiveElevation("provisional_elevation", at) + `)`
}

// MembershipRows is a row set (user_id bigint, project_id bigint,
// project_role_id bigint) of the membership grants in effect at the SQL time
// expression at. Use it as a FROM item with an alias. Disabled, it is empty.
//
// It does not test the project or the person: each reader keeps its own
// rules for those (a suspended project grants nothing, provisional or not).
func MembershipRows(at string) string {
	if !Enabled() {
		return `(SELECT NULL::bigint AS user_id, NULL::bigint AS project_id, NULL::bigint AS project_role_id
    WHERE false AND ` + at + `::timestamptz IS NOT NULL)`
	}
	return `(SELECT provisional_member.user_id AS user_id, provisional_member.project_id AS project_id,
        provisional_member.project_role_id AS project_role_id
    FROM ` + Table + ` AS provisional_member
    JOIN elitea_auth.admin_elevations AS provisional_member_elevation
      ON provisional_member_elevation.id = provisional_member.elevation_id
    WHERE provisional_member.kind = '` + KindMembership + `'
      AND provisional_member.confirmed_at IS NULL
      AND ` + ActiveElevation("provisional_member_elevation", at) + `)`
}

// Cleanuper is the part of testing.TB that EnableForTest needs.
type Cleanuper interface {
	Helper()
	Cleanup(func())
}

// EnableForTest switches the fragments on for the length of t, for a test
// whose database holds the tables. A test that calls it does not run in
// parallel with one that expects the switch off.
func EnableForTest(t Cleanuper) {
	t.Helper()
	previous := Enable(true)
	t.Cleanup(func() { Enable(previous) })
}
