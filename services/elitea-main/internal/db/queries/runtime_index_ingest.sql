-- name: GetRuntimeAdmissionByIdempotency :one
SELECT j.execution_id,
       j.command_id,
       j.generation,
       i.index_generation,
       j.request_digest,
       j.admitted_at,
       o.deadline,
       i.index_meta_id,
       i.index_meta_correlation_id,
       i.index_meta_initialized_at
FROM elitea_runtime.execution_jobs AS j
JOIN elitea_runtime.command_outbox AS o
  ON o.execution_id = j.execution_id AND o.generation = j.generation
JOIN elitea_runtime.index_ingest_jobs AS i
  ON i.execution_id = j.execution_id AND i.generation = j.generation
WHERE j.idempotency_scope = sqlc.arg(idempotency_scope)::text
  AND j.idempotency_key = sqlc.arg(idempotency_key)::text
  AND j.capability_id = 'index.ingest.v1';

-- name: EnsureRuntimeAdmissionPolicy :exec
INSERT INTO elitea_runtime.execution_admission_policies (
    capability_id, max_outstanding
) VALUES (
    sqlc.arg(capability_id)::text,
    sqlc.arg(max_outstanding)::bigint
)
ON CONFLICT (capability_id) DO NOTHING;

-- name: LockRuntimeAdmissionPolicy :one
SELECT max_outstanding
FROM elitea_runtime.execution_admission_policies
WHERE capability_id = sqlc.arg(capability_id)::text
FOR UPDATE;

-- name: ListActiveIndexIngestTarget :many
SELECT j.execution_id
FROM elitea_runtime.execution_jobs AS j
JOIN elitea_runtime.index_ingest_jobs AS i
  ON i.execution_id = j.execution_id
 AND i.generation = j.generation
WHERE j.capability_id = 'index.ingest.v1'
  AND j.tenant_id = sqlc.arg(tenant_id)::text
  AND j.resource_project_id = sqlc.arg(resource_project_id)::integer
  AND j.projection_project_id = sqlc.arg(projection_project_id)::integer
  AND i.capability_id = j.capability_id
  AND i.toolkit_id = sqlc.arg(toolkit_id)::integer
  AND i.index_name = sqlc.arg(index_name)::text
  AND j.state IN ('PENDING', 'DISPATCHED', 'CLAIMED', 'RUNNING', 'SETTLING')
ORDER BY j.admitted_at, j.execution_id
LIMIT 2;

-- name: CountActiveRuntimeExecutionsUpTo :one
SELECT count(*)
FROM (
    SELECT 1
    FROM elitea_runtime.execution_jobs
    WHERE capability_id = sqlc.arg(capability_id)::text
      AND state IN ('PENDING', 'DISPATCHED', 'CLAIMED', 'RUNNING', 'SETTLING')
    LIMIT sqlc.arg(max_outstanding)::bigint
) AS bounded_active;

-- name: LoadRuntimeAdmissionTiming :one
WITH authority AS MATERIALIZED (
    SELECT date_trunc('milliseconds', clock_timestamp()) AS admitted_at
)
SELECT admitted_at::timestamptz AS admitted_at,
       (admitted_at + (sqlc.arg(deadline_ttl_millis)::bigint * interval '1 millisecond'))::timestamptz AS deadline
FROM authority;

-- name: AllocateIndexGeneration :one
INSERT INTO elitea_runtime.index_generation_counters (
    resource_project_id, toolkit_id, index_name, last_generation, updated_at
) VALUES (
    sqlc.arg(resource_project_id)::integer,
    sqlc.arg(toolkit_id)::integer,
    sqlc.arg(index_name)::text,
    1,
    clock_timestamp()
)
ON CONFLICT (resource_project_id, toolkit_id, index_name) DO UPDATE
SET last_generation =
        elitea_runtime.index_generation_counters.last_generation + 1,
    updated_at = clock_timestamp()
WHERE elitea_runtime.index_generation_counters.last_generation
      < 9223372036854775807
RETURNING last_generation;

-- name: InsertRuntimeInputBundle :exec
INSERT INTO elitea_runtime.input_bundles (
    input_bundle_id, immutable_version, media_type, resource_project_id,
    manifest_digest, manifest_size, manifest_bytes, created_by, created_at
) VALUES (
    sqlc.arg(input_bundle_id)::text,
    sqlc.arg(immutable_version)::text,
    sqlc.arg(media_type)::text,
    sqlc.arg(resource_project_id)::integer,
    sqlc.arg(manifest_digest)::bytea,
    sqlc.arg(manifest_size)::bigint,
    sqlc.arg(manifest_bytes)::bytea,
    sqlc.arg(created_by)::text,
    sqlc.arg(created_at)::timestamptz
);

-- name: InsertRuntimeInputBundleEntry :exec
INSERT INTO elitea_runtime.input_bundle_entries (
    input_bundle_id, entry_id, entry_version, semantic_role, media_type,
    content_digest, content_size, content_reference, classification,
    required_grant_audience, content_bytes
) VALUES (
    sqlc.arg(input_bundle_id)::text,
    sqlc.arg(entry_id)::text,
    sqlc.arg(entry_version)::text,
    sqlc.arg(semantic_role)::text,
    sqlc.arg(media_type)::text,
    sqlc.arg(content_digest)::bytea,
    sqlc.arg(content_size)::bigint,
    sqlc.arg(content_reference)::text,
    sqlc.arg(classification)::text,
    sqlc.arg(required_grant_audience)::text,
    sqlc.arg(content_bytes)::bytea
);

-- name: InsertIndexIngestExecutionJob :one
INSERT INTO elitea_runtime.execution_jobs (
    execution_id, generation, command_id, tenant_id, resource_project_id,
    projection_project_id, actor_id, principal_ref, capability_id,
    capability_version, input_bundle_id, request_digest,
    idempotency_scope, idempotency_key, state, desired_state, admitted_at,
    trigger_origin
) VALUES (
    sqlc.arg(execution_id)::text,
    sqlc.arg(generation)::bigint,
    sqlc.arg(command_id)::text,
    sqlc.arg(tenant_id)::text,
    sqlc.arg(resource_project_id)::integer,
    sqlc.arg(projection_project_id)::integer,
    sqlc.arg(actor_id)::text,
    sqlc.arg(principal_ref)::text,
    'index.ingest.v1',
    sqlc.arg(capability_version)::text,
    sqlc.arg(input_bundle_id)::text,
    sqlc.arg(request_digest)::bytea,
    sqlc.arg(idempotency_scope)::text,
    sqlc.arg(idempotency_key)::text,
    sqlc.arg(state)::text,
    'RUNNING',
    sqlc.arg(admitted_at)::timestamptz,
    -- shared/0140: every index ingest is an unattended run, whoever started
    -- it, so the analytics active-user reads do not count it as a person's
    -- activity.
    'index'
)
ON CONFLICT (idempotency_scope, idempotency_key) DO NOTHING
RETURNING execution_id;

-- name: InsertIndexIngestJob :exec
INSERT INTO elitea_runtime.index_ingest_jobs (
    execution_id, generation, index_generation, capability_id, input_bundle_id,
    toolkit_configuration_entry_id, tool_parameters_entry_id,
    llm_model_entry_id, llm_configuration_entry_id, mcp_tokens_entry_id,
    embedding_binding_entry_id,
    client_stream_id, client_message_id, sio_event,
    index_meta_id, index_meta_correlation_id,
    index_meta_initialization_status,
    index_meta_initialization_attempt_count,
    index_meta_initialization_next_attempt_at,
    toolkit_id, index_name, initiator
) VALUES (
    sqlc.arg(execution_id)::text,
    sqlc.arg(generation)::bigint,
    sqlc.arg(index_generation)::bigint,
    'index.ingest.v1',
    sqlc.arg(input_bundle_id)::text,
    sqlc.arg(toolkit_configuration_entry_id)::text,
    sqlc.arg(tool_parameters_entry_id)::text,
    sqlc.narg(llm_model_entry_id)::text,
    sqlc.narg(llm_configuration_entry_id)::text,
    sqlc.narg(mcp_tokens_entry_id)::text,
    sqlc.narg(embedding_binding_entry_id)::text,
    sqlc.narg(client_stream_id)::text,
    sqlc.narg(client_message_id)::text,
    sqlc.narg(sio_event)::text,
    sqlc.arg(index_meta_id)::text,
    sqlc.arg(index_meta_correlation_id)::text,
    'PENDING',
    0,
    sqlc.arg(index_meta_initialization_next_attempt_at)::timestamptz,
    sqlc.arg(toolkit_id)::integer,
    sqlc.arg(index_name)::text,
    sqlc.arg(initiator)::text
);

-- name: InsertCurrentIndexTerminalNotification :execrows
WITH source AS MATERIALIZED (
    SELECT j.resource_project_id,
           j.actor_id::integer AS user_id,
           i.toolkit_id,
           i.index_name,
           i.index_meta_id,
           i.index_generation,
           i.initiator,
           CASE
               WHEN sqlc.arg(terminal_state)::text IN (
                   'created',
                   'completed',
                   'scheduled_reindex',
                   'failed',
                   'partly_indexed'
               )
                   THEN sqlc.arg(terminal_state)::text
               WHEN sqlc.arg(completion_status)::text = 'error'
                   THEN 'failed'
               WHEN sqlc.arg(completion_status)::text = 'partly_indexed'
                   THEN 'partly_indexed'
               WHEN i.initiator = 'schedule'
                   THEN 'scheduled_reindex'
               WHEN i.index_generation = 1
                   THEN 'created'
               ELSE 'completed'
           END AS terminal_state,
           CASE
               WHEN sqlc.arg(reindex_present)::boolean
                   THEN sqlc.arg(reindex)::boolean
               WHEN i.initiator = 'schedule'
                   THEN TRUE
               ELSE i.index_generation > 1
           END AS reindex
    FROM elitea_runtime.execution_jobs AS j
    JOIN elitea_runtime.index_ingest_jobs AS i
      ON i.execution_id = j.execution_id
     AND i.generation = j.generation
     AND i.capability_id = j.capability_id
    WHERE j.execution_id = sqlc.arg(execution_id)::text
      AND j.generation = sqlc.arg(generation)::bigint
      AND j.capability_id = 'index.ingest.v1'
      AND i.initiator IN ('user', 'schedule')
),
notification AS (
    SELECT source.*,
           CASE
               WHEN source.terminal_state = 'failed'
                   THEN format('Index [%s]() is failed.', source.index_name)
               WHEN source.reindex
                   THEN format(
                       'Index [%s]() is successfully reindexed%s. {"reindexed": %s, "indexed": %s}',
                       source.index_name,
                       CASE
                           WHEN source.initiator = 'schedule'
                               THEN ' by schedule'
                           ELSE ''
                       END,
                       sqlc.arg(updated)::bigint,
                       sqlc.arg(indexed)::bigint
                   )
               ELSE format(
                   'Index [%s]() is successfully created: {"indexed": %s}',
                   source.index_name,
                   sqlc.arg(indexed)::bigint
               )
           END AS message
    FROM source
    WHERE source.terminal_state IN (
        'created',
        'completed',
        'scheduled_reindex',
        'failed'
    )
)
INSERT INTO centry.notifications (
    uuid,
    is_seen,
    project_id,
    user_id,
    meta,
    event_type
)
SELECT sqlc.arg(notification_uuid)::text::uuid,
       FALSE,
       notification.resource_project_id,
       notification.user_id,
       jsonb_build_object(
           'id', notification.index_meta_id,
           'index_name', notification.index_name,
           'state', notification.terminal_state,
           'error', CASE
               WHEN notification.terminal_state = 'failed'
                   THEN sqlc.arg(error_message)::text
               ELSE NULL
           END,
           'reindex', notification.reindex,
           'indexed', sqlc.arg(indexed)::bigint,
           'updated', sqlc.arg(updated)::bigint,
           'toolkit_id', notification.toolkit_id,
           'initiator', notification.initiator,
           'message', notification.message
       ),
       'index_data_changed'
FROM notification
ON CONFLICT (uuid) DO NOTHING;

-- name: MarkIndexMetaInitialized :one
UPDATE elitea_runtime.index_ingest_jobs AS i
SET index_meta_initialized_at = COALESCE(
    i.index_meta_initialized_at,
    authority.initialized_at
),
    index_meta_initialization_status = 'INITIALIZED',
    index_meta_initialization_claim_token = NULL,
    index_meta_initialization_claim_expires_at = NULL,
    index_meta_initialization_next_attempt_at = NULL,
    index_meta_initialization_last_error_code = NULL,
    index_meta_initialization_resolved_at = COALESCE(
        i.index_meta_initialization_resolved_at,
        authority.initialized_at
    ),
    index_meta_initialization_failed_at = NULL
FROM elitea_runtime.execution_jobs AS j,
     (
         SELECT date_trunc(
             'milliseconds',
             clock_timestamp()
         )::timestamptz AS initialized_at
     ) AS authority
WHERE i.execution_id = sqlc.arg(execution_id)::text
  AND i.generation = sqlc.arg(generation)::bigint
  AND i.index_generation = sqlc.arg(index_generation)::bigint
  AND i.capability_id = 'index.ingest.v1'
  AND i.index_meta_id = sqlc.arg(index_meta_id)::text
  AND i.index_meta_correlation_id = sqlc.arg(index_meta_correlation_id)::text
  AND j.execution_id = i.execution_id
  AND j.generation = i.generation
  AND j.capability_id = i.capability_id
  AND j.state = 'PENDING'
  AND j.desired_state = 'RUNNING'
RETURNING i.index_meta_initialized_at;

-- name: ClaimPendingIndexMetaInitializations :many
WITH candidates AS (
    SELECT i.execution_id, i.generation
    FROM elitea_runtime.index_ingest_jobs AS i
    JOIN elitea_runtime.execution_jobs AS j
      ON j.execution_id = i.execution_id
     AND j.generation = i.generation
     AND j.capability_id = i.capability_id
    JOIN elitea_runtime.command_outbox AS o
      ON o.execution_id = i.execution_id
     AND o.generation = i.generation
    WHERE i.capability_id = 'index.ingest.v1'
      AND i.index_meta_initialized_at IS NULL
      AND i.index_meta_initialization_status IN ('PENDING', 'RUNNING')
      AND (
          i.index_meta_initialization_status = 'PENDING'
          AND i.index_meta_initialization_next_attempt_at <= clock_timestamp()
          OR
          i.index_meta_initialization_status = 'RUNNING'
          AND i.index_meta_initialization_claim_expires_at <= clock_timestamp()
      )
      AND j.state = 'PENDING'
      AND j.desired_state = 'RUNNING'
      AND o.prepared_at IS NULL
      AND o.published_at IS NULL
      AND o.authority_granted_at IS NULL
      AND o.retired_at IS NULL
    ORDER BY COALESCE(
                 i.index_meta_initialization_next_attempt_at,
                 i.index_meta_initialization_claim_expires_at
             ),
             i.execution_id,
             i.generation
    LIMIT sqlc.arg(claim_limit)::integer
    FOR UPDATE OF i SKIP LOCKED
),
claimed AS (
    UPDATE elitea_runtime.index_ingest_jobs AS i
    SET index_meta_initialization_status = 'RUNNING',
        index_meta_initialization_claim_token = sqlc.arg(claim_token)::text,
        index_meta_initialization_claim_expires_at =
            clock_timestamp()
            + (
                sqlc.arg(claim_lease_microseconds)::bigint
                * interval '1 microsecond'
            ),
        index_meta_initialization_attempt_count = LEAST(
            i.index_meta_initialization_attempt_count::bigint + 1,
            2147483647
        )::integer,
        index_meta_initialization_next_attempt_at = NULL,
        index_meta_initialization_last_error_code = NULL
    FROM candidates
    WHERE i.execution_id = candidates.execution_id
      AND i.generation = candidates.generation
    RETURNING i.execution_id,
              i.generation,
              i.index_meta_initialization_attempt_count,
              i.index_meta_initialization_claim_expires_at
)
SELECT execution_id, generation, index_meta_initialization_attempt_count,
       index_meta_initialization_claim_expires_at
FROM claimed
ORDER BY execution_id, generation;

-- name: QuarantineExpiredTerminalIndexMetaInitializations :one
WITH candidates AS MATERIALIZED (
    SELECT i.execution_id, i.generation
    FROM elitea_runtime.index_ingest_jobs AS i
    JOIN elitea_runtime.execution_jobs AS j
      ON j.execution_id = i.execution_id
     AND j.generation = i.generation
     AND j.capability_id = i.capability_id
    JOIN elitea_runtime.command_outbox AS o
      ON o.execution_id = i.execution_id
     AND o.generation = i.generation
    WHERE i.capability_id = 'index.ingest.v1'
      AND i.index_meta_initialized_at IS NULL
      AND i.index_meta_initialization_status = 'RUNNING'
      AND i.index_meta_initialization_claim_expires_at <= clock_timestamp()
      AND j.state = 'FAILED'
      AND j.terminal_error_code = 'DEADLINE_EXCEEDED'
      AND o.prepared_at IS NULL
      AND o.published_at IS NULL
      AND o.authority_granted_at IS NULL
      AND o.retired_at IS NOT NULL
      AND o.retirement_code = 'DEADLINE_EXCEEDED'
    ORDER BY i.index_meta_initialization_claim_expires_at,
             i.execution_id,
             i.generation
    LIMIT sqlc.arg(quarantine_limit)::integer
    FOR UPDATE OF i, j, o SKIP LOCKED
),
quarantined AS (
    UPDATE elitea_runtime.index_ingest_jobs AS i
    SET index_meta_initialization_status = 'QUARANTINED',
        index_meta_initialization_claim_token = NULL,
        index_meta_initialization_claim_expires_at = NULL,
        index_meta_initialization_next_attempt_at = NULL,
        index_meta_initialization_last_error_code =
            'INITIALIZATION_DEADLINE_EXCEEDED',
        index_meta_initialization_failed_at =
            date_trunc('milliseconds', clock_timestamp())
    FROM candidates
    WHERE i.execution_id = candidates.execution_id
      AND i.generation = candidates.generation
    RETURNING i.execution_id
)
SELECT count(*)::bigint AS quarantined_count
FROM quarantined;

-- name: ClaimExactIndexMetaInitialization :one
WITH claimed AS (
    UPDATE elitea_runtime.index_ingest_jobs AS i
    SET index_meta_initialization_status = 'RUNNING',
        index_meta_initialization_claim_token = sqlc.arg(claim_token)::text,
        index_meta_initialization_claim_expires_at =
            clock_timestamp()
            + (
                sqlc.arg(claim_lease_microseconds)::bigint
                * interval '1 microsecond'
            ),
        index_meta_initialization_attempt_count = LEAST(
            i.index_meta_initialization_attempt_count::bigint + 1,
            2147483647
        )::integer,
        index_meta_initialization_next_attempt_at = NULL,
        index_meta_initialization_last_error_code = NULL
    FROM elitea_runtime.execution_jobs AS j,
         elitea_runtime.command_outbox AS o
    WHERE i.execution_id = sqlc.arg(execution_id)::text
      AND i.generation = sqlc.arg(generation)::bigint
      AND i.index_generation = sqlc.arg(index_generation)::bigint
      AND i.index_meta_id = sqlc.arg(index_meta_id)::text
      AND i.index_meta_correlation_id =
          sqlc.arg(index_meta_correlation_id)::text
      AND i.capability_id = 'index.ingest.v1'
      AND i.index_meta_initialized_at IS NULL
      AND (
          i.index_meta_initialization_status = 'PENDING'
          AND i.index_meta_initialization_next_attempt_at <= clock_timestamp()
          OR
          i.index_meta_initialization_status = 'RUNNING'
          AND i.index_meta_initialization_claim_expires_at <= clock_timestamp()
      )
      AND j.execution_id = i.execution_id
      AND j.generation = i.generation
      AND j.capability_id = i.capability_id
      AND j.state = 'PENDING'
      AND j.desired_state = 'RUNNING'
      AND o.execution_id = i.execution_id
      AND o.generation = i.generation
      AND o.prepared_at IS NULL
      AND o.published_at IS NULL
      AND o.authority_granted_at IS NULL
      AND o.retired_at IS NULL
    RETURNING i.execution_id,
              i.generation,
              i.index_meta_initialization_attempt_count,
              i.index_meta_initialization_claim_expires_at
)
SELECT execution_id, generation, index_meta_initialization_attempt_count,
       index_meta_initialization_claim_expires_at
FROM claimed;

-- name: LoadIndexMetaInitializationWork :one
SELECT j.command_id,
       j.tenant_id,
       j.resource_project_id,
       j.projection_project_id,
       j.actor_id,
       j.admitted_at,
       o.deadline,
       i.index_generation,
       i.index_meta_id,
       i.index_meta_correlation_id,
       i.client_stream_id,
       i.client_message_id,
       i.sio_event,
       i.toolkit_id,
       i.initiator,
       toolkit.content_bytes AS toolkit_configuration,
       parameters.content_bytes AS tool_parameters
FROM elitea_runtime.index_ingest_jobs AS i
JOIN elitea_runtime.execution_jobs AS j
  ON j.execution_id = i.execution_id
 AND j.generation = i.generation
 AND j.capability_id = i.capability_id
JOIN elitea_runtime.command_outbox AS o
  ON o.execution_id = i.execution_id
 AND o.generation = i.generation
LEFT JOIN elitea_runtime.input_bundle_entries AS toolkit
  ON toolkit.input_bundle_id = i.input_bundle_id
 AND toolkit.entry_id = i.toolkit_configuration_entry_id
 AND toolkit.semantic_role = 'index.toolkit_configuration'
LEFT JOIN elitea_runtime.input_bundle_entries AS parameters
  ON parameters.input_bundle_id = i.input_bundle_id
 AND parameters.entry_id = i.tool_parameters_entry_id
 AND parameters.semantic_role = 'index.tool_parameters'
WHERE i.execution_id = sqlc.arg(execution_id)::text
  AND i.generation = sqlc.arg(generation)::bigint
  AND i.capability_id = 'index.ingest.v1'
  AND i.index_meta_initialized_at IS NULL
  AND i.index_meta_initialization_status = 'RUNNING'
  AND i.index_meta_initialization_claim_token = sqlc.arg(claim_token)::text
  AND i.index_meta_initialization_claim_expires_at > clock_timestamp()
  AND j.state = 'PENDING'
  AND j.desired_state = 'RUNNING'
  AND o.prepared_at IS NULL
  AND o.published_at IS NULL
  AND o.authority_granted_at IS NULL
  AND o.retired_at IS NULL;

-- name: ResolveIndexMetaInitialization :one
UPDATE elitea_runtime.index_ingest_jobs AS i
SET index_meta_initialized_at = authority.initialized_at,
    index_meta_initialization_status = 'INITIALIZED',
    index_meta_initialization_claim_token = NULL,
    index_meta_initialization_claim_expires_at = NULL,
    index_meta_initialization_next_attempt_at = NULL,
    index_meta_initialization_last_error_code = NULL,
    index_meta_initialization_resolved_at = authority.initialized_at,
    index_meta_initialization_failed_at = NULL
FROM elitea_runtime.execution_jobs AS j,
     elitea_runtime.command_outbox AS o,
     (
         SELECT date_trunc(
             'milliseconds',
             clock_timestamp()
         )::timestamptz AS initialized_at
     ) AS authority
WHERE i.execution_id = sqlc.arg(execution_id)::text
  AND i.generation = sqlc.arg(generation)::bigint
  AND i.capability_id = 'index.ingest.v1'
  AND i.index_meta_initialized_at IS NULL
  AND i.index_meta_initialization_status = 'RUNNING'
  AND i.index_meta_initialization_claim_token = sqlc.arg(claim_token)::text
  AND i.index_meta_initialization_claim_expires_at > clock_timestamp()
  AND j.execution_id = i.execution_id
  AND j.generation = i.generation
  AND j.capability_id = i.capability_id
  AND j.state = 'PENDING'
  AND j.desired_state = 'RUNNING'
  AND o.execution_id = i.execution_id
  AND o.generation = i.generation
  AND o.prepared_at IS NULL
  AND o.published_at IS NULL
  AND o.authority_granted_at IS NULL
  AND o.retired_at IS NULL
RETURNING i.index_meta_initialized_at;

-- name: ReleaseIndexMetaInitialization :execrows
UPDATE elitea_runtime.index_ingest_jobs
SET index_meta_initialization_status = 'PENDING',
    index_meta_initialization_claim_token = NULL,
    index_meta_initialization_claim_expires_at = NULL,
    index_meta_initialization_next_attempt_at =
        clock_timestamp()
        + (
            LEAST(
                30,
                (1::bigint << LEAST(
                    index_meta_initialization_attempt_count,
                    5
                ))
            ) * interval '1 second'
        ),
    index_meta_initialization_last_error_code =
        sqlc.arg(last_error_code)::text
WHERE execution_id = sqlc.arg(execution_id)::text
  AND generation = sqlc.arg(generation)::bigint
  AND capability_id = 'index.ingest.v1'
  AND index_meta_initialized_at IS NULL
  AND index_meta_initialization_status = 'RUNNING'
  AND index_meta_initialization_claim_token = sqlc.arg(claim_token)::text;

-- name: QuarantineIndexMetaInitialization :one
WITH locked_triple AS MATERIALIZED (
    SELECT i.execution_id,
           i.generation,
           CASE
               WHEN j.state = 'PENDING'
                AND j.desired_state = 'RUNNING'
                AND o.prepared_at IS NULL
                AND o.published_at IS NULL
                AND o.authority_granted_at IS NULL
                AND o.retired_at IS NULL
                   THEN 'ACTIVE_PRE_AUTHORITY'::text
               ELSE 'TERMINAL_DEADLINE'::text
           END AS classification
    FROM elitea_runtime.index_ingest_jobs AS i
    JOIN elitea_runtime.execution_jobs AS j
      ON j.execution_id = i.execution_id
     AND j.generation = i.generation
     AND j.capability_id = i.capability_id
    JOIN elitea_runtime.command_outbox AS o
      ON o.execution_id = i.execution_id
     AND o.generation = i.generation
    WHERE i.execution_id = sqlc.arg(execution_id)::text
      AND i.generation = sqlc.arg(generation)::bigint
      AND i.capability_id = 'index.ingest.v1'
      AND i.index_meta_initialized_at IS NULL
      AND i.index_meta_initialization_status = 'RUNNING'
      AND i.index_meta_initialization_claim_token =
          sqlc.arg(claim_token)::text
      AND (
          (
              j.state = 'PENDING'
              AND j.desired_state = 'RUNNING'
              AND o.prepared_at IS NULL
              AND o.published_at IS NULL
              AND o.authority_granted_at IS NULL
              AND o.retired_at IS NULL
          )
          OR (
              j.state = 'FAILED'
              AND j.terminal_error_code = 'DEADLINE_EXCEEDED'
              AND o.retired_at IS NOT NULL
              AND o.retirement_code = 'DEADLINE_EXCEEDED'
              AND o.authority_granted_at IS NULL
          )
      )
    FOR UPDATE OF i, j, o
),
quarantined_index AS (
    UPDATE elitea_runtime.index_ingest_jobs AS i
    SET index_meta_initialization_status = 'QUARANTINED',
        index_meta_initialization_claim_token = NULL,
        index_meta_initialization_claim_expires_at = NULL,
        index_meta_initialization_next_attempt_at = NULL,
        index_meta_initialization_last_error_code =
            sqlc.arg(last_error_code)::text,
        index_meta_initialization_failed_at =
            date_trunc('milliseconds', clock_timestamp())
    FROM locked_triple AS locked
    WHERE i.execution_id = locked.execution_id
      AND i.generation = locked.generation
    RETURNING i.execution_id, i.generation, locked.classification
),
quarantined_job AS (
    UPDATE elitea_runtime.execution_jobs AS j
    SET state = 'QUARANTINED',
        desired_state = 'CANCELLED',
        settled_at = date_trunc('milliseconds', clock_timestamp())
    FROM quarantined_index AS i
    WHERE j.execution_id = i.execution_id
      AND j.generation = i.generation
      AND i.classification = 'ACTIVE_PRE_AUTHORITY'
    RETURNING j.execution_id, j.generation, j.projection_project_id
),
retired_outbox AS (
    UPDATE elitea_runtime.command_outbox AS o
    SET retired_at = date_trunc('milliseconds', clock_timestamp()),
        retirement_code = 'CANCELLED'
    FROM quarantined_job AS j
    WHERE o.execution_id = j.execution_id
      AND o.generation = j.generation
    RETURNING o.outbox_id, o.execution_id, o.generation
),
replayed_failure AS (
    INSERT INTO elitea_runtime.execution_replay_events (
        event_id, execution_id, generation, projection_project_id,
        event_type, event_bytes, event_digest
    )
    SELECT 'index-meta-initialization-quarantine:' || o.outbox_id,
           j.execution_id,
           j.generation,
           j.projection_project_id,
           'execution.failed',
           sqlc.arg(failure_event_bytes)::bytea,
           sqlc.arg(failure_event_digest)::bytea
    FROM quarantined_job AS j
    JOIN retired_outbox AS o
      ON o.execution_id = j.execution_id
     AND o.generation = j.generation
    RETURNING execution_id
),
converged AS (
    SELECT execution_id
    FROM replayed_failure
    UNION ALL
    SELECT execution_id
    FROM quarantined_index
    WHERE classification = 'TERMINAL_DEADLINE'
)
SELECT execution_id FROM converged;

-- name: InsertRuntimeCommandOutbox :exec
INSERT INTO elitea_runtime.command_outbox (
    outbox_id, execution_id, generation, stream_name, dispatch_ordinal,
    resource_class, isolation_class, priority, deadline, limits_revision,
    traceparent, tracestate, created_at, publish_attempts
) VALUES (
    sqlc.arg(outbox_id)::text,
    sqlc.arg(execution_id)::text,
    sqlc.arg(generation)::bigint,
    sqlc.arg(stream_name)::text,
    1,
    sqlc.arg(resource_class)::text,
    sqlc.arg(isolation_class)::text,
    sqlc.arg(priority)::integer,
    sqlc.arg(deadline)::timestamptz,
    sqlc.arg(limits_revision)::text,
    '',
    '',
    sqlc.arg(created_at)::timestamptz,
    0
);

-- name: GetExpectedIndexIngestHeader :one
SELECT j.tenant_id,
       j.resource_project_id,
       j.projection_project_id,
       j.capability_id,
       j.command_id,
       j.execution_id,
       j.generation,
       b.input_bundle_id,
       b.manifest_digest AS input_bundle_digest,
       i.toolkit_configuration_entry_id,
       i.tool_parameters_entry_id,
       i.llm_model_entry_id,
       i.llm_configuration_entry_id,
       i.mcp_tokens_entry_id,
       i.embedding_binding_entry_id,
       o.limits_revision
FROM elitea_runtime.execution_jobs AS j
JOIN elitea_runtime.index_ingest_jobs AS i
  ON i.execution_id = j.execution_id
 AND i.generation = j.generation
JOIN elitea_runtime.input_bundles AS b
  ON b.input_bundle_id = j.input_bundle_id
JOIN elitea_runtime.command_outbox AS o
  ON o.execution_id = j.execution_id
 AND o.generation = j.generation
WHERE j.execution_id = sqlc.arg(execution_id)::text
  AND j.generation = sqlc.arg(generation)::bigint
  AND j.capability_id = 'index.ingest.v1';

-- name: ListExpectedIndexIngestEntries :many
SELECT e.entry_id,
       e.entry_version,
       e.semantic_role,
       e.content_digest,
       e.classification
FROM elitea_runtime.execution_jobs AS j
JOIN elitea_runtime.index_ingest_jobs AS i
  ON i.execution_id = j.execution_id
 AND i.generation = j.generation
JOIN elitea_runtime.input_bundle_entries AS e
  ON e.input_bundle_id = j.input_bundle_id
WHERE j.execution_id = sqlc.arg(execution_id)::text
  AND j.generation = sqlc.arg(generation)::bigint
  AND j.capability_id = 'index.ingest.v1'
ORDER BY e.entry_id;

-- name: GetDurableIndexResultArtifact :one
SELECT a.artifact_id,
       a.immutable_version,
       a.media_type,
       a.byte_length,
       a.digest,
       a.classification,
       a.storage_record_id,
       a.bytes_verified_at
FROM elitea_runtime.index_result_artifacts AS a
JOIN elitea_runtime.execution_jobs AS j
  ON j.execution_id = a.execution_id
 AND j.generation = a.generation
WHERE a.artifact_id = sqlc.arg(artifact_id)::text
  AND a.immutable_version = sqlc.arg(immutable_version)::text
  AND a.execution_id = sqlc.arg(execution_id)::text
  AND a.generation = sqlc.arg(generation)::bigint
  AND a.resource_project_id = sqlc.arg(resource_project_id)::integer
  AND j.tenant_id = sqlc.arg(tenant_id)::text
  AND j.resource_project_id = sqlc.arg(resource_project_id)::integer
  AND j.projection_project_id = sqlc.arg(projection_project_id)::integer
  AND j.command_id = sqlc.arg(command_id)::text
  AND j.capability_id = 'index.ingest.v1';

-- name: InsertIndexResultArtifact :one
-- S20c: the writer side of elitea_runtime.index_result_artifacts —
-- GetDurableIndexResultArtifact above is the pre-existing read side
-- (ArtifactVerifier.VerifyDurable), this is IndexResultArtifactWriter's own
-- CommitArtifact. No ON CONFLICT: a (artifact_id, immutable_version)
-- collision is handled in Go (repos.IndexResultArtifactWriter), which
-- distinguishes a safe exact-content retry from a genuine identity
-- conflict — a bare ON CONFLICT DO NOTHING can't make that distinction
-- from the write alone.
--
-- metadata_created_at is set explicitly to the same value as
-- bytes_verified_at, rather than left to its own DEFAULT clock_timestamp()
-- — confirmed empirically (a real 23514 violation, not by inspection) that
-- a client-computed bytes_verified_at is always microseconds earlier than
-- a server-side DEFAULT evaluated at INSERT time, which the table's own
-- index_result_artifact_verified_order CHECK (bytes_verified_at >=
-- metadata_created_at) rejects. Reusing one Go-computed timestamp for both
-- columns guarantees equality, which satisfies >=.
INSERT INTO elitea_runtime.index_result_artifacts (
    artifact_id, immutable_version, execution_id, generation,
    resource_project_id, media_type, byte_length, digest, classification,
    storage_record_id, bytes_verified_at, metadata_created_at
) VALUES (
    sqlc.arg(artifact_id)::text, sqlc.arg(immutable_version)::text,
    sqlc.arg(execution_id)::text, sqlc.arg(generation)::bigint,
    sqlc.arg(resource_project_id)::integer, sqlc.arg(media_type)::text,
    sqlc.arg(byte_length)::bigint, sqlc.arg(digest)::bytea,
    sqlc.arg(classification)::text, sqlc.arg(storage_record_id)::text,
    sqlc.arg(bytes_verified_at)::timestamptz, sqlc.arg(bytes_verified_at)::timestamptz
)
RETURNING artifact_id, immutable_version, execution_id, generation,
    resource_project_id, media_type, byte_length, digest, classification,
    storage_record_id, bytes_verified_at;

-- name: GetIndexResultArtifactByPrimaryKey :one
-- S20c: keyed only by the table's actual primary key, no execution_jobs
-- join — used both by CommitArtifact's duplicate-PK reconciliation (decide
-- exact-retry vs genuine conflict) and by ResolveArtifact (find the
-- storage_record_id to resolve a read grant against). Unlike
-- GetDurableIndexResultArtifact above, this does not re-check
-- tenant/projection/command identity; ResolveArtifact does that itself
-- against the row's own execution_id/generation/resource_project_id.
SELECT artifact_id, immutable_version, execution_id, generation,
    resource_project_id, media_type, byte_length, digest, classification,
    storage_record_id, bytes_verified_at
FROM elitea_runtime.index_result_artifacts
WHERE artifact_id = sqlc.arg(artifact_id)::text
  AND immutable_version = sqlc.arg(immutable_version)::text;
