package auth

import (
	"context"
	"errors"
	"slices"
)

const (
	PermissionModeAdministration = "administration"
	PermissionModeDeveloper      = "developer"
	PermissionModeDefault        = "default"
)

// ErrPermissionDenied is the sentinel for an authorization REFUSAL.
//
// A PermissionResolver must return this error, or an error that wraps it, when
// it refuses the principal. The resolver must return any other error only for
// an infrastructure failure: a lost database connection, a query timeout, or a
// canceled context. Callers use the difference to select the status code. A
// refusal is 403. An infrastructure failure is 500.
//
// Do not report an infrastructure failure as a refusal. The operator then sees
// a wall of 403 answers, with no error rate signal, and audits roles instead of
// the database.
var ErrPermissionDenied = errors.New("permission denied")

type PermissionResolution struct {
	// UserID is the RESOLVED user, and an implementation returning a nil error
	// must populate it with a positive id.
	//
	// It is not an echo of what the principal claimed. For a token principal
	// the claimed id is the TOKEN's, and reporting the owning user is the whole
	// point of resolving: callers use this field as a user foreign key and as
	// the identity they display. A resolver that leaves it zero on success
	// makes every such caller drop the resolution — silently, since there is
	// nothing to log.
	UserID int64
	// Permissions may be empty. Empty means "this principal holds none in this
	// mode", which is an ANSWER; a refusal is ErrPermissionDenied instead.
	Permissions []string
	// ElevationSessionRef is the browser session an elevation may be bound
	// to (auth.Credential.ElevationSessionRef), 0 when the credential that
	// was resolved can hold none. Only the administration mode sets it; the
	// elevation union (ADR-0059 R2.1) reads it.
	ElevationSessionRef int64
	// Roles is the caller's STANDING administration roles, by name, read by
	// the same query as Permissions: the owner's roles for every kind whose
	// set the owner's roles decide (session, form, native, and a scoped or
	// inherit token), and empty for a kind that acts with none. Only the
	// administration mode sets it; the admin audit row records it as
	// acting_roles (ADR-0059 R1.4a).
	Roles []string
	// TokenAdminAccess is the admin_access of the personal access token
	// behind the principal (none, scoped, inherit; shared 0265), "" for every
	// other credential. Only the administration mode sets it; the token rule
	// of the action table reads it (ADR-0059 R1.2b).
	TokenAdminAccess string
	// ElevatedPermissions are the permissions of Permissions that only an
	// active elevation bound to the session gives (ADR-0059 decision 5,
	// story R2.1b): the union of the open elevations' level sets minus the
	// standing set. A permission the standing set holds too is not here, so
	// "the permission that admitted the request came from the elevation"
	// (R2.8a) is membership in this list. Sorted; nil when no elevation took
	// part. Only the administration mode sets it.
	ElevatedPermissions []string
	// ElevationIDs are the active elevations bound to the session that took
	// part in Permissions, sorted; nil when none did. An elevation adds no
	// role name: Roles stays the standing roles.
	ElevationIDs []string
}

// StandingPermissions is Permissions without what only an elevation gives:
// the set the caller's standing roles decide for this credential. Sorted
// when Permissions is, never nil.
func (r PermissionResolution) StandingPermissions() []string {
	standing := make([]string, 0, len(r.Permissions))
	for _, permission := range r.Permissions {
		if !slices.Contains(r.ElevatedPermissions, permission) {
			standing = append(standing, permission)
		}
	}
	return standing
}

// Elevated reports whether an elevation took part in the resolution.
func (r PermissionResolution) Elevated() bool {
	return len(r.ElevationIDs) > 0 || len(r.ElevatedPermissions) > 0
}

// StandingAdministrationResolver is the standing-set call (ADR-0059 decision
// 3, "The exemption is read from standing roles only"; story R2.1b): the
// administration-mode resolution of ResolvePermissions with no elevation and
// no provisional grant in it, ever. The role rules, the super_admin
// exemption and the project self-membership check read the caller's own set
// from it, so a super_admin-level elevation never carries the exemption.
type StandingAdministrationResolver interface {
	ResolveStandingAdministration(ctx context.Context, principal User) (PermissionResolution, error)
}

// ResolveStandingAdministration answers the standing administration set of
// principal through resolver: its standing-set call when it has one, and
// otherwise its administration-mode resolution with whatever it reports as
// elevated taken out. Either way the answer reports no elevation.
func ResolveStandingAdministration(ctx context.Context, resolver PermissionResolver, principal User) (PermissionResolution, error) {
	if resolver == nil {
		return PermissionResolution{}, ErrPermissionDenied
	}
	if standing, ok := resolver.(StandingAdministrationResolver); ok {
		resolution, err := standing.ResolveStandingAdministration(ctx, principal)
		if err != nil {
			return PermissionResolution{}, err
		}
		return withoutElevation(resolution), nil
	}
	resolution, err := resolver.ResolvePermissions(ctx, principal, PermissionModeAdministration, "")
	if err != nil {
		return PermissionResolution{}, err
	}
	return withoutElevation(resolution), nil
}

func withoutElevation(resolution PermissionResolution) PermissionResolution {
	resolution.Permissions = resolution.StandingPermissions()
	resolution.ElevatedPermissions = nil
	resolution.ElevationIDs = nil
	return resolution
}

// PermissionResolver resolves the permissions of a principal in one mode.
//
// The implementation must obey the ErrPermissionDenied contract above.
type PermissionResolver interface {
	ResolvePermissions(
		ctx context.Context,
		principal User,
		mode string,
		projectID string,
	) (PermissionResolution, error)
}

// MembershipPermissionResolver answers "does this principal hold the
// permission ANYWHERE" — the union of what they hold across every project they
// are a member of.
//
// It exists for the routes whose ANSWER is already scoped to the caller's own
// memberships, and which therefore have no single project to resolve against.
// The project list is the first one: its query returns the caller's projects
// only, but its gate resolved against the public project in the URL, so an
// account that is not a member of the public project read 403 instead of its
// own projects (#830).
//
// The mode contract is that of PermissionResolver. In the CENTRAL modes
// (administration, developer) no project takes part in the answer, so an
// implementation must resolve them exactly as ResolvePermissions does. In
// DEFAULT mode the answer is the union over the caller's memberships.
//
// The ErrPermissionDenied contract above holds unchanged: a principal with no
// membership at all is not a refusal, it is an EMPTY permission set, and the
// gate turns that into 403.
type MembershipPermissionResolver interface {
	ResolveMembershipPermissions(
		ctx context.Context,
		principal User,
		mode string,
	) (PermissionResolution, error)
}
