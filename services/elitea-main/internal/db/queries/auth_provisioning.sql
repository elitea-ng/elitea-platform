-- name: AcquireAuthProviderAdvisoryLock :exec
SELECT pg_advisory_xact_lock(
    hashtextextended(sqlc.arg(provider_ref)::text, 0)
);

-- name: GetAuthUserByProviderForProvisioning :one
SELECT
    owner.id,
    owner.email,
    owner.name,
    owner.last_login,
    owner.suspended
FROM public.auth_core__user_provider AS provider
JOIN public.auth_core__user AS owner ON owner.id = provider.user_id
WHERE provider.provider_ref = sqlc.arg(provider_ref)::text
FOR UPDATE OF provider, owner;

-- name: GetAuthUserByEmailForProvisioning :one
SELECT id, email, name, last_login, suspended
FROM public.auth_core__user
WHERE email = sqlc.arg(email)::text
FOR UPDATE;

-- name: CreateAuthUserByEmailIfMissing :one
INSERT INTO public.auth_core__user (email, name)
VALUES (sqlc.arg(email)::text, sqlc.arg(name)::text)
ON CONFLICT (email) DO NOTHING
RETURNING id, email, name, last_login, suspended;

-- name: LinkAuthProviderIfMissing :execrows
INSERT INTO public.auth_core__user_provider (user_id, provider_ref)
VALUES (sqlc.arg(user_id)::integer, sqlc.arg(provider_ref)::text)
ON CONFLICT (provider_ref) DO NOTHING;

-- name: TouchProvisionedAuthUser :one
UPDATE public.auth_core__user
SET
    last_login = clock_timestamp() AT TIME ZONE 'UTC',
    name = CASE
        WHEN name IS NULL OR name = '' THEN sqlc.arg(name)::text
        ELSE name
    END
WHERE id = sqlc.arg(user_id)::integer
  AND suspended = false
RETURNING id, email, name, last_login, suspended;

-- name: CountAuthUserRolesInMode :one
SELECT count(*)
FROM public.auth_core__user_role AS user_role
JOIN public.auth_core__role AS role ON role.id = user_role.role_id
WHERE user_role.user_id = sqlc.arg(user_id)::integer
  AND role.mode = sqlc.arg(mode)::varchar(64);

-- name: AssignAuthUserRoleByNameAndMode :execrows
INSERT INTO public.auth_core__user_role (user_id, role_id)
SELECT sqlc.arg(user_id)::integer, role.id
FROM public.auth_core__role AS role
WHERE role.name = sqlc.arg(role_name)::varchar(64)
  AND role.mode = sqlc.arg(mode)::varchar(64)
ON CONFLICT (user_id, role_id) DO NOTHING;

-- name: HasAuthAdministrationAdminRole :one
SELECT EXISTS (
    SELECT 1
    FROM public.auth_core__user_role AS user_role
    JOIN public.auth_core__role AS role ON role.id = user_role.role_id
    WHERE user_role.user_id = sqlc.arg(user_id)::integer
      AND role.mode = 'administration'
      AND role.name IN ('admin', 'super_admin')
)::boolean;

-- name: AssignExistingProjectRoles :execrows
INSERT INTO public.auth_core__project_user_role (project_id, user_id, role_id)
SELECT
    sqlc.arg(project_id)::integer,
    sqlc.arg(user_id)::integer,
    role.id
FROM public.auth_core__project_role AS role
WHERE role.project_id = sqlc.arg(project_id)::integer
  AND role.name = ANY(sqlc.arg(role_names)::text[])
ON CONFLICT (project_id, user_id, role_id) DO NOTHING;
