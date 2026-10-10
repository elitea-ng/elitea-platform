package middleware

import (
	"context"
	"errors"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/personalproject"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth"
)

// rowQuerier is the minimal subset of *pgxpool.Pool the resolver needs. It lets
// tests substitute a fake without a live database.
type rowQuerier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// DBPersonalProjectResolver resolves a user's personal project id from Postgres,
// mirroring pylon's projects_get_personal_project_id RPC:
//
//	project = SELECT id FROM centry.project WHERE name = 'project_user_<uid>'
//	if project exists AND user is a member of it → return project.id
//	if project missing → fall back to the system-user email pattern
//	                     'system_user_<n>@centry.user' → return <n>
type DBPersonalProjectResolver struct {
	pool rowQuerier
	// ensurer is optional; a nil one makes this resolver read-only, which is
	// what it always was.
	//
	// It is here as well as on `/social/author` because the two are the only
	// readers of "the caller's personal project", and only one of them is on
	// the SPA's path. A PAT-only caller — the SDK, a scheduled job, a scripted
	// client — never requests `/social/author`, so hooking that endpoint alone
	// left those callers with no personal project and `/llm` answering
	// `project_not_resolved` forever. pylon triggers the same work for every
	// authenticated request.
	ensurer personalproject.AsyncEnsurer
}

// NewDBPersonalProjectResolver builds a resolver backed by the given pool.
func NewDBPersonalProjectResolver(pool *pgxpool.Pool) *DBPersonalProjectResolver {
	return &DBPersonalProjectResolver{pool: pool}
}

// WithPersonalProjectEnsurer returns a resolver that also ASKS for the personal
// project it could not find. See PersonalProjectEnsurer for why this reader
// needs it and not only `/social/author`.
func (r *DBPersonalProjectResolver) WithPersonalProjectEnsurer(
	ensurer personalproject.AsyncEnsurer,
) *DBPersonalProjectResolver {
	if r == nil {
		return nil
	}
	resolved := *r
	resolved.ensurer = ensurer
	return &resolved
}

// ensure asks for the caller's personal project in the background. It never
// blocks and never changes this call's answer: provisioning applies a tenant
// migration corpus, so the caller that triggered it is told "no personal
// project" and a later call gets the id.
//
// THE ID COMES FROM `auth.User.OwningUserID`, not from a parse of the string
// this resolver was handed. That accessor is this repository's answer to "which
// auth_core__user owns this principal": it prefers the validated `UserID` field
// and refuses a principal whose id is a TOKEN id. `/social/author` applies it
// before asking for the same work, and the two readers must not judge the same
// principal by different rules — `project_user_<token id>` would name a
// project belonging to whichever account happened to share that number.
//
// The resolver is asked about a plain string, so the two are reconciled rather
// than assumed equal: nothing is provisioned unless the id under resolution IS
// the caller's owning user.
func (r *DBPersonalProjectResolver) ensure(ctx context.Context, userID string) {
	if r.ensurer == nil {
		return
	}
	user, ok := auth.UserFromContext(ctx)
	if !ok {
		return
	}
	owning, ok := user.OwningUserID()
	if !ok || owning <= 0 {
		return
	}
	if strconv.FormatInt(owning, 10) != strings.TrimSpace(userID) {
		return
	}
	r.ensurer.EnsureAsync(owning)
}

// PersonalProjectID implements PersonalProjectResolver.
func (r *DBPersonalProjectResolver) PersonalProjectID(ctx context.Context, userID string) (int, error) {
	if userID == "" {
		return 0, nil
	}
	if r.pool == nil {
		return 0, errors.New("project resolver: nil pool")
	}

	// Built from the prefix the package that WRITES this name exports, not from
	// a `project_user_` literal repeated here: writer and readers have to
	// agree, and a rename that updated one of them would make resolution answer
	// "" for every account with no test failing.
	projectName := personalproject.NamePrefix + userID

	var projectID int
	err := r.pool.QueryRow(ctx,
		`SELECT id FROM centry.project WHERE name = $1 LIMIT 1`, projectName,
	).Scan(&projectID)

	switch {
	case err == nil:
		// The named personal project exists; confirm the caller is a member of
		// it before trusting it (mirrors admin_check_user_in_project).
		member, mErr := r.userInProject(ctx, projectID, userID)
		if mErr != nil {
			return 0, mErr
		}
		if member {
			return projectID, nil
		}
		// Named project exists but the user is not a member: fall through to the
		// email-pattern fallback below.
		return r.resolveWithoutNamedProject(ctx, userID)
	case errors.Is(err, pgx.ErrNoRows):
		return r.resolveWithoutNamedProject(ctx, userID)
	default:
		return 0, err
	}
}

// resolveWithoutNamedProject is the answer when no personal project this caller
// is a member of exists — and the point at which one is asked for.
//
// The system-user fallback runs FIRST and unchanged: an identity that resolves
// through it already has a project and must not be given a second one, which is
// the same exclusion the ensurer applies by email.
func (r *DBPersonalProjectResolver) resolveWithoutNamedProject(
	ctx context.Context, userID string,
) (int, error) {
	projectID, err := r.projectIDFromSystemEmail(ctx, userID)
	if err != nil || projectID != 0 {
		return projectID, err
	}
	r.ensure(ctx, userID)
	return 0, nil
}

// userInProject reports whether userID has any role in projectID.
func (r *DBPersonalProjectResolver) userInProject(ctx context.Context, projectID int, userID string) (bool, error) {
	uid, err := strconv.Atoi(userID)
	if err != nil {
		return false, nil
	}
	var exists bool
	err = r.pool.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM auth_core__project_user_role
			WHERE project_id = $1 AND user_id = $2
		)`, projectID, uid).Scan(&exists)
	if err != nil {
		return false, err
	}
	return exists, nil
}

// projectIDFromSystemEmail implements pylon's fallback: when there is no named
// personal project, a system user whose email matches
// 'system_user_<n>@centry.user' resolves to project id <n>.
func (r *DBPersonalProjectResolver) projectIDFromSystemEmail(ctx context.Context, userID string) (int, error) {
	uid, err := strconv.Atoi(userID)
	if err != nil {
		return 0, nil
	}
	var email string
	err = r.pool.QueryRow(ctx,
		`SELECT COALESCE(email, '') FROM auth_core__user WHERE id = $1`, uid,
	).Scan(&email)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return 0, nil
		}
		return 0, err
	}
	if m := systemUserEmailRe.FindStringSubmatch(email); m != nil {
		if id, convErr := strconv.Atoi(m[1]); convErr == nil {
			return id, nil
		}
	}
	return 0, nil
}
