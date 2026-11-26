package legacyrbac

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/adminaccess"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/audit"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/auth"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/db/provisionalgrants"
)

// ErrPermissionDenied is the refusal sentinel of the auth.PermissionResolver
// contract. It is an alias, so a caller that tests errors.Is(err,
// auth.ErrPermissionDenied) separates a refusal from a database failure.
var ErrPermissionDenied = auth.ErrPermissionDenied

type postgresStore interface {
	Query(context.Context, string, ...any) (pgx.Rows, error)
	QueryRow(context.Context, string, ...any) pgx.Row
}

type PostgresResolver struct {
	store postgresStore
	// now is the clock the elevation predicate is evaluated at (nil:
	// time.Now). Access is the predicate, never a job, so a test clock past
	// the expiry is enough to show an elevation grants nothing.
	now func() time.Time
}

var (
	_ auth.PermissionResolver             = (*PostgresResolver)(nil)
	_ auth.MembershipPermissionResolver   = (*PostgresResolver)(nil)
	_ auth.StandingAdministrationResolver = (*PostgresResolver)(nil)
)

func NewPostgresResolver(pool *pgxpool.Pool) *PostgresResolver {
	if pool == nil {
		return &PostgresResolver{}
	}
	return &PostgresResolver{store: pool}
}

func (r *PostgresResolver) ResolvePermissions(
	ctx context.Context,
	principal auth.User,
	mode string,
	projectID string,
) (auth.PermissionResolution, error) {
	if r == nil || r.store == nil {
		return auth.PermissionResolution{}, ErrPermissionDenied
	}

	mode, err := normalizeMode(mode)
	if err != nil {
		return auth.PermissionResolution{}, err
	}
	userID, err := r.resolveUserID(ctx, principal)
	if err != nil {
		return auth.PermissionResolution{}, err
	}

	if mode == auth.PermissionModeAdministration {
		return r.administrationResolution(ctx, principal, userID, true)
	}
	if principal.Credential().Kind == auth.CredentialKindSealed {
		// A sealed session reaches the administration surface only
		// (R2.7a): it holds nothing in the developer and default modes,
		// whatever its person's memberships.
		return auth.PermissionResolution{UserID: userID, Permissions: []string{}}, nil
	}
	var permissions []string
	switch mode {
	case auth.PermissionModeDeveloper:
		permissions, err = r.centralPermissions(ctx, userID, mode)
	case auth.PermissionModeDefault:
		var id int64
		id, err = parsePositiveID(projectID)
		if err == nil {
			err = r.requireActiveProject(ctx, id)
		}
		if err == nil {
			permissions, err = r.projectPermissions(ctx, userID, id)
		}
	}
	if err != nil {
		return auth.PermissionResolution{}, err
	}
	if permissions == nil {
		permissions = []string{}
	}
	return auth.PermissionResolution{UserID: userID, Permissions: permissions}, nil
}

// ResolveMembershipPermissions is the auth.MembershipPermissionResolver half of
// this resolver: the caller's permissions in EVERY project they are a member
// of, unioned, with no project taken from the request.
//
// The central modes are answered exactly as ResolvePermissions answers them,
// because they never consult a project.
func (r *PostgresResolver) ResolveMembershipPermissions(
	ctx context.Context,
	principal auth.User,
	mode string,
) (auth.PermissionResolution, error) {
	if r == nil || r.store == nil {
		return auth.PermissionResolution{}, ErrPermissionDenied
	}

	mode, err := normalizeMode(mode)
	if err != nil {
		return auth.PermissionResolution{}, err
	}
	userID, err := r.resolveUserID(ctx, principal)
	if err != nil {
		return auth.PermissionResolution{}, err
	}

	if mode == auth.PermissionModeAdministration {
		return r.administrationResolution(ctx, principal, userID, true)
	}
	if principal.Credential().Kind == auth.CredentialKindSealed {
		// A sealed session reaches the administration surface only
		// (R2.7a): it holds nothing in the developer and default modes,
		// whatever its person's memberships.
		return auth.PermissionResolution{UserID: userID, Permissions: []string{}}, nil
	}
	var permissions []string
	switch mode {
	case auth.PermissionModeDeveloper:
		permissions, err = r.centralPermissions(ctx, userID, mode)
	case auth.PermissionModeDefault:
		permissions, err = r.membershipPermissions(ctx, userID)
	}
	if err != nil {
		return auth.PermissionResolution{}, err
	}
	if permissions == nil {
		permissions = []string{}
	}
	return auth.PermissionResolution{UserID: userID, Permissions: permissions}, nil
}

// ResolveStandingAdministration is the standing-set call (ADR-0059 decision
// 3, "The exemption is read from standing roles only"; story R2.1b): the
// administration-mode resolution of ResolvePermissions with no elevation in
// it, ever, and no provisional grant (R2.12 adds those to ResolvePermissions
// only). It does not read the elevation ledger at all, so its answer is the
// same with and without an active elevation. The role rules, the super_admin
// exemption and the project self-membership check read the caller's own set
// from it (adminaccess.ResolveCaller).
func (r *PostgresResolver) ResolveStandingAdministration(
	ctx context.Context,
	principal auth.User,
) (auth.PermissionResolution, error) {
	if r == nil || r.store == nil {
		return auth.PermissionResolution{}, ErrPermissionDenied
	}
	userID, err := r.resolveUserID(ctx, principal)
	if err != nil {
		return auth.PermissionResolution{}, err
	}
	return r.administrationResolution(ctx, principal, userID, false)
}

// administrationResolution answers the administration mode: the credential
// decides the set (ADR-0059 decision 7, the credentials table). The developer
// and default modes never come here and resolve as before.
//
//   - session, form: the owner's set;
//   - token: by the token row's admin_access, read at this request, so a
//     `scoped` token is the owner's CURRENT set intersected with
//     admin_scopes and a scope the owner loses is lost by the token with no
//     write;
//   - native: the owner's set intersected with the registry permissions
//     marked native_client;
//   - sealed: no standing set at all; with elevated set, its elevation only
//     (R2.7a);
//   - run, system and the zero kind: empty.
//
// With elevated set, a browser session (session, form, sealed) with a
// server-side row also gains the elevation union (ADR-0059 decision 5, "Bound to one
// session"; story R2.1b): the administration set of the level's role of
// every ACTIVE elevation of the same person bound to the same session
// reference (elevationGrants). Every other kind never matches: its
// ElevationSessionRef is 0, and the kind is checked again here. The
// standing-set call passes false and never reads the ledger.
//
// userID is already resolved and checked by the caller, so a suspended owner
// or an expired token was refused before any of this.
func (r *PostgresResolver) administrationResolution(
	ctx context.Context,
	principal auth.User,
	userID int64,
	elevated bool,
) (_ auth.PermissionResolution, failure error) {
	credential := principal.Credential()
	resolution := auth.PermissionResolution{
		UserID:              userID,
		Permissions:         []string{},
		ElevationSessionRef: credential.ElevationSessionRef(),
		Roles:               []string{},
	}
	// The admin audit row of the request records the roles the caller acted
	// with (ADR-0059 R1.4a), from this resolution: no second query. A failed
	// resolution records nothing. An elevation adds no role name.
	defer func() {
		if failure == nil {
			audit.RecordAdministration(ctx, audit.Administration{ActingRoles: resolution.Roles})
			if len(resolution.ElevationIDs) > 0 {
				// The row also records the elevation the request was made
				// under (shared 0268). The slot below the audit middleware is
				// what the row is written from, so the returned context is
				// not needed.
				_ = audit.ContextWithElevationID(ctx, resolution.ElevationIDs[0])
			}
		}
	}()
	if !credentialShapeCoherent(credential) {
		resolution.ElevationSessionRef = 0
		return resolution, nil
	}
	var access TokenAdminAccess
	switch credential.Kind {
	case auth.CredentialKindSession, auth.CredentialKindForm, auth.CredentialKindNative:
	case auth.CredentialKindSealed:
		// A sealed session holds its elevation and nothing else (ADR-0059
		// decision 7): no standing role, no owner permission, so the
		// standing-set call answers empty and the elevated call answers the
		// elevation only (story R2.7a).
		if !elevated || resolution.ElevationSessionRef <= 0 {
			return resolution, nil
		}
		// Assigned to resolution, not returned directly: the deferred
		// audit record reads it.
		var err error
		if resolution, err = r.withElevation(ctx, resolution, userID); err != nil {
			return auth.PermissionResolution{}, err
		}
		return resolution, nil
	case auth.CredentialKindToken:
		var err error
		access, err = r.tokenAdminAccess(ctx, credential.TokenID, userID)
		if err != nil {
			return auth.PermissionResolution{}, err
		}
		resolution.TokenAdminAccess = access.Access
		if access.Access != TokenAdminAccessScoped && access.Access != TokenAdminAccessInherit {
			return resolution, nil
		}
	default:
		// run, system and the zero kind: no query, nothing held.
		return resolution, nil
	}
	roles, owner, err := r.administrationRoles(ctx, userID)
	if err != nil {
		return auth.PermissionResolution{}, err
	}
	resolution.Roles = roles
	resolution.Permissions = AdministrationSet(credential.Kind, access, owner)
	if !elevated || resolution.ElevationSessionRef <= 0 {
		return resolution, nil
	}
	switch credential.Kind {
	case auth.CredentialKindSession, auth.CredentialKindForm:
	default:
		return resolution, nil
	}
	if resolution, err = r.withElevation(ctx, resolution, userID); err != nil {
		return auth.PermissionResolution{}, err
	}
	if resolution, err = r.withProvisionalRoles(ctx, resolution, userID); err != nil {
		return auth.PermissionResolution{}, err
	}
	return resolution, nil
}

// withProvisionalRoles adds to a browser session's resolution the
// administration roles other people gave this person under an elevation that
// is still active (ADR-0059 decision 5, "A role given under an elevation is
// provisional"; story R2.12a): the rows of provisionalgrants.RoleRows, read at
// the resolver's clock, so the grant stops the moment its elevation ends with
// no job running.
//
// A provisional permission is resolved as what it is, a permission of
// someone else's elevation: it is added to ElevatedPermissions (unless the
// standing set holds it too) and the granting elevation to Elevations. So
// what an elevation cannot do binds the grantee as it binds the grantor (the
// restriction rule reads ElevatedPermissions), the grantee's flagged changes
// carry that elevation's id into its review, and a role the grantee gives on
// is provisional too. Role names stay the standing ones.
//
// Only the full resolution of a browser session calls it (session or form,
// with a server-side row): the standing-set call never does, so a
// provisionally granted person is never exempt and never counts as an
// approver or a reviewer.
func (r *PostgresResolver) withProvisionalRoles(
	ctx context.Context,
	resolution auth.PermissionResolution,
	userID int64,
) (auth.PermissionResolution, error) {
	if !provisionalgrants.Enabled() {
		return resolution, nil
	}
	rows, err := r.store.Query(ctx, `
SELECT grant_row.elevation_id, grant_row.level, grant_row.path, role_grant.permission
FROM `+provisionalgrants.RoleRows("$3")+` AS grant_row
JOIN public.auth_core__role AS role
  ON role.name = grant_row.role_name AND role.mode = $2
LEFT JOIN public.auth_core__role_permission AS role_grant ON role_grant.role_id = role.id
WHERE grant_row.user_id = $1`,
		userID, auth.PermissionModeAdministration, r.clock().UTC())
	if err != nil {
		return auth.PermissionResolution{}, fmt.Errorf("resolve the provisional roles: %w", err)
	}
	defer rows.Close()
	// The person's own elevations stay first, in the resolver's order, so
	// ElevationID (and the elevation a provisional write of the person binds
	// to) is the person's own; the grantors' elevations follow, by id.
	own := resolution.Elevations
	var grantors []auth.ElevationGrant
	granted := append([]string(nil), resolution.ElevatedPermissions...)
	found := false
	for rows.Next() {
		var elevation auth.ElevationGrant
		var permission *string
		if err := rows.Scan(&elevation.ID, &elevation.Level, &elevation.Path, &permission); err != nil {
			return auth.PermissionResolution{}, fmt.Errorf("scan a provisional role: %w", err)
		}
		found = true
		if !slices.ContainsFunc(own, func(e auth.ElevationGrant) bool { return e.ID == elevation.ID }) {
			grantors = append(grantors, elevation)
		}
		if permission != nil {
			granted = append(granted, *permission)
		}
	}
	if err := rows.Err(); err != nil {
		return auth.PermissionResolution{}, fmt.Errorf("read the provisional roles: %w", err)
	}
	if !found {
		return resolution, nil
	}
	resolution.Permissions, resolution.ElevatedPermissions = UnionElevation(resolution.StandingPermissions(), granted)
	slices.SortFunc(grantors, func(a, b auth.ElevationGrant) int { return strings.Compare(a.ID, b.ID) })
	grantors = slices.CompactFunc(grantors, func(a, b auth.ElevationGrant) bool { return a.ID == b.ID })
	elevations := append(append([]auth.ElevationGrant(nil), own...), grantors...)
	ids := make([]string, 0, len(elevations))
	for _, elevation := range elevations {
		ids = append(ids, elevation.ID)
	}
	resolution.Elevations, resolution.ElevationIDs, resolution.ElevationID = elevations, ids, ids[0]
	return resolution, nil
}

// withElevation adds to a resolution the union of the active elevations of
// the person bound to its session reference (elevationGrants).
func (r *PostgresResolver) withElevation(
	ctx context.Context,
	resolution auth.PermissionResolution,
	userID int64,
) (auth.PermissionResolution, error) {
	elevations, granted, err := r.elevationGrants(ctx, userID, resolution.ElevationSessionRef)
	if err != nil {
		return auth.PermissionResolution{}, err
	}
	resolution.Permissions, resolution.ElevatedPermissions = UnionElevation(resolution.Permissions, granted)
	var ids []string
	for _, elevation := range elevations {
		ids = append(ids, elevation.ID)
	}
	resolution.ElevationIDs = ids
	resolution.Elevations = elevations
	if len(ids) > 0 {
		// The resolver's elevation indicator (R1.3a reads it to refuse
		// token scopes under an elevation): set whenever an active
		// elevation is bound to the session, even one that adds nothing
		// the standing set lacks.
		resolution.ElevationID = ids[0]
	}
	return resolution, nil
}

// UnionElevation answers the union of a standing set and what the active
// elevations grant, and the part of it that only the elevations give (nil
// when that is empty). Both are sorted and de-duplicated; union is never nil.
func UnionElevation(standing, granted []string) (union, elevatedOnly []string) {
	union = append(make([]string, 0, len(standing)+len(granted)), standing...)
	for _, permission := range granted {
		if !slices.Contains(standing, permission) {
			elevatedOnly = append(elevatedOnly, permission)
			union = append(union, permission)
		}
	}
	slices.Sort(union)
	slices.Sort(elevatedOnly)
	return slices.Compact(union), slices.Compact(elevatedOnly)
}

// elevationGrants reads the active elevations of the person bound to ONE
// browser session, and the administration permissions of their levels' roles
// (elitea_auth.admin_elevations, shared 0269).
//
//   - Both the person and the session reference must match: an elevation of
//     one person never reaches another person's session, nor a second
//     session of the same person.
//   - Active is the predicate, evaluated at the resolver's clock: approved,
//     started, no end, and the expiry in the future. No sweeper has to run
//     for an elevation to stop granting.
//   - The level joins its role by NAME in the administration mode at read
//     time, so a later grant migration that changes the role reaches an
//     active elevation; no grant is ever copied.
//   - Every matching row counts, and the answer is the union of their sets:
//     the ledger allows one open elevation per session, and the resolver
//     never picks one row of several.
//
// The elevations are the rows that matched, with their level and path,
// sorted by id, nil for none; a level whose role is missing or empty still
// names its elevation and grants nothing.
func (r *PostgresResolver) elevationGrants(ctx context.Context, userID, sessionRef int64) ([]auth.ElevationGrant, []string, error) {
	rows, err := r.store.Query(ctx, `
SELECT elevation.id::text, elevation.level, elevation.path, role_grant.permission
FROM elitea_auth.admin_elevations AS elevation
LEFT JOIN public.auth_core__role AS role
  ON role.name = elevation.level AND role.mode = $3
LEFT JOIN public.auth_core__role_permission AS role_grant ON role_grant.role_id = role.id
WHERE elevation.user_id = $1
  AND elevation.browser_session_ref = $2
  AND elevation.decision = 'approved'
  AND elevation.started_at IS NOT NULL
  AND elevation.started_at <= $4
  AND elevation.ended_at IS NULL
  AND elevation.expires_at > $4`,
		userID, sessionRef, auth.PermissionModeAdministration, r.clock().UTC())
	if err != nil {
		return nil, nil, fmt.Errorf("resolve the elevations of the session: %w", err)
	}
	defer rows.Close()
	var elevations []auth.ElevationGrant
	var permissions []string
	for rows.Next() {
		var elevation auth.ElevationGrant
		var permission *string
		if err := rows.Scan(&elevation.ID, &elevation.Level, &elevation.Path, &permission); err != nil {
			return nil, nil, fmt.Errorf("scan an elevation of the session: %w", err)
		}
		elevations = append(elevations, elevation)
		if permission != nil {
			permissions = append(permissions, *permission)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, nil, fmt.Errorf("read the elevations of the session: %w", err)
	}
	slices.SortFunc(elevations, func(a, b auth.ElevationGrant) int { return strings.Compare(a.ID, b.ID) })
	slices.Sort(permissions)
	return slices.CompactFunc(elevations, func(a, b auth.ElevationGrant) bool { return a.ID == b.ID }),
		slices.Compact(permissions), nil
}

func (r *PostgresResolver) clock() time.Time {
	if r.now != nil {
		return r.now()
	}
	return time.Now()
}

// administrationRoles answers the user's administration-mode roles and the
// permissions they grant, in one query: what centralPermissions answers for
// the administration mode, plus the role names the admin audit row records.
// Both are sorted and de-duplicated, and never nil. A role with no grant is a
// role the user holds and is named.
func (r *PostgresResolver) administrationRoles(ctx context.Context, userID int64) ([]string, []string, error) {
	rows, err := r.store.Query(ctx, `
SELECT role.name, role_grant.permission
FROM public.auth_core__user_role AS assignment
JOIN public.auth_core__role AS role ON role.id = assignment.role_id
LEFT JOIN public.auth_core__role_permission AS role_grant ON role_grant.role_id = role.id
WHERE assignment.user_id = $1
  AND role.mode = $2`, userID, auth.PermissionModeAdministration)
	if err != nil {
		return nil, nil, fmt.Errorf("resolve legacy administration roles: %w", err)
	}
	defer rows.Close()
	roles, permissions := []string{}, []string{}
	for rows.Next() {
		var role string
		var permission *string
		if err := rows.Scan(&role, &permission); err != nil {
			return nil, nil, fmt.Errorf("scan legacy administration roles: %w", err)
		}
		roles = append(roles, role)
		if permission != nil {
			permissions = append(permissions, *permission)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, nil, fmt.Errorf("read legacy administration roles: %w", err)
	}
	slices.Sort(roles)
	slices.Sort(permissions)
	return slices.Compact(roles), slices.Compact(permissions), nil
}

// credentialShapeCoherent refuses a kind the principal's other fields
// contradict: a browser session that names a token row, or a token-backed
// kind that names none. No authenticating site builds such a principal, and
// it resolves nothing.
func credentialShapeCoherent(credential auth.Credential) bool {
	switch credential.Kind {
	case auth.CredentialKindSession, auth.CredentialKindForm, auth.CredentialKindSealed:
		return credential.TokenID == ""
	case auth.CredentialKindToken, auth.CredentialKindNative, auth.CredentialKindRun, auth.CredentialKindSystem:
		return credential.TokenID != ""
	}
	return false
}

// The values of auth_core__token.admin_access (shared 0265).
const (
	TokenAdminAccessNone    = "none"
	TokenAdminAccessScoped  = "scoped"
	TokenAdminAccessInherit = "inherit"
)

// TokenAdminAccess is the administration access a personal access token row
// carries (auth_core__token.admin_access and admin_scopes, shared 0265).
type TokenAdminAccess struct {
	Access string
	Scopes []string
}

// tokenScopeAllowed says whether a name may be a token scope at all: its
// registry row opts in (adminaccess.Permission.Token). The write path (R1.3a)
// validates scopes too; this is the read side, so a scope written by hand or
// by a defect (super_admin, identity trust) never resolves. R1.3a opted the
// eligible rows in (adminaccess.tokenScopeEligible). A variable so the
// package's tests can stand in an eligibility of their own.
var tokenScopeAllowed = func(name string) bool {
	row, ok := adminaccess.Lookup(name)
	return ok && row.Token
}

// AdministrationSet is the administration set a credential of kind holds,
// given its owner's standing set (owner) and, for a personal access token,
// the token row's access. It is the one statement of the credentials table of
// ADR-0059 decision 7; the resolver applies it to what PostgreSQL answered.
// A scoped token keeps the scopes its owner still holds and the registry
// admits as token scopes (tokenScopeAllowed).
// The result is sorted, de-duplicated and never nil.
func AdministrationSet(kind auth.CredentialKind, access TokenAdminAccess, owner []string) []string {
	var keep func(string) bool
	switch kind {
	case auth.CredentialKindSession, auth.CredentialKindForm:
		keep = func(string) bool { return true }
	case auth.CredentialKindToken:
		switch access.Access {
		case TokenAdminAccessInherit:
			keep = func(string) bool { return true }
		case TokenAdminAccessScoped:
			keep = func(name string) bool {
				return slices.Contains(access.Scopes, name) && tokenScopeAllowed(name)
			}
		}
	case auth.CredentialKindNative:
		keep = func(name string) bool {
			row, ok := adminaccess.Lookup(name)
			return ok && row.NativeClient
		}
	}
	set := make([]string, 0, len(owner))
	if keep == nil {
		return set
	}
	for _, name := range owner {
		if keep(name) {
			set = append(set, name)
		}
	}
	slices.Sort(set)
	return slices.Compact(set)
}

// tokenAdminAccess reads the administration access of the token row the
// principal names, at this request. It is a separate read, made only for a
// personal access token in the administration mode, so the developer and
// default modes issue exactly the statements they issued before.
func (r *PostgresResolver) tokenAdminAccess(ctx context.Context, tokenID string, userID int64) (TokenAdminAccess, error) {
	id, err := parsePositiveID(tokenID)
	if err != nil {
		return TokenAdminAccess{}, err
	}
	var access TokenAdminAccess
	err = r.store.QueryRow(ctx, `
SELECT token.admin_access, token.admin_scopes
FROM public.auth_core__token AS token
WHERE token.id = $1 AND token.user_id = $2`, id, userID).Scan(&access.Access, &access.Scopes)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return TokenAdminAccess{}, ErrPermissionDenied
		}
		return TokenAdminAccess{}, fmt.Errorf("resolve token administration access: %w", err)
	}
	return access, nil
}

func normalizeMode(mode string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(mode)) {
	case auth.PermissionModeAdministration:
		return auth.PermissionModeAdministration, nil
	case auth.PermissionModeDeveloper:
		return auth.PermissionModeDeveloper, nil
	case auth.PermissionModeDefault, "prompt_lib":
		return auth.PermissionModeDefault, nil
	default:
		return "", ErrPermissionDenied
	}
}

func (r *PostgresResolver) resolveUserID(ctx context.Context, principal auth.User) (int64, error) {
	isToken := principal.TokenID != "" || strings.EqualFold(principal.AuthType, "token")
	if isToken {
		if principal.TokenID == "" || principal.UserID == "" {
			return 0, ErrPermissionDenied
		}
		id, err := parsePositiveID(principal.TokenID)
		if err != nil {
			return 0, err
		}
		claimedUserID, err := parsePositiveID(principal.UserID)
		if err != nil {
			return 0, err
		}
		var userID int64
		err = r.store.QueryRow(ctx, `
WITH token_owner AS MATERIALIZED (
    SELECT token.user_id
    FROM public.auth_core__token AS token
    JOIN public.auth_core__user AS owner ON owner.id = token.user_id
    WHERE token.id = $1
      AND owner.suspended = false
      AND (token.expires IS NULL OR token.expires > (clock_timestamp() AT TIME ZONE 'UTC'))
), last_login_update AS (
    UPDATE public.auth_core__user AS owner
    SET last_login = clock_timestamp() AT TIME ZONE 'UTC'
    WHERE owner.id = (SELECT user_id FROM token_owner)
      AND (
          owner.last_login IS NULL
          OR owner.last_login::date < (clock_timestamp() AT TIME ZONE 'UTC')::date
      )
    RETURNING owner.id
)
SELECT user_id FROM token_owner`, id).Scan(&userID)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return 0, ErrPermissionDenied
			}
			return 0, fmt.Errorf("resolve legacy token owner: %w", err)
		}
		if claimedUserID != userID {
			return 0, ErrPermissionDenied
		}
		return userID, nil
	}
	if principal.UserID != "" {
		userID, err := parsePositiveID(principal.UserID)
		if err != nil {
			return 0, err
		}
		return r.requireUser(ctx, userID)
	}

	userID, err := parsePositiveID(principal.ID)
	if err != nil {
		return 0, err
	}
	return r.requireUser(ctx, userID)
}

func (r *PostgresResolver) requireUser(ctx context.Context, userID int64) (int64, error) {
	var found int64
	err := r.store.QueryRow(ctx,
		`SELECT id FROM public.auth_core__user WHERE id = $1 AND suspended = false`,
		userID,
	).Scan(&found)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return 0, ErrPermissionDenied
		}
		return 0, fmt.Errorf("resolve legacy user: %w", err)
	}
	return found, nil
}

func (r *PostgresResolver) requireActiveProject(ctx context.Context, projectID int64) error {
	var suspended bool
	err := r.store.QueryRow(ctx,
		`SELECT suspended FROM centry.project WHERE id = $1`,
		projectID,
	).Scan(&suspended)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrPermissionDenied
		}
		return fmt.Errorf("resolve legacy project: %w", err)
	}
	if suspended {
		return ErrPermissionDenied
	}
	return nil
}

func (r *PostgresResolver) centralPermissions(ctx context.Context, userID int64, mode string) ([]string, error) {
	rows, err := r.store.Query(ctx, `
SELECT DISTINCT role_grant.permission
FROM public.auth_core__user_role AS assignment
JOIN public.auth_core__role AS role ON role.id = assignment.role_id
JOIN public.auth_core__role_permission AS role_grant ON role_grant.role_id = role.id
WHERE assignment.user_id = $1
  AND role.mode = $2
ORDER BY role_grant.permission`, userID, mode)
	if err != nil {
		return nil, fmt.Errorf("resolve legacy central permissions: %w", err)
	}
	return scanPermissions(rows)
}

func (r *PostgresResolver) projectPermissions(ctx context.Context, userID, projectID int64) ([]string, error) {
	rows, err := r.store.Query(ctx, `
WITH assigned_roles AS (
    SELECT project_role.id, project_role.name
    FROM public.auth_core__project_user_role AS assignment
    JOIN public.auth_core__project_role AS project_role
      ON project_role.id = assignment.role_id
     AND project_role.project_id = assignment.project_id
    WHERE assignment.project_id = $1
      AND assignment.user_id = $2
    UNION
    -- A membership given under an elevation that is still active (ADR-0059
    -- R2.12b) counts as an assigned role, the default fallback included.
    SELECT project_role.id, project_role.name
    FROM `+provisionalgrants.MembershipRows("$3")+` AS provisional
    JOIN public.auth_core__project_role AS project_role
      ON project_role.id = provisional.project_role_id
     AND project_role.project_id = provisional.project_id
    WHERE provisional.project_id = $1
      AND provisional.user_id = $2
), project_permissions AS (
    SELECT DISTINCT project_grant.permission
    FROM assigned_roles
    JOIN public.auth_core__project_role_permission AS project_grant
      ON project_grant.project_id = $1
     AND project_grant.role_id = assigned_roles.id
), effective_permissions AS (
    SELECT permission
    FROM project_permissions
    UNION ALL
    SELECT DISTINCT central_grant.permission
    FROM assigned_roles
    JOIN public.auth_core__role AS central_role
      ON central_role.name = assigned_roles.name
     AND central_role.mode = 'default'
    JOIN public.auth_core__role_permission AS central_grant
      ON central_grant.role_id = central_role.id
    WHERE NOT EXISTS (SELECT 1 FROM project_permissions)
)
SELECT DISTINCT permission
FROM effective_permissions
ORDER BY permission`, projectID, userID, r.clock().UTC())
	if err != nil {
		return nil, fmt.Errorf("resolve legacy project permissions: %w", err)
	}
	return scanPermissions(rows)
}

// membershipPermissions is projectPermissions run over every project the user
// belongs to at once, and it keeps that query's two rules per project:
//
//   - a suspended project grants nothing, which is what requireActiveProject
//     enforces on the single-project path;
//   - the central default-mode fallback applies only to a project that carries
//     no per-project grant row for the caller's roles there, so a project with
//     its own grants still suppresses it — per project, not globally.
func (r *PostgresResolver) membershipPermissions(ctx context.Context, userID int64) ([]string, error) {
	rows, err := r.store.Query(ctx, `
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
    UNION
    -- A membership given under an elevation that is still active (ADR-0059
    -- R2.12b), with the same suspended-project rule.
    SELECT provisional.project_id, project_role.id, project_role.name
    FROM `+provisionalgrants.MembershipRows("$2")+` AS provisional
    JOIN public.auth_core__project_role AS project_role
      ON project_role.id = provisional.project_role_id
     AND project_role.project_id = provisional.project_id
    JOIN centry.project AS project
      ON project.id = provisional.project_id
     AND project.suspended = false
    WHERE provisional.user_id = $1
), project_permissions AS (
    SELECT DISTINCT assigned_roles.project_id, project_grant.permission
    FROM assigned_roles
    JOIN public.auth_core__project_role_permission AS project_grant
      ON project_grant.project_id = assigned_roles.project_id
     AND project_grant.role_id = assigned_roles.id
), effective_permissions AS (
    SELECT permission
    FROM project_permissions
    UNION ALL
    SELECT DISTINCT central_grant.permission
    FROM assigned_roles
    JOIN public.auth_core__role AS central_role
      ON central_role.name = assigned_roles.name
     AND central_role.mode = 'default'
    JOIN public.auth_core__role_permission AS central_grant
      ON central_grant.role_id = central_role.id
    WHERE NOT EXISTS (
        SELECT 1
        FROM project_permissions
        WHERE project_permissions.project_id = assigned_roles.project_id
    )
)
SELECT DISTINCT permission
FROM effective_permissions
ORDER BY permission`, userID, r.clock().UTC())
	if err != nil {
		return nil, fmt.Errorf("resolve legacy membership permissions: %w", err)
	}
	return scanPermissions(rows)
}

func scanPermissions(rows pgx.Rows) ([]string, error) {
	defer rows.Close()
	permissions := make([]string, 0)
	for rows.Next() {
		var permission string
		if err := rows.Scan(&permission); err != nil {
			return nil, fmt.Errorf("scan legacy permission: %w", err)
		}
		permissions = append(permissions, permission)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate legacy permissions: %w", err)
	}
	return permissions, nil
}

func parsePositiveID(value string) (int64, error) {
	id, err := strconv.ParseInt(value, 10, 64)
	if err != nil || id <= 0 {
		return 0, ErrPermissionDenied
	}
	return id, nil
}
