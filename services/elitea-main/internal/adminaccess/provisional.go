package adminaccess

// Provisional grants (ADR-0059 decision 5, "What an elevation cannot do";
// stories R2.12a, issue 2526, and R2.12b, issue 2606).
//
// An elevated person could give a standing administration role, or a project
// membership, to an accomplice and so make the elevation permanent. So what
// an elevated person gives to SOMEONE ELSE is provisional: a row of
// elitea_auth.admin_provisional_grants (shared 0284), bound to the elevation,
// and never a row of auth_core__user_role or auth_core__project_user_role.
//
//   - It is in effect while that elevation is active, for the grantee, in
//     the resolver and in the project access predicate
//     (provisionalgrants). That is a predicate, never a job: when the
//     elevation ends the grant stops.
//   - The standing-set call never reads it: a provisionally granted person
//     is never exempt, never an approver and never a reviewer.
//   - The review of the elevation confirms it (ConfirmProvisionalGrants),
//     only with the outcome `justified` and grant by grant: the reviewer
//     writes the standing assignment, a grant history row with the source
//     `review`. `unauthorized` confirms none, and neither does the terminal
//     state unreviewed_no_reviewer.
//
// The interception is decided by the caller of the write (Caller.Elevated):
// a session under an elevation gives provisionally, a standing global
// administrator's elevated session included. A removal is not intercepted:
// it is a user change, flagged for the review.

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/adminaccess/rolegrants"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/db/provisionalgrants"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/securityaudit"
)

// The kinds of a provisional grant.
const (
	ProvisionalKindRole       = provisionalgrants.KindRole
	ProvisionalKindMembership = provisionalgrants.KindMembership
)

var (
	// ErrProvisionalNoElevation: the caller of a provisional write is under
	// no elevation.
	ErrProvisionalNoElevation = errors.New("provisional grant refused: the caller is under no elevation")
	// ErrProvisionalConfirmOutcome: a confirmation with an outcome other
	// than justified.
	ErrProvisionalConfirmOutcome = fmt.Errorf("%w: only a justified review confirms a provisional grant", ErrElevationInvalid)
	// ErrProvisionalGrantNotFound: the grant is not one of the elevation's,
	// or it was confirmed already.
	ErrProvisionalGrantNotFound = fmt.Errorf("%w: no unconfirmed provisional grant of this elevation has that id", ErrElevationInvalid)
)

// ProvisionalGrant is one row of elitea_auth.admin_provisional_grants.
type ProvisionalGrant struct {
	ID          uuid.UUID
	ElevationID uuid.UUID
	Kind        string
	UserID      int64
	GrantedBy   int64
	// RoleName is the administration role of a role grant.
	RoleName string
	// ProjectID, ProjectRoleID and ProjectRoleName are the membership of a
	// membership grant.
	ProjectID       int64
	ProjectRoleID   int64
	ProjectRoleName string
	CreatedAt       time.Time
	ConfirmedAt     time.Time
	ConfirmedBy     int64
}

// provisionalElevation is the elevation a caller's provisional write is
// bound to: the first of its active elevations, which is the caller's own
// when it has one (the resolver lists a grantor's elevation, through which a
// provisional role reaches the caller, after the caller's own). A caller
// with no elevation gives nothing provisionally.
func provisionalElevation(caller Caller) (uuid.UUID, error) {
	if !caller.Elevated || len(caller.Elevations) == 0 {
		return uuid.Nil, ErrProvisionalNoElevation
	}
	id, err := uuid.Parse(caller.Elevations[0].ID)
	if err != nil {
		return uuid.Nil, fmt.Errorf("provisional grant: the elevation id %q: %w", caller.Elevations[0].ID, err)
	}
	return id, nil
}

// ProvisionalElevationID is the id of the elevation a provisional write of
// caller binds to, "" when the caller is under none: the elevation its
// security audit event names.
func ProvisionalElevationID(caller Caller) string {
	id, err := provisionalElevation(caller)
	if err != nil {
		return ""
	}
	return id.String()
}

// GiveProvisionalRoles records the administration roles `roles` that the
// elevated caller gives to userID, on q (the write's transaction, holding
// LockRoleWrites). It writes no standing assignment and no grant history. A
// role the person already holds provisionally under the same elevation is
// kept as it is. It answers the roles, sorted.
//
// The reserved break-glass account gets nothing (rolegrants.ErrReservedAccount)
// and an undefined role refuses the write (rolegrants.ErrRoleNotFound), as a
// standing grant would.
func GiveProvisionalRoles(ctx context.Context, q ExecQuerier, caller Caller, userID int64, mode string, roles []string, at time.Time) ([]string, error) {
	elevation, err := provisionalElevation(caller)
	if err != nil {
		return nil, err
	}
	if mode != ModeAdministration {
		return nil, fmt.Errorf("provisional grant: only an administration role is provisional, not %q", mode)
	}
	if err := provisionalgrants.EnsureEnabled(ctx, q); err != nil {
		return nil, err
	}
	reserved, err := IsReservedAccount(ctx, q, userID)
	if err != nil {
		return nil, err
	}
	if reserved {
		return nil, rolegrants.ErrReservedAccount
	}
	given := slices.Clone(roles)
	slices.Sort(given)
	given = slices.Compact(given)
	for _, role := range given {
		var defined bool
		if err := q.QueryRow(ctx, `
SELECT EXISTS (SELECT 1 FROM public.auth_core__role WHERE name = $1 AND mode = $2)`, role, mode).Scan(&defined); err != nil {
			return nil, fmt.Errorf("provisional grant: read role %s/%s: %w", mode, role, err)
		}
		if !defined {
			return nil, rolegrants.ErrRoleNotFound
		}
		if _, err := q.Exec(ctx, `
INSERT INTO `+provisionalgrants.Table+`
    (elevation_id, kind, user_id, granted_by, role_mode, role_name, created_at)
VALUES ($1, '`+ProvisionalKindRole+`', $2, $3, $4, $5, $6)
ON CONFLICT (elevation_id, user_id, role_mode, role_name) WHERE kind = '`+ProvisionalKindRole+`' DO NOTHING`,
			elevation, userID, caller.UserID, mode, role, at.UTC()); err != nil {
			return nil, fmt.Errorf("provisional grant: record %s/%s: %w", mode, role, err)
		}
	}
	return given, nil
}

// GiveProvisionalMembership records the project roles roleIDs that the
// elevated caller gives to userID in projectID, on q (the write's
// transaction). It writes no row of auth_core__project_user_role. A role id
// the project does not define is refused.
func GiveProvisionalMembership(ctx context.Context, q ExecQuerier, caller Caller, projectID, userID int64, roleIDs []int, at time.Time) error {
	elevation, err := provisionalElevation(caller)
	if err != nil {
		return err
	}
	if err := provisionalgrants.EnsureEnabled(ctx, q); err != nil {
		return err
	}
	for _, roleID := range roleIDs {
		tag, err := q.Exec(ctx, `
INSERT INTO `+provisionalgrants.Table+`
    (elevation_id, kind, user_id, granted_by, project_id, project_role_id, project_role_name, created_at)
SELECT $1, '`+ProvisionalKindMembership+`', $2, $3, project_role.project_id, project_role.id, project_role.name, $6
FROM public.auth_core__project_role AS project_role
WHERE project_role.project_id = $4 AND project_role.id = $5
ON CONFLICT (elevation_id, user_id, project_id, project_role_id) WHERE kind = '`+ProvisionalKindMembership+`' DO NOTHING`,
			elevation, userID, caller.UserID, projectID, roleID, at.UTC())
		if err != nil {
			return fmt.Errorf("provisional membership: record role %d of project %d: %w", roleID, projectID, err)
		}
		if tag.RowsAffected() == 0 {
			var known bool
			if err := q.QueryRow(ctx, `
SELECT EXISTS (SELECT 1 FROM public.auth_core__project_role WHERE project_id = $1 AND id = $2)`,
				projectID, roleID).Scan(&known); err != nil {
				return fmt.Errorf("provisional membership: read role %d of project %d: %w", roleID, projectID, err)
			}
			if !known {
				return fmt.Errorf("provisional membership: project %d defines no role %d", projectID, roleID)
			}
		}
	}
	return nil
}

const provisionalGrantColumns = `grant_row.id, grant_row.elevation_id, grant_row.kind, grant_row.user_id, grant_row.granted_by,
       COALESCE(grant_row.role_name, ''), COALESCE(grant_row.project_id, 0), COALESCE(grant_row.project_role_id, 0),
       COALESCE(grant_row.project_role_name, ''), grant_row.created_at, grant_row.confirmed_at,
       COALESCE(grant_row.confirmed_by, 0)`

func scanProvisionalGrant(row pgx.CollectableRow) (ProvisionalGrant, error) {
	var grant ProvisionalGrant
	var confirmed *time.Time
	if err := row.Scan(&grant.ID, &grant.ElevationID, &grant.Kind, &grant.UserID, &grant.GrantedBy, &grant.RoleName,
		&grant.ProjectID, &grant.ProjectRoleID, &grant.ProjectRoleName, &grant.CreatedAt, &confirmed,
		&grant.ConfirmedBy); err != nil {
		return ProvisionalGrant{}, err
	}
	grant.CreatedAt = grant.CreatedAt.UTC()
	if confirmed != nil {
		grant.ConfirmedAt = confirmed.UTC()
	}
	return grant, nil
}

// ElevationProvisionalGrants answers every provisional grant of the
// elevation, oldest first. Without the table there are none.
func ElevationProvisionalGrants(ctx context.Context, q Querier, id uuid.UUID) ([]ProvisionalGrant, error) {
	present, err := relationPresent(ctx, q, provisionalgrants.Table)
	if err != nil || !present {
		return []ProvisionalGrant{}, err
	}
	rows, err := q.Query(ctx, `
SELECT `+provisionalGrantColumns+` FROM `+provisionalgrants.Table+` AS grant_row
WHERE grant_row.elevation_id = $1 ORDER BY grant_row.created_at, grant_row.id`, id)
	if err != nil {
		return nil, fmt.Errorf("read the provisional grants: %w", err)
	}
	grants, err := pgx.CollectRows(rows, scanProvisionalGrant)
	if err != nil {
		return nil, fmt.Errorf("read the provisional grants: %w", err)
	}
	return grants, nil
}

// ProvisionalProjectMembers answers the provisional memberships of a project
// that are in effect at `at`, for the project's member listing. Without the
// table, or with the provisional grants switched off, there are none.
func ProvisionalProjectMembers(ctx context.Context, q Querier, projectID int64, at time.Time) ([]ProvisionalGrant, error) {
	if !provisionalgrants.Enabled() {
		return []ProvisionalGrant{}, nil
	}
	rows, err := q.Query(ctx, `
SELECT `+provisionalGrantColumns+` FROM `+provisionalgrants.Table+` AS grant_row
JOIN elitea_auth.admin_elevations AS elevation ON elevation.id = grant_row.elevation_id
WHERE grant_row.kind = '`+ProvisionalKindMembership+`' AND grant_row.project_id = $1
  AND grant_row.confirmed_at IS NULL
  AND `+provisionalgrants.ActiveElevation("elevation", "$2")+`
ORDER BY grant_row.user_id, grant_row.project_role_id`, projectID, at.UTC())
	if err != nil {
		return nil, fmt.Errorf("read the provisional members: %w", err)
	}
	grants, err := pgx.CollectRows(rows, scanProvisionalGrant)
	if err != nil {
		return nil, fmt.Errorf("read the provisional members: %w", err)
	}
	return grants, nil
}

// ConfirmProvisionalGrants confirms the grants `ids` of the elevation, on tx
// (the review's transaction, holding LockRoleWrites as its first
// statement): each writes the standing assignment with reviewerID as the
// actor and the source `review`, and is marked confirmed. A grant that is
// not the elevation's, or was confirmed already, refuses all of them
// (ErrProvisionalGrantNotFound), and so does a grant given to the reviewer
// (ErrReviewerInvolved). Each confirmation that changes the grantee's
// standing set appends its admin.access.changed event (source `review`)
// through appender. It answers the number of grants confirmed.
//
// Only ReviewElevation calls it, after the outcome `justified` was checked.
func ConfirmProvisionalGrants(ctx context.Context, tx pgx.Tx, appender securityaudit.Appender, review ElevationReview, at time.Time) (int, error) {
	elevation, ids, reviewerID := review.ID, review.Confirm, review.ReviewerUserID
	if len(ids) == 0 {
		return 0, nil
	}
	ids = slices.Clone(ids)
	slices.SortFunc(ids, func(a, b uuid.UUID) int { return slices.Compare(a[:], b[:]) })
	ids = slices.Compact(ids)
	rows, err := tx.Query(ctx, `
SELECT `+provisionalGrantColumns+` FROM `+provisionalgrants.Table+` AS grant_row
WHERE grant_row.elevation_id = $1 AND grant_row.id = ANY($2::uuid[]) AND grant_row.confirmed_at IS NULL
ORDER BY grant_row.id
FOR UPDATE`, elevation, ids)
	if err != nil {
		return 0, fmt.Errorf("confirm the provisional grants: %w", err)
	}
	grants, err := pgx.CollectRows(rows, scanProvisionalGrant)
	if err != nil {
		return 0, fmt.Errorf("confirm the provisional grants: %w", err)
	}
	if len(grants) != len(ids) {
		return 0, ErrProvisionalGrantNotFound
	}
	for _, grant := range grants {
		// A reviewer never confirms what was given to them: that would make
		// their own standing set out of the elevation they review.
		if grant.UserID == reviewerID {
			return 0, fmt.Errorf("%w: grant %s was given to the reviewer", ErrReviewerInvolved, grant.ID)
		}
	}
	origin := rolegrants.Origin{ActorUserID: reviewerID, Source: rolegrants.SourceReview, Reason: "confirmed by the review of elevation " + elevation.String()}
	for _, grant := range grants {
		origin.SourceRef = grant.ID.String()
		var account bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM public.auth_core__user WHERE id = $1)`,
			grant.UserID).Scan(&account); err != nil {
			return 0, fmt.Errorf("confirm the provisional grants: read the account: %w", err)
		}
		if !account {
			return 0, fmt.Errorf("%w: the account of grant %s no longer exists", ErrElevationInvalid, grant.ID)
		}
		before, err := grantSubjectSet(ctx, tx, grant)
		if err != nil {
			return 0, err
		}
		switch grant.Kind {
		case ProvisionalKindRole:
			if _, err := rolegrants.Grant(ctx, tx, grant.UserID, ModeAdministration, grant.RoleName, origin); err != nil {
				return 0, fmt.Errorf("confirm the provisional role %s of user %d: %w", grant.RoleName, grant.UserID, err)
			}
		case ProvisionalKindMembership:
			var role int
			err := tx.QueryRow(ctx, `
SELECT project_role.id FROM public.auth_core__project_role AS project_role
WHERE project_role.project_id = $1 AND project_role.id = $2`, grant.ProjectID, grant.ProjectRoleID).Scan(&role)
			if errors.Is(err, pgx.ErrNoRows) {
				return 0, fmt.Errorf("%w: project %d no longer defines the role of grant %s",
					ErrElevationInvalid, grant.ProjectID, grant.ID)
			}
			if err != nil {
				return 0, fmt.Errorf("confirm the provisional membership: read the project role: %w", err)
			}
			if _, err := tx.Exec(ctx, `
INSERT INTO public.auth_core__project_user_role (project_id, user_id, role_id)
VALUES ($1, $2, $3)
ON CONFLICT (project_id, user_id, role_id) DO NOTHING`, grant.ProjectID, grant.UserID, role); err != nil {
				return 0, fmt.Errorf("confirm the provisional membership of user %d in project %d: %w",
					grant.UserID, grant.ProjectID, err)
			}
		}
		if _, err := tx.Exec(ctx, `
UPDATE `+provisionalgrants.Table+` SET confirmed_at = $2, confirmed_by = $3 WHERE id = $1`,
			grant.ID, at.UTC(), reviewerID); err != nil {
			return 0, fmt.Errorf("mark the provisional grant confirmed: %w", err)
		}
		if err := appendConfirmedGrant(ctx, tx, appender, review, grant, before); err != nil {
			return 0, err
		}
	}
	return len(grants), nil
}

// grantSubjectSet is the grantee's standing set the grant adds to: the
// administration roles of a role grant, the project roles of a membership.
func grantSubjectSet(ctx context.Context, q Querier, grant ProvisionalGrant) ([]string, error) {
	if grant.Kind == ProvisionalKindRole {
		return UserCentralRoles(ctx, q, int(grant.UserID), ModeAdministration)
	}
	rows, err := q.Query(ctx, `
SELECT DISTINCT role.name
FROM public.auth_core__project_user_role assignment
JOIN public.auth_core__project_role role ON role.id = assignment.role_id
WHERE assignment.project_id = $1 AND assignment.user_id = $2
ORDER BY 1`, grant.ProjectID, grant.UserID)
	if err != nil {
		return nil, fmt.Errorf("confirm the provisional membership: read the project roles: %w", err)
	}
	names, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, fmt.Errorf("confirm the provisional membership: read the project roles: %w", err)
	}
	return names, nil
}

// appendConfirmedGrant appends the admin.access.changed event of one
// confirmation, in tx, when it changed the grantee's standing set: the
// reviewer is the actor, the source is `review` and the elevation is the
// reviewed one. A membership event is partitioned by its project.
func appendConfirmedGrant(
	ctx context.Context, tx pgx.Tx, appender securityaudit.Appender, review ElevationReview,
	grant ProvisionalGrant, before []string,
) error {
	after, err := grantSubjectSet(ctx, tx, grant)
	if err != nil {
		return err
	}
	before, after = sortedNames(before), sortedNames(after)
	if slices.Equal(before, after) {
		return nil
	}
	change := securityaudit.AdminAccessChanged{
		ActorUserID:       review.ReviewerUserID,
		SubjectType:       "user",
		SubjectID:         strconv.FormatInt(grant.UserID, 10),
		AuthenticationRef: review.AuthenticationRef,
		CorrelationID:     review.CorrelationID,
		Change: securityaudit.AccessChange{
			Kind: securityaudit.AccessRoleAssignment, Mode: ModeAdministration,
			Source: rolegrants.SourceReview, Before: before, After: after,
			ElevationID: review.ID.String(),
		},
	}
	if grant.Kind == ProvisionalKindMembership {
		var member bool
		if err := tx.QueryRow(ctx, `
SELECT EXISTS (SELECT 1 FROM public.auth_core__project_user_role WHERE project_id = $1 AND user_id = $2)`,
			grant.ProjectID, review.ReviewerUserID).Scan(&member); err != nil {
			return fmt.Errorf("confirm the provisional membership: read the reviewer's membership: %w", err)
		}
		held, err := UserCentralRoles(ctx, tx, int(grant.UserID), ModeAdministration)
		if err != nil {
			return err
		}
		change.TenantProjectID = grant.ProjectID
		change.Change.Kind, change.Change.Mode = securityaudit.AccessProjectMembership, ""
		change.Change.Membership = &securityaudit.MembershipFacts{ActorIsMember: member, MemberHoldsAdminRole: len(held) > 0}
	}
	if err := appender.Append(ctx, tx, securityaudit.NewAdminAccessChanged(change)); err != nil {
		return fmt.Errorf("confirm the provisional grant: append the access event: %w", err)
	}
	return nil
}

// sortedNames is a sorted copy of names without duplicates or empty names,
// never nil.
func sortedNames(names []string) []string {
	out := make([]string, 0, len(names))
	for _, name := range names {
		if name != "" {
			out = append(out, name)
		}
	}
	slices.Sort(out)
	return slices.Compact(out)
}
