-- The unqualified toolkit table is intentional. These queries execute only
-- inside an authorized project transaction whose local search_path is the
-- exact p_<project_id> tenant schema.

-- name: ListCurrentIndexScheduleProjects :many
SELECT project.id::integer
FROM centry.project AS project
WHERE project.create_success IS TRUE
  AND project.id > sqlc.arg('after_project_id')::integer
ORDER BY project.id
LIMIT sqlc.arg('page_limit')::integer;

-- name: ListCurrentIndexScheduleToolkits :many
SELECT toolkit.id,
       toolkit.type,
       jsonb_extract_path(toolkit.meta, 'indexes_meta') AS indexes_meta
FROM elitea_tools AS toolkit
WHERE toolkit.id > sqlc.arg('after_toolkit_id')::integer
  AND jsonb_typeof(toolkit.meta -> 'indexes_meta') = 'object'
ORDER BY toolkit.id
LIMIT sqlc.arg('page_limit')::integer;

-- name: LockCurrentIndexScheduleToolkit :one
SELECT settings, meta
FROM elitea_tools
WHERE id = sqlc.arg('toolkit_id')::integer
FOR UPDATE;

-- name: LockCurrentIndexScheduleToolkitMeta :one
SELECT meta
FROM elitea_tools
WHERE id = sqlc.arg('toolkit_id')::integer
FOR UPDATE;

-- name: UpdateCurrentIndexScheduleToolkitMeta :execrows
UPDATE elitea_tools
SET meta = sqlc.arg('meta')::jsonb,
    updated_at = (clock_timestamp() AT TIME ZONE 'UTC')
WHERE id = sqlc.arg('toolkit_id')::integer;

-- name: InsertCurrentIndexScheduleNotification :execrows
INSERT INTO centry.notifications (
    uuid,
    is_seen,
    project_id,
    user_id,
    meta,
    event_type,
    created_at,
    updated_at
) VALUES (
    sqlc.arg('notification_uuid')::text::uuid,
    FALSE,
    sqlc.arg('project_id')::integer,
    sqlc.arg('user_id')::integer,
    sqlc.arg('meta')::jsonb,
    'index_data_changed',
    now() AT TIME ZONE 'UTC',
    now() AT TIME ZONE 'UTC'
)
ON CONFLICT (uuid) DO NOTHING;
