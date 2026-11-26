package accessevent

// The project_membership kind of admin.access.changed (ADR-0059 R1.5): a
// membership written through a central route. The bulk invite
// (internal/api/v2/admin) and the central member routes
// (internal/api/v2/eliteacore, issue 2994) both append it here, so the facts
// R1.6 rates it by are read one way.

import (
	"context"
	"fmt"
	"slices"
	"strconv"

	"github.com/jackc/pgx/v5"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/adminaccess"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/auth"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/securityaudit"
)

// ProjectMemberRoles is the person's roles in the project, by name, sorted.
func ProjectMemberRoles(ctx context.Context, q adminaccess.Querier, projectID, userID int64) ([]string, error) {
	rows, err := q.Query(ctx, `
SELECT DISTINCT role.name
FROM public.auth_core__project_user_role assignment
JOIN public.auth_core__project_role role ON role.id = assignment.role_id
WHERE assignment.project_id = $1 AND assignment.user_id = $2
ORDER BY 1`, projectID, userID)
	if err != nil {
		return nil, fmt.Errorf("read the project roles: %w", err)
	}
	names, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, fmt.Errorf("read the project roles: %w", err)
	}
	return names, nil
}

// SortedSet is a sorted copy of names without duplicates or empty names,
// never nil.
func SortedSet(names []string) []string {
	out := make([]string, 0, len(names))
	for _, name := range names {
		if name != "" {
			out = append(out, name)
		}
	}
	slices.Sort(out)
	return slices.Compact(out)
}

// AppendMembershipChange appends, in tx, the project_membership event of the
// membership of userID in projectID that a central route wrote, when the
// write changed the person's roles there: before is the set the write read
// in tx before it wrote, and the set after is read here. requestContext is
// the request's (the actor, the credential, the elevation, the trace).
//
// The facts: whether the actor is a member of the project, whether the
// member holds an administration role, and bulk, which the caller states
// (a bulk invite, or one request that writes more than one membership).
// The partition is the project's.
func AppendMembershipChange(
	ctx context.Context, tx pgx.Tx, appender securityaudit.Appender, requestContext context.Context,
	projectID, userID int64, before []string, bulk bool,
) error {
	if appender == nil {
		return fmt.Errorf("accessevent: no security audit appender")
	}
	after, err := ProjectMemberRoles(ctx, tx, projectID, userID)
	if err != nil {
		return err
	}
	before, after = SortedSet(before), SortedSet(after)
	if slices.Equal(before, after) {
		return nil
	}
	facts, err := membershipFacts(ctx, tx, requestContext, projectID, userID, bulk)
	if err != nil {
		return err
	}
	change := FromRequest(requestContext, securityaudit.AdminAccessChanged{
		SubjectType:     "user",
		SubjectID:       strconv.FormatInt(userID, 10),
		TenantProjectID: projectID,
		Change: securityaudit.AccessChange{
			Kind:       securityaudit.AccessProjectMembership,
			Before:     before,
			After:      after,
			Membership: &facts,
		},
	})
	return appender.Append(ctx, tx, securityaudit.NewAdminAccessChanged(change))
}

// membershipFacts are the facts of a membership of userID in projectID
// written by the request's actor, read in tx.
func membershipFacts(
	ctx context.Context, tx pgx.Tx, requestContext context.Context, projectID, userID int64, bulk bool,
) (securityaudit.MembershipFacts, error) {
	facts := securityaudit.MembershipFacts{Bulk: bulk}
	if principal, ok := auth.UserFromContext(requestContext); ok {
		if actor, resolved := principal.OwningUserID(); resolved && actor > 0 {
			var member bool
			if err := tx.QueryRow(ctx, `
SELECT EXISTS (SELECT 1 FROM public.auth_core__project_user_role
               WHERE project_id = $1 AND user_id = $2)`, projectID, actor).Scan(&member); err != nil {
				return facts, fmt.Errorf("read the actor's membership: %w", err)
			}
			facts.ActorIsMember = member
		}
	}
	held, err := adminaccess.UserCentralRoles(ctx, tx, int(userID), adminaccess.ModeAdministration)
	if err != nil {
		return facts, err
	}
	facts.MemberHoldsAdminRole = len(held) > 0
	return facts, nil
}
