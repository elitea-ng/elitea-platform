package legacyrbac

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth"
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
}

var (
	_ auth.PermissionResolver           = (*PostgresResolver)(nil)
	_ auth.MembershipPermissionResolver = (*PostgresResolver)(nil)
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

	var permissions []string
	switch mode {
	case auth.PermissionModeAdministration, auth.PermissionModeDeveloper:
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

	var permissions []string
	switch mode {
	case auth.PermissionModeAdministration, auth.PermissionModeDeveloper:
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
ORDER BY permission`, projectID, userID)
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
ORDER BY permission`, userID)
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
