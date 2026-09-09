-- These unqualified names are intentional. Every query is executed inside a
-- transaction whose local search_path is derived from the authorized project.
-- This file projects the existing 16-column tenant table; it does not define a
-- replacement configuration store.

-- name: FindCurrentConfigurationByEliteaTitle :one
SELECT uuid::text AS configuration_uuid,
       project_id,
       type,
       data,
       shared
FROM configuration
WHERE project_id = sqlc.arg('project_id')::integer
  AND elitea_title = sqlc.arg('elitea_title')::text
  AND (NOT sqlc.arg('shared_only')::boolean OR shared = true)
LIMIT 1;

-- The current Configurations -> LiteLLM model projection keys embedding
-- models by data.name. A two-row sentinel lets the adapter reject duplicate
-- mutable definitions instead of selecting one silently.
-- name: FindCurrentEmbeddingConfigurations :many
SELECT uuid::text AS configuration_uuid,
       project_id,
       type,
       section,
       data,
       shared
FROM configuration
WHERE project_id = sqlc.arg('project_id')::integer
  AND type = 'embedding_model'
  AND section = 'embedding'
  AND status_ok = true
  AND data->>'name' = sqlc.arg('model_name')::text
  AND (NOT sqlc.arg('shared_only')::boolean OR shared = true)
ORDER BY id ASC
LIMIT 2;

-- name: GetCurrentConfiguration :one
SELECT configuration.id,
       configuration.uuid::text AS configuration_uuid,
       configuration.project_id,
       configuration.label,
       configuration.elitea_title,
       configuration.type,
       configuration.section,
       configuration.data,
       configuration.meta,
       configuration.shared,
       configuration.status_ok,
       configuration.status_logs,
       configuration.source,
       configuration.author_id,
       configuration.created_at,
       configuration.updated_at,
       EXISTS (
           SELECT 1
           FROM centry.social_pins
           WHERE social_pins.entity = 'configuration'
             AND social_pins.project_id = sqlc.arg('project_id')::integer
             AND social_pins.entity_id = configuration.id
       ) AS is_pinned
FROM configuration
WHERE configuration.id = sqlc.arg('configuration_id')::integer
  AND configuration.project_id = sqlc.arg('project_id')::integer
LIMIT 1;

-- Empty section intentionally disables the section predicate for the exact
-- current UI contract. The caller caps limit_rows at the 257-row sentinel.
-- name: ListCurrentConfigurationTypes :many
SELECT DISTINCT type
FROM configuration
WHERE project_id = sqlc.arg('project_id')::integer
  AND (sqlc.arg('section')::text = '' OR section = sqlc.arg('section')::text)
ORDER BY type ASC
LIMIT sqlc.arg('limit_rows')::integer;

-- This deliberately projects only the six fields exposed by nested
-- configuration options. The same bounded query is run first in the current
-- project and, when requested, in the public project with shared_only=true.
-- name: ListCurrentConfigurationOptionCandidates :many
SELECT elitea_title,
       label,
       type,
       section,
       shared,
       project_id
FROM configuration
WHERE project_id = sqlc.arg('project_id')::integer
  AND (
      (
          COALESCE(cardinality(sqlc.arg('types')::text[]), 0) > 0
          AND type = ANY(sqlc.arg('types')::text[])
      )
      OR (
          COALESCE(cardinality(sqlc.arg('sections')::text[]), 0) > 0
          AND section = ANY(sqlc.arg('sections')::text[])
      )
  )
  AND (NOT sqlc.arg('shared_only')::boolean OR shared = true)
ORDER BY id ASC
LIMIT sqlc.arg('limit_rows')::integer;

-- Raw data is intentional: the Go adapter performs type-safe, redacted
-- decoding before applying section-specific response shaping. ID order gives
-- duplicate candidates a deterministic baseline order.
-- name: GetCurrentModelCatalogBounds :one
SELECT count(*)::bigint AS row_count,
       COALESCE(sum(candidate.projected_bytes), 0)::bigint AS projected_bytes
FROM (
    SELECT octet_length(data::text)::bigint
             + octet_length(elitea_title)
             + octet_length(section)
             + COALESCE(octet_length(label), 0) AS projected_bytes
    FROM configuration
    WHERE project_id = sqlc.arg('project_id')::integer
      AND section = sqlc.arg('section')::text
      AND status_ok = true
      AND (NOT sqlc.arg('shared_only')::boolean OR shared = true)
    ORDER BY id ASC
    LIMIT sqlc.arg('limit_rows')::integer
) AS candidate;

-- name: ListCurrentModelConfigurations :many
SELECT id,
       project_id,
       label,
       elitea_title,
       section,
       data,
       shared
FROM configuration
WHERE project_id = sqlc.arg('project_id')::integer
  AND section = sqlc.arg('section')::text
  AND status_ok = true
  AND (NOT sqlc.arg('shared_only')::boolean OR shared = true)
ORDER BY id ASC
LIMIT sqlc.arg('limit_rows')::integer;

-- name: CountCurrentConfigurations :one
SELECT count(*)
FROM configuration
WHERE project_id = sqlc.arg('project_id')::integer
  AND (COALESCE(cardinality(sqlc.arg('ids')::integer[]), 0) = 0 OR id = ANY(sqlc.arg('ids')::integer[]))
  AND (COALESCE(cardinality(sqlc.arg('types')::text[]), 0) = 0 OR type = ANY(sqlc.arg('types')::text[]))
  AND (COALESCE(cardinality(sqlc.arg('sections')::text[]), 0) = 0 OR section = ANY(sqlc.arg('sections')::text[]))
  AND (sqlc.arg('label_query')::text = '' OR label ILIKE ('%' || sqlc.arg('label_query')::text || '%'));

-- name: ListCurrentConfigurations :many
SELECT configuration.id,
       configuration.uuid::text AS configuration_uuid,
       configuration.project_id,
       configuration.label,
       configuration.elitea_title,
       configuration.type,
       configuration.section,
       configuration.data,
       configuration.meta,
       configuration.shared,
       configuration.status_ok,
       configuration.status_logs,
       configuration.source,
       configuration.author_id,
       configuration.created_at,
       configuration.updated_at,
       COALESCE(social_pins.id IS NOT NULL, false)::boolean AS is_pinned
FROM configuration
LEFT JOIN centry.social_pins
  ON social_pins.entity = 'configuration'
 AND social_pins.project_id = sqlc.arg('project_id')::integer
 AND social_pins.entity_id = configuration.id
WHERE configuration.project_id = sqlc.arg('project_id')::integer
  AND (COALESCE(cardinality(sqlc.arg('ids')::integer[]), 0) = 0 OR configuration.id = ANY(sqlc.arg('ids')::integer[]))
  AND (COALESCE(cardinality(sqlc.arg('types')::text[]), 0) = 0 OR configuration.type = ANY(sqlc.arg('types')::text[]))
  AND (COALESCE(cardinality(sqlc.arg('sections')::text[]), 0) = 0 OR configuration.section = ANY(sqlc.arg('sections')::text[]))
  AND (sqlc.arg('label_query')::text = '' OR configuration.label ILIKE ('%' || sqlc.arg('label_query')::text || '%'))
ORDER BY
  (social_pins.id IS NOT NULL) DESC,
  social_pins.updated_at DESC NULLS LAST,
  CASE WHEN sqlc.arg('sort_order')::text = 'asc'  AND sqlc.arg('sort_by')::text = 'id'            THEN configuration.id END ASC,
  CASE WHEN sqlc.arg('sort_order')::text = 'desc' AND sqlc.arg('sort_by')::text = 'id'            THEN configuration.id END DESC,
  CASE WHEN sqlc.arg('sort_order')::text = 'asc'  AND sqlc.arg('sort_by')::text = 'uuid'          THEN configuration.uuid END ASC,
  CASE WHEN sqlc.arg('sort_order')::text = 'desc' AND sqlc.arg('sort_by')::text = 'uuid'          THEN configuration.uuid END DESC,
  CASE WHEN sqlc.arg('sort_order')::text = 'asc'  AND sqlc.arg('sort_by')::text = 'project_id'    THEN configuration.project_id END ASC,
  CASE WHEN sqlc.arg('sort_order')::text = 'desc' AND sqlc.arg('sort_by')::text = 'project_id'    THEN configuration.project_id END DESC,
  CASE WHEN sqlc.arg('sort_order')::text = 'asc'  AND sqlc.arg('sort_by')::text = 'label'         THEN configuration.label END ASC NULLS LAST,
  CASE WHEN sqlc.arg('sort_order')::text = 'desc' AND sqlc.arg('sort_by')::text = 'label'         THEN configuration.label END DESC NULLS LAST,
  CASE WHEN sqlc.arg('sort_order')::text = 'asc'  AND sqlc.arg('sort_by')::text = 'elitea_title'  THEN configuration.elitea_title END ASC,
  CASE WHEN sqlc.arg('sort_order')::text = 'desc' AND sqlc.arg('sort_by')::text = 'elitea_title'  THEN configuration.elitea_title END DESC,
  CASE WHEN sqlc.arg('sort_order')::text = 'asc'  AND sqlc.arg('sort_by')::text = 'type'          THEN configuration.type END ASC,
  CASE WHEN sqlc.arg('sort_order')::text = 'desc' AND sqlc.arg('sort_by')::text = 'type'          THEN configuration.type END DESC,
  CASE WHEN sqlc.arg('sort_order')::text = 'asc'  AND sqlc.arg('sort_by')::text = 'section'       THEN configuration.section END ASC,
  CASE WHEN sqlc.arg('sort_order')::text = 'desc' AND sqlc.arg('sort_by')::text = 'section'       THEN configuration.section END DESC,
  CASE WHEN sqlc.arg('sort_order')::text = 'asc'  AND sqlc.arg('sort_by')::text = 'data'          THEN configuration.data END ASC,
  CASE WHEN sqlc.arg('sort_order')::text = 'desc' AND sqlc.arg('sort_by')::text = 'data'          THEN configuration.data END DESC,
  CASE WHEN sqlc.arg('sort_order')::text = 'asc'  AND sqlc.arg('sort_by')::text = 'meta'          THEN configuration.meta END ASC,
  CASE WHEN sqlc.arg('sort_order')::text = 'desc' AND sqlc.arg('sort_by')::text = 'meta'          THEN configuration.meta END DESC,
  CASE WHEN sqlc.arg('sort_order')::text = 'asc'  AND sqlc.arg('sort_by')::text = 'shared'        THEN configuration.shared END ASC,
  CASE WHEN sqlc.arg('sort_order')::text = 'desc' AND sqlc.arg('sort_by')::text = 'shared'        THEN configuration.shared END DESC,
  CASE WHEN sqlc.arg('sort_order')::text = 'asc'  AND sqlc.arg('sort_by')::text = 'status_ok'     THEN configuration.status_ok END ASC,
  CASE WHEN sqlc.arg('sort_order')::text = 'desc' AND sqlc.arg('sort_by')::text = 'status_ok'     THEN configuration.status_ok END DESC,
  CASE WHEN sqlc.arg('sort_order')::text = 'asc'  AND sqlc.arg('sort_by')::text = 'status_logs'   THEN configuration.status_logs END ASC NULLS LAST,
  CASE WHEN sqlc.arg('sort_order')::text = 'desc' AND sqlc.arg('sort_by')::text = 'status_logs'   THEN configuration.status_logs END DESC NULLS LAST,
  CASE WHEN sqlc.arg('sort_order')::text = 'asc'  AND sqlc.arg('sort_by')::text = 'source'        THEN configuration.source END ASC,
  CASE WHEN sqlc.arg('sort_order')::text = 'desc' AND sqlc.arg('sort_by')::text = 'source'        THEN configuration.source END DESC,
  CASE WHEN sqlc.arg('sort_order')::text = 'asc'  AND sqlc.arg('sort_by')::text = 'author_id'     THEN configuration.author_id END ASC NULLS LAST,
  CASE WHEN sqlc.arg('sort_order')::text = 'desc' AND sqlc.arg('sort_by')::text = 'author_id'     THEN configuration.author_id END DESC NULLS LAST,
  CASE WHEN sqlc.arg('sort_order')::text = 'asc'  AND sqlc.arg('sort_by')::text = 'created_at'    THEN configuration.created_at END ASC,
  CASE WHEN sqlc.arg('sort_order')::text = 'desc' AND sqlc.arg('sort_by')::text = 'created_at'    THEN configuration.created_at END DESC,
  CASE WHEN sqlc.arg('sort_order')::text = 'asc'  AND sqlc.arg('sort_by')::text = 'updated_at'    THEN configuration.updated_at END ASC NULLS LAST,
  CASE WHEN sqlc.arg('sort_order')::text = 'desc' AND sqlc.arg('sort_by')::text = 'updated_at'    THEN configuration.updated_at END DESC NULLS LAST,
  configuration.id ASC
LIMIT sqlc.arg('limit_rows')::integer
OFFSET sqlc.arg('offset_rows')::integer;

-- name: CountCurrentSharedConfigurations :one
SELECT count(*)
FROM configuration
WHERE project_id = sqlc.arg('project_id')::integer
  AND shared = true
  AND (COALESCE(cardinality(sqlc.arg('ids')::integer[]), 0) = 0 OR id = ANY(sqlc.arg('ids')::integer[]))
  AND (COALESCE(cardinality(sqlc.arg('types')::text[]), 0) = 0 OR type = ANY(sqlc.arg('types')::text[]))
  AND (COALESCE(cardinality(sqlc.arg('sections')::text[]), 0) = 0 OR section = ANY(sqlc.arg('sections')::text[]));

-- name: ListCurrentSharedConfigurations :many
SELECT id,
       uuid::text AS configuration_uuid,
       project_id,
       label,
       elitea_title,
       type,
       section,
       data,
       meta,
       shared,
       status_ok,
       status_logs,
       source,
       author_id,
       created_at,
       updated_at,
       false AS is_pinned
FROM configuration
WHERE project_id = sqlc.arg('project_id')::integer
  AND shared = true
  AND (COALESCE(cardinality(sqlc.arg('ids')::integer[]), 0) = 0 OR id = ANY(sqlc.arg('ids')::integer[]))
  AND (COALESCE(cardinality(sqlc.arg('types')::text[]), 0) = 0 OR type = ANY(sqlc.arg('types')::text[]))
  AND (COALESCE(cardinality(sqlc.arg('sections')::text[]), 0) = 0 OR section = ANY(sqlc.arg('sections')::text[]))
ORDER BY
  CASE WHEN sqlc.arg('sort_order')::text = 'asc'  AND sqlc.arg('sort_by')::text = 'id'            THEN id END ASC,
  CASE WHEN sqlc.arg('sort_order')::text = 'desc' AND sqlc.arg('sort_by')::text = 'id'            THEN id END DESC,
  CASE WHEN sqlc.arg('sort_order')::text = 'asc'  AND sqlc.arg('sort_by')::text = 'uuid'          THEN uuid END ASC,
  CASE WHEN sqlc.arg('sort_order')::text = 'desc' AND sqlc.arg('sort_by')::text = 'uuid'          THEN uuid END DESC,
  CASE WHEN sqlc.arg('sort_order')::text = 'asc'  AND sqlc.arg('sort_by')::text = 'project_id'    THEN project_id END ASC,
  CASE WHEN sqlc.arg('sort_order')::text = 'desc' AND sqlc.arg('sort_by')::text = 'project_id'    THEN project_id END DESC,
  CASE WHEN sqlc.arg('sort_order')::text = 'asc'  AND sqlc.arg('sort_by')::text = 'label'         THEN label END ASC NULLS LAST,
  CASE WHEN sqlc.arg('sort_order')::text = 'desc' AND sqlc.arg('sort_by')::text = 'label'         THEN label END DESC NULLS LAST,
  CASE WHEN sqlc.arg('sort_order')::text = 'asc'  AND sqlc.arg('sort_by')::text = 'elitea_title'  THEN elitea_title END ASC,
  CASE WHEN sqlc.arg('sort_order')::text = 'desc' AND sqlc.arg('sort_by')::text = 'elitea_title'  THEN elitea_title END DESC,
  CASE WHEN sqlc.arg('sort_order')::text = 'asc'  AND sqlc.arg('sort_by')::text = 'type'          THEN type END ASC,
  CASE WHEN sqlc.arg('sort_order')::text = 'desc' AND sqlc.arg('sort_by')::text = 'type'          THEN type END DESC,
  CASE WHEN sqlc.arg('sort_order')::text = 'asc'  AND sqlc.arg('sort_by')::text = 'section'       THEN section END ASC,
  CASE WHEN sqlc.arg('sort_order')::text = 'desc' AND sqlc.arg('sort_by')::text = 'section'       THEN section END DESC,
  CASE WHEN sqlc.arg('sort_order')::text = 'asc'  AND sqlc.arg('sort_by')::text = 'data'          THEN data END ASC,
  CASE WHEN sqlc.arg('sort_order')::text = 'desc' AND sqlc.arg('sort_by')::text = 'data'          THEN data END DESC,
  CASE WHEN sqlc.arg('sort_order')::text = 'asc'  AND sqlc.arg('sort_by')::text = 'meta'          THEN meta END ASC,
  CASE WHEN sqlc.arg('sort_order')::text = 'desc' AND sqlc.arg('sort_by')::text = 'meta'          THEN meta END DESC,
  CASE WHEN sqlc.arg('sort_order')::text = 'asc'  AND sqlc.arg('sort_by')::text = 'shared'        THEN shared END ASC,
  CASE WHEN sqlc.arg('sort_order')::text = 'desc' AND sqlc.arg('sort_by')::text = 'shared'        THEN shared END DESC,
  CASE WHEN sqlc.arg('sort_order')::text = 'asc'  AND sqlc.arg('sort_by')::text = 'status_ok'     THEN status_ok END ASC,
  CASE WHEN sqlc.arg('sort_order')::text = 'desc' AND sqlc.arg('sort_by')::text = 'status_ok'     THEN status_ok END DESC,
  CASE WHEN sqlc.arg('sort_order')::text = 'asc'  AND sqlc.arg('sort_by')::text = 'status_logs'   THEN status_logs END ASC NULLS LAST,
  CASE WHEN sqlc.arg('sort_order')::text = 'desc' AND sqlc.arg('sort_by')::text = 'status_logs'   THEN status_logs END DESC NULLS LAST,
  CASE WHEN sqlc.arg('sort_order')::text = 'asc'  AND sqlc.arg('sort_by')::text = 'source'        THEN source END ASC,
  CASE WHEN sqlc.arg('sort_order')::text = 'desc' AND sqlc.arg('sort_by')::text = 'source'        THEN source END DESC,
  CASE WHEN sqlc.arg('sort_order')::text = 'asc'  AND sqlc.arg('sort_by')::text = 'author_id'     THEN author_id END ASC NULLS LAST,
  CASE WHEN sqlc.arg('sort_order')::text = 'desc' AND sqlc.arg('sort_by')::text = 'author_id'     THEN author_id END DESC NULLS LAST,
  CASE WHEN sqlc.arg('sort_order')::text = 'asc'  AND sqlc.arg('sort_by')::text = 'created_at'    THEN created_at END ASC,
  CASE WHEN sqlc.arg('sort_order')::text = 'desc' AND sqlc.arg('sort_by')::text = 'created_at'    THEN created_at END DESC,
  CASE WHEN sqlc.arg('sort_order')::text = 'asc'  AND sqlc.arg('sort_by')::text = 'updated_at'    THEN updated_at END ASC NULLS LAST,
  CASE WHEN sqlc.arg('sort_order')::text = 'desc' AND sqlc.arg('sort_by')::text = 'updated_at'    THEN updated_at END DESC NULLS LAST,
  id ASC
LIMIT sqlc.arg('limit_rows')::integer
OFFSET sqlc.arg('offset_rows')::integer;

-- name: InsertCurrentConfiguration :one
INSERT INTO configuration (
    uuid, project_id, label, elitea_title, type, section, data, meta, shared,
    status_ok, status_logs, source, author_id
) VALUES (
    sqlc.arg('configuration_uuid')::text::uuid,
    sqlc.arg('project_id')::integer,
    sqlc.narg('label')::text,
    sqlc.arg('elitea_title')::text,
    sqlc.arg('configuration_type')::text,
    sqlc.arg('section')::text,
    sqlc.arg('data')::jsonb,
    sqlc.arg('meta')::jsonb,
    sqlc.arg('shared')::boolean,
    sqlc.arg('status_ok')::boolean,
    sqlc.narg('status_logs')::text,
    sqlc.arg('source')::text,
    sqlc.narg('author_id')::integer
)
RETURNING id,
          uuid::text AS configuration_uuid,
          project_id,
          label,
          elitea_title,
          type,
          section,
          data,
          meta,
          shared,
          status_ok,
          status_logs,
          source,
          author_id,
          created_at,
          updated_at,
          false AS is_pinned;

-- name: ReplaceCurrentConfiguration :one
UPDATE configuration
SET label = sqlc.narg('label')::text,
    elitea_title = sqlc.arg('elitea_title')::text,
    data = sqlc.arg('data')::jsonb,
    meta = sqlc.arg('meta')::jsonb,
    shared = sqlc.arg('shared')::boolean,
    status_ok = sqlc.arg('status_ok')::boolean,
    status_logs = sqlc.narg('status_logs')::text,
    updated_at = now()
WHERE id = sqlc.arg('configuration_id')::integer
  AND project_id = sqlc.arg('project_id')::integer
RETURNING id,
          uuid::text AS configuration_uuid,
          project_id,
          label,
          elitea_title,
          type,
          section,
          data,
          meta,
          shared,
          status_ok,
          status_logs,
          source,
          author_id,
          created_at,
          updated_at,
          false AS is_pinned;

-- name: DeleteCurrentConfiguration :one
DELETE FROM configuration
WHERE id = sqlc.arg('configuration_id')::integer
  AND project_id = sqlc.arg('project_id')::integer
RETURNING id;
