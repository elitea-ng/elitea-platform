-- The unqualified table name is intentional. This query runs only inside an
-- authorized project transaction whose local search_path is p_<project_id>.

-- name: GetCurrentToolkit :one
SELECT id,
       created_at,
       updated_at,
       type,
       name,
       description,
       settings,
       author_id,
       shared_owner_id,
       shared_id,
       meta
FROM elitea_tools
WHERE id = sqlc.arg('toolkit_id')::integer
LIMIT 1;

-- The social access feature is optional. Return 0 when its override table is
-- absent, including the older organization-only folder schema. Return 1 when
-- the complete access projection exists. Return -1 when overrides exist
-- without their dependencies, which the repository refuses.
-- name: CurrentFolderAccessState :one
SELECT CASE
    WHEN to_regclass('entity_folders') IS NOT NULL
     AND to_regclass('social_folder_items') IS NOT NULL
     AND to_regclass('folder_access_overrides') IS NOT NULL THEN 1
    WHEN to_regclass('folder_access_overrides') IS NULL THEN 0
    ELSE -1
END::integer AS state;

-- Current-platform folder access is a restrictive overlay on project RBAC.
-- A toolkit outside a folder, or inside a folder without an override, remains
-- visible. A no_access override hides both toolkit and MCP folder item kinds.
-- name: GetCurrentMCPToolkitVisibleToActor :one
SELECT toolkit.id,
       toolkit.created_at,
       toolkit.updated_at,
       toolkit.type,
       toolkit.name,
       toolkit.description,
       toolkit.settings,
       toolkit.author_id,
       toolkit.shared_owner_id,
       toolkit.shared_id,
       toolkit.meta
FROM elitea_tools AS toolkit
WHERE toolkit.id = sqlc.arg('toolkit_id')::integer
  AND NOT EXISTS (
      SELECT 1
      FROM social_folder_items AS item
      JOIN folder_access_overrides AS access
        ON access.folder_id = item.folder_id
      WHERE item.entity IN ('toolkit', 'mcp')
        AND item.entity_id = toolkit.id
        AND access.user_id = sqlc.arg('actor_id')::integer
        AND access.access_level = 'no_access'
  )
LIMIT 1;
