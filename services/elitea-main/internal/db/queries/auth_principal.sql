-- The project binding is reloaded with the principal, exactly as
-- GetActivePATPrincipalByUUID reads it, so a token re-validated by row ID keeps
-- the binding its bearer form carries.
-- name: GetActivePATPrincipalByID :one
SELECT
    token.id AS token_id,
    owner.id AS user_id,
    COALESCE(owner.email, '')::text AS email,
    binding.project_id,
    (bound_project.suspended IS FALSE
        AND bound_project.create_success IS TRUE)::boolean AS bound_project_active
FROM public.auth_core__token AS token
JOIN public.auth_core__user AS owner ON owner.id = token.user_id
LEFT JOIN elitea_identity.token_project_binding AS binding
       ON binding.token_id = token.id
LEFT JOIN centry.project AS bound_project
       ON bound_project.id = binding.project_id
WHERE token.id = sqlc.arg(token_id)::integer
  AND owner.suspended = false
  AND (token.expires IS NULL OR token.expires > (clock_timestamp() AT TIME ZONE 'UTC'));

-- name: GetActiveUserPrincipalByID :one
SELECT
    owner.id AS user_id,
    COALESCE(owner.email, '')::text AS email
FROM public.auth_core__user AS owner
WHERE owner.id = sqlc.arg(user_id)::integer
  AND owner.suspended = false;
