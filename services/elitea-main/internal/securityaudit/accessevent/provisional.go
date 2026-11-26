package accessevent

// The events of a provisional grant (ADR-0059 decision 5; stories R2.12a,
// issue 2526, and R2.12b, issue 2606). What an elevated person gives to
// someone else changes no standing set, so the standing events append
// nothing for it. These name the gift instead: the kind is the standing
// kind (role_assignment, project_membership), the source is `provisional`,
// before is the grantee's standing set and after is that set with what was
// given provisionally. The elevation is the one the grant is bound to
// (adminaccess.ProvisionalElevationID).

import (
	"context"
	"fmt"
	"slices"
	"strconv"

	"github.com/jackc/pgx/v5"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/adminaccess"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/securityaudit"
)

// SourceProvisional is the source of the event of a provisional grant.
const SourceProvisional = "provisional"

// AppendProvisionalRoles appends, in tx, the role_assignment event of the
// administration roles `given` that the elevated caller gave userID
// provisionally. It appends nothing when the person already holds them all
// standing.
func AppendProvisionalRoles(
	ctx context.Context, tx pgx.Tx, appender securityaudit.Appender, requestContext context.Context,
	caller adminaccess.Caller, userID int, given []string,
) error {
	if len(given) == 0 {
		return nil
	}
	if appender == nil {
		return fmt.Errorf("accessevent: no security audit appender")
	}
	before, err := adminaccess.UserCentralRoles(ctx, tx, userID, adminaccess.ModeAdministration)
	if err != nil {
		return err
	}
	before = SortedSet(before)
	after := SortedSet(append(slices.Clone(before), given...))
	if slices.Equal(before, after) {
		return nil
	}
	change := FromRequest(requestContext, securityaudit.AdminAccessChanged{
		ActorUserID: caller.UserID,
		SubjectType: "user",
		SubjectID:   strconv.Itoa(userID),
		Change: securityaudit.AccessChange{
			Kind: securityaudit.AccessRoleAssignment, Mode: adminaccess.ModeAdministration,
			Source: SourceProvisional, Before: before, After: after,
			ElevationID: adminaccess.ProvisionalElevationID(caller),
		},
	})
	return appender.Append(ctx, tx, securityaudit.NewAdminAccessChanged(change))
}

// AppendProvisionalMembership appends, in tx, the project_membership event
// of the project roles roleIDs that the elevated caller gave userID in
// projectID provisionally, with the facts AppendMembershipChange states. It
// appends nothing when the person already holds them all standing.
func AppendProvisionalMembership(
	ctx context.Context, tx pgx.Tx, appender securityaudit.Appender, requestContext context.Context,
	caller adminaccess.Caller, projectID, userID int64, roleIDs []int, bulk bool,
) error {
	if len(roleIDs) == 0 {
		return nil
	}
	if appender == nil {
		return fmt.Errorf("accessevent: no security audit appender")
	}
	before, err := ProjectMemberRoles(ctx, tx, projectID, userID)
	if err != nil {
		return err
	}
	rows, err := tx.Query(ctx, `
SELECT name FROM public.auth_core__project_role WHERE project_id = $1 AND id = ANY($2::int[])`, projectID, roleIDs)
	if err != nil {
		return fmt.Errorf("read the project roles given: %w", err)
	}
	given, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return fmt.Errorf("read the project roles given: %w", err)
	}
	before = SortedSet(before)
	after := SortedSet(append(slices.Clone(before), given...))
	if slices.Equal(before, after) {
		return nil
	}
	facts, err := membershipFacts(ctx, tx, requestContext, projectID, userID, bulk)
	if err != nil {
		return err
	}
	change := FromRequest(requestContext, securityaudit.AdminAccessChanged{
		ActorUserID:     caller.UserID,
		SubjectType:     "user",
		SubjectID:       strconv.FormatInt(userID, 10),
		TenantProjectID: projectID,
		Change: securityaudit.AccessChange{
			Kind:        securityaudit.AccessProjectMembership,
			Source:      SourceProvisional,
			Before:      before,
			After:       after,
			ElevationID: adminaccess.ProvisionalElevationID(caller),
			Membership:  &facts,
		},
	})
	return appender.Append(ctx, tx, securityaudit.NewAdminAccessChanged(change))
}
