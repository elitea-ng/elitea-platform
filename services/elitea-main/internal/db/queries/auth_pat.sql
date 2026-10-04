-- The LEFT JOIN onto elitea_identity.token_project_binding is what lets a user
-- see which project a key bills (ADR-0018, spec-llm-project-scope §4). It is a
-- LEFT JOIN because an unbound token is the default and must still be listed.
-- A row with uuid NULL is not a personal access token: no PAT JWT can name it.
-- Native device sessions (ADR-0025, shared 0141) anchor on exactly such rows,
-- so the filter keeps a signed-in phone from showing up in Settings > Tokens
-- as a key named `native:<client>`. The device registry lists those instead.
-- name: ListOwnedPATs :many
SELECT
    token.id,
    token.uuid,
    token.expires,
    COALESCE(token.user_id, 0)::integer AS user_id,
    token.name,
    binding.project_id
FROM public.auth_core__token AS token
LEFT JOIN elitea_identity.token_project_binding AS binding
       ON binding.token_id = token.id
WHERE token.user_id = sqlc.arg(user_id)::integer
  AND token.uuid IS NOT NULL
ORDER BY token.id;

-- name: GetOwnedPAT :one
SELECT
    token.id,
    token.uuid,
    token.expires,
    COALESCE(token.user_id, 0)::integer AS user_id,
    token.name,
    binding.project_id
FROM public.auth_core__token AS token
LEFT JOIN elitea_identity.token_project_binding AS binding
       ON binding.token_id = token.id
WHERE token.uuid = sqlc.arg(uuid)::text
  AND token.uuid IS NOT NULL
  AND token.user_id = sqlc.arg(user_id)::integer;

-- name: CreatePATForActiveUser :one
INSERT INTO public.auth_core__token (uuid, expires, user_id, name)
SELECT
    sqlc.arg(uuid)::varchar(36),
    sqlc.narg(expires)::timestamp without time zone,
    owner.id,
    sqlc.narg(name)::text
FROM public.auth_core__user AS owner
WHERE owner.id = sqlc.arg(user_id)::integer
  AND owner.suspended = false
RETURNING
    id,
    uuid,
    expires,
    COALESCE(user_id, 0)::integer AS user_id,
    name;

-- A binding is written once, in the same transaction as the token INSERT, and
-- after the membership check. There is no update query on purpose: a binding is
-- a fact about a key, not a setting that changes under a running integration
-- (spec-llm-project-scope §4).
-- name: CreateTokenProjectBinding :exec
INSERT INTO elitea_identity.token_project_binding (token_id, project_id)
VALUES (sqlc.arg(token_id)::integer, sqlc.arg(project_id)::integer);

-- The list and get responses read the binding back through the LEFT JOIN
-- above, so no point-read query exists. A separate read would be a second round
-- trip for a value the same row already carries.

-- Token deletion deletes the binding explicitly, in the same transaction, and
-- does NOT rely on ON DELETE CASCADE (spec-llm-project-scope §3.1). Migration
-- 0071 guards its foreign key with to_regclass, because elitea-migrate can run
-- before pylon creates auth_core. When the guard skips, the migration is still
-- ledgered as applied and no later run adds the constraint, so that database
-- has no cascade for its whole life. The constraint stays as the second of two
-- independent guarantees; this query is the first.
-- name: DeleteTokenProjectBinding :exec
DELETE FROM elitea_identity.token_project_binding
WHERE token_id = sqlc.arg(token_id)::integer;

-- name: LockPATByUUID :one
SELECT
    token.id,
    COALESCE(token.user_id, 0)::integer AS user_id
FROM public.auth_core__token AS token
WHERE token.uuid = sqlc.arg(uuid)::text
FOR UPDATE;

-- name: DeletePATByID :execrows
DELETE FROM public.auth_core__token
WHERE id = sqlc.arg(id)::integer;

-- This is the single query the credential validator runs for every request.
-- The token binding rides along on the row the validator already reads, so a
-- bound token costs no additional round trip on the request path
-- (spec-llm-project-scope §3.2). Do not split it into a second lookup.
--
-- bound_project_active carries the lifecycle state of the bound project on the
-- same row, for the same reason. Suspension is a reversible boolean on
-- centry.project and revokes no binding, so a bound token kept spending a
-- suspended project's budget and credentials. The middleware re-checks the
-- state here instead (spec-llm-project-scope §7 invariant 3).
--
-- BOTH joins must stay LEFT joins. Most personal access tokens carry no
-- binding, and an inner join on centry.project would drop every one of those
-- rows. That breaks authentication for the majority of callers.
-- name: GetActivePATPrincipalByUUID :one
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
WHERE token.uuid = sqlc.arg(uuid)::text
  AND owner.suspended = false
  AND (token.expires IS NULL OR token.expires > (clock_timestamp() AT TIME ZONE 'UTC'));

-- `token.uuid IS NOT NULL` is load-bearing twice over: the durable-execution
-- actor issuer signs the selected row as a PAT JWT, and a native device anchor
-- (ADR-0025, uuid NULL) must never be signed into one.
-- name: GetActivePATForUser :one
SELECT
    token.id AS token_id,
    token.uuid,
    token.expires,
    owner.id AS user_id,
    COALESCE(owner.email, '')::text AS email
FROM public.auth_core__token AS token
JOIN public.auth_core__user AS owner ON owner.id = token.user_id
WHERE owner.id = sqlc.arg(user_id)::integer
  AND owner.suspended = false
  AND token.uuid IS NOT NULL
  AND (token.expires IS NULL OR token.expires > (clock_timestamp() AT TIME ZONE 'UTC'))
ORDER BY token.id
LIMIT 1;

-- name: GetActiveProjectSystemPAT :one
SELECT
    project.id AS project_id,
    owner.id AS user_id,
    token.id AS token_id,
    token.uuid,
    token.expires,
    COALESCE(owner.email, '')::text AS email
FROM centry.project AS project
JOIN public.auth_core__user AS owner
  ON owner.email = ('system_user_' || project.id::text || '@centry.user')
JOIN public.auth_core__project_user_role AS assignment
  ON assignment.project_id = project.id
 AND assignment.user_id = owner.id
JOIN public.auth_core__project_role AS project_role
  ON project_role.id = assignment.role_id
 AND project_role.project_id = project.id
JOIN public.auth_core__token AS token
  ON token.user_id = owner.id
WHERE project.id = sqlc.arg(project_id)::integer
  AND project.create_success = true
  AND project.suspended = false
  AND owner.suspended = false
  AND token.name = 'api'
  AND token.uuid IS NOT NULL
  AND (token.expires IS NULL OR token.expires > (clock_timestamp() AT TIME ZONE 'UTC'))
ORDER BY token.id
LIMIT 1;
