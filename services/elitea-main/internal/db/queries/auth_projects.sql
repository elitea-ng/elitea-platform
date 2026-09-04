-- This is the exact current projects_get_personal_project_id decision tree:
-- a named personal project wins only when the user has any project-role
-- assignment; the system-user email fallback is considered only when that
-- named project does not exist.
-- name: ResolveCurrentPersonalProjectID :one
WITH personal_project AS MATERIALIZED (
    SELECT project.id
    FROM centry.project AS project
    WHERE project.name = ('project_user_' || sqlc.arg('user_id')::integer::text)
    ORDER BY project.id
    LIMIT 1
), member_personal_project AS MATERIALIZED (
    SELECT project.id
    FROM personal_project AS project
    WHERE EXISTS (
        SELECT 1
        FROM public.auth_core__project_user_role AS assignment
        WHERE assignment.project_id = project.id
          AND assignment.user_id = sqlc.arg('user_id')::integer
    )
), resolved_project AS (
    SELECT project.id AS project_id
    FROM member_personal_project AS project

    UNION ALL

    SELECT substring(
               user_account.email
               FROM '^system_user_([0-9]+)@centry[.]user$'
           )::integer AS project_id
    FROM public.auth_core__user AS user_account
    WHERE user_account.id = sqlc.arg('user_id')::integer
      AND user_account.email ~ '^system_user_[0-9]+@centry[.]user$'
      AND NOT EXISTS (SELECT 1 FROM personal_project)
)
SELECT project.project_id::integer AS project_id
FROM resolved_project AS project
LIMIT 1;

-- name: IsCurrentUserProjectMember :one
SELECT EXISTS (
    SELECT 1
    FROM public.auth_core__project_user_role AS assignment
    JOIN centry.project AS project ON project.id = assignment.project_id
    WHERE assignment.user_id = sqlc.arg('user_id')::integer
      AND assignment.project_id = sqlc.arg('project_id')::integer
      AND project.create_success IS TRUE
      AND project.suspended IS FALSE
) AS is_member;

-- Exact current-platform containment for tracing credentials. Project role
-- identifiers are compared exactly: billing-admin does
-- not carry administrator authority. The personal-project signal is
-- independent of role assignment so a missing role row cannot revoke an
-- owner's access to project_user_<userID>.
-- name: CanManageCurrentTracingConfiguration :one
SELECT CASE WHEN
    EXISTS (
        SELECT 1
        FROM public.auth_core__project_user_role AS assignment
        JOIN public.auth_core__project_role AS project_role
          ON project_role.id = assignment.role_id
         AND project_role.project_id = assignment.project_id
        WHERE assignment.project_id = sqlc.arg('project_id')::bigint
          AND assignment.user_id = sqlc.arg('user_id')::bigint
          AND project_role.name = ANY(ARRAY['admin', 'super_admin', 'system']::text[])
    )
    OR EXISTS (
        SELECT 1
        FROM centry.project AS project
        WHERE project.id = sqlc.arg('project_id')::bigint
          AND project.name = 'project_user_' || sqlc.arg('user_id')::bigint::text
    )
THEN TRUE ELSE FALSE END AS can_manage;

-- name: ListCurrentUserProjects :many
WITH candidate_projects AS MATERIALIZED (
    SELECT
        project.id,
        project.name,
        project.owner_id,
        project.plugins,
        project.keycloak_groups,
        project.create_success,
        project.suspended
    FROM centry.project AS project
    WHERE (
        sqlc.narg('search')::text IS NULL
        OR project.name ILIKE ('%' || sqlc.narg('search')::text || '%')
    )
    ORDER BY project.id
    LIMIT sqlc.narg('limit')::integer
    OFFSET sqlc.narg('offset')::integer
), member_projects AS MATERIALIZED (
    SELECT DISTINCT assignment.project_id
    FROM public.auth_core__project_user_role AS assignment
    WHERE assignment.user_id = sqlc.arg('user_id')::integer
)
SELECT
    project.id,
    project.name,
    project.owner_id,
    project.plugins,
    project.keycloak_groups,
    project.create_success,
    project.suspended,
    project_group.id AS group_id,
    project_group.name AS group_name
FROM candidate_projects AS project
JOIN member_projects AS membership ON membership.project_id = project.id
LEFT JOIN centry.project_group_association AS association
    ON association.project_id = project.id
LEFT JOIN centry.project_group AS project_group
    ON project_group.id = association.group_id
WHERE (
    NOT sqlc.arg('check_public_role')::boolean
    OR project.id <> sqlc.arg('public_project_id')::integer
    OR EXISTS (
        SELECT 1
        FROM public.auth_core__project_user_role AS assignment
        JOIN public.auth_core__project_role AS project_role
            ON project_role.id = assignment.role_id
            AND project_role.project_id = assignment.project_id
        WHERE assignment.project_id = project.id
          AND assignment.user_id = sqlc.arg('user_id')::integer
          AND project_role.name = 'admin'
    )
)
ORDER BY project.id, project_group.id;
