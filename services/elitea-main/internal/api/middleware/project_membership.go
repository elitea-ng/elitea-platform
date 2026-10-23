package middleware

import (
	"context"
	"math"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/db/sqlcgen"
)

// ProjectMembershipChecker answers whether a user may act inside a project.
// The /llm edge uses it to admit a caller-supplied project selector before that
// project reaches the signed identity and becomes the gateway's budget scope.
// userID is the owning auth_core__user id, from auth.User.OwningUserID. A
// token id is never accepted here.
type ProjectMembershipChecker interface {
	IsProjectMember(ctx context.Context, userID int64, projectID int) (bool, error)
}

// projectMemberQuerier is the single query this checker runs. A test supplies
// the membership answer through it, so the admission decision runs without a
// live database.
type projectMemberQuerier interface {
	IsCurrentUserProjectMember(context.Context, sqlcgen.IsCurrentUserProjectMemberParams) (bool, error)
}

// ProjectMemberQuerier exposes that interface to tests in other packages.
type ProjectMemberQuerier = projectMemberQuerier

// projectMembership is the database-backed ProjectMembershipChecker.
//
// It runs IsCurrentUserProjectMember, which is the strictest membership
// predicate in the service: the user must hold a role in the project, and the
// project itself must be provisioned (create_success) and not suspended.
//
// RequireProjectAccess in project_authorization.go asks a wider question — it
// also admits a central super_admin, and it ignores the project lifecycle
// columns. The two stay separate on purpose. This selector decides where money
// is spent, so it grants the narrower right: an unprovisioned or suspended
// project must never collect spend, and platform administration is not by
// itself a licence to bill an arbitrary project.
type projectMembership struct {
	queries projectMemberQuerier
}

// NewProjectMembership returns a ProjectMembershipChecker backed by pool.
//
// It returns an untyped nil interface for a nil pool. A nil *pgxpool.Pool boxed
// into an interface compares non-nil, so a caller could not otherwise see that
// no checker is configured. The /llm edge fails closed on a nil checker.
func NewProjectMembership(pool *pgxpool.Pool) ProjectMembershipChecker {
	if pool == nil {
		return nil
	}
	return projectMembership{queries: sqlcgen.New(pool)}
}

// NewProjectMembershipWith is NewProjectMembership over an injected querier.
func NewProjectMembershipWith(queries ProjectMemberQuerier) ProjectMembershipChecker {
	if queries == nil {
		return nil
	}
	return projectMembership{queries: queries}
}

// IsProjectMember reports whether userID may act inside projectID.
//
// Both ids are int4 columns. A value that does not fit is not a member. That is
// a refusal and not a failure: the function returns false with a nil error, so
// the edge ignores the selector and bills the caller's own project. It must not
// look like an outage, because the edge treats an outage the same way (ADR-0018,
// spec-llm-project-scope §5 rows 5 and 6) and a caller learns nothing either
// way.
func (m projectMembership) IsProjectMember(ctx context.Context, userID int64, projectID int) (bool, error) {
	if userID <= 0 || userID > math.MaxInt32 {
		return false, nil
	}
	if projectID <= 0 || projectID > math.MaxInt32 {
		return false, nil
	}
	return m.queries.IsCurrentUserProjectMember(ctx, sqlcgen.IsCurrentUserProjectMemberParams{
		UserID:    int32(userID),
		ProjectID: int32(projectID),
	})
}
