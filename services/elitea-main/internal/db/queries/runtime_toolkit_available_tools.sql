
-- name: GetToolkitAvailableToolsAdmissionByIdempotency :one
SELECT j.execution_id,
       j.command_id,
       j.generation,
       j.request_digest,
       j.admitted_at,
       o.deadline
FROM elitea_runtime.execution_jobs AS j
JOIN elitea_runtime.command_outbox AS o
  ON o.execution_id = j.execution_id AND o.generation = j.generation
WHERE j.idempotency_scope = sqlc.arg(idempotency_scope)::text
  AND j.idempotency_key = sqlc.arg(idempotency_key)::text
  AND j.capability_id = 'toolkit.available_tools.v1';

-- name: InsertToolkitAvailableToolsJob :one
INSERT INTO elitea_runtime.execution_jobs (
    execution_id, generation, command_id, tenant_id, resource_project_id,
    projection_project_id, actor_id, principal_ref, capability_id,
    capability_version, input_bundle_id, request_digest,
    idempotency_scope, idempotency_key, state, desired_state, admitted_at
) VALUES (
    sqlc.arg(execution_id)::text,
    sqlc.arg(generation)::bigint,
    sqlc.arg(command_id)::text,
    sqlc.arg(tenant_id)::text,
    sqlc.arg(resource_project_id)::integer,
    sqlc.arg(projection_project_id)::integer,
    sqlc.arg(actor_id)::text,
    sqlc.arg(principal_ref)::text,
    'toolkit.available_tools.v1',
    sqlc.arg(capability_version)::text,
    sqlc.arg(input_bundle_id)::text,
    sqlc.arg(request_digest)::bytea,
    sqlc.arg(idempotency_scope)::text,
    sqlc.arg(idempotency_key)::text,
    sqlc.arg(state)::text,
    'RUNNING',
    sqlc.arg(admitted_at)::timestamptz
)
ON CONFLICT (idempotency_scope, idempotency_key) DO NOTHING
RETURNING execution_id;

-- name: LockToolkitAvailableToolsEnvelope :one
SELECT o.prepared_signed_envelope_bytes,
       o.prepared_signed_envelope_digest,
       COALESCE(o.prepared_signature_profile, 0) AS prepared_signature_profile,
       COALESCE(o.prepared_key_id, '') AS prepared_key_id,
       (o.published_at IS NOT NULL)::boolean AS published,
       (o.retired_at IS NOT NULL)::boolean AS retired,
       (o.deadline <= clock_timestamp())::boolean AS deadline_expired,
       (o.authority_granted_at IS NOT NULL)::boolean AS authority_granted,
       j.state
FROM elitea_runtime.execution_jobs AS j
JOIN elitea_runtime.command_outbox AS o
  ON o.execution_id = j.execution_id AND o.generation = j.generation
WHERE o.outbox_id = sqlc.arg(outbox_id)::text
  AND o.stream_name = sqlc.arg(stream_name)::text
  AND j.capability_id = 'toolkit.available_tools.v1'
  AND j.generation = 1
  AND j.desired_state = 'RUNNING'
FOR UPDATE OF j, o;

-- name: StorePreparedToolkitAvailableToolsEnvelope :execrows
UPDATE elitea_runtime.command_outbox
SET prepared_signed_envelope_bytes = sqlc.arg(envelope_bytes)::bytea,
    prepared_signed_envelope_digest = sqlc.arg(envelope_digest)::bytea,
    prepared_signature_profile = sqlc.arg(signature_profile)::integer,
    prepared_key_id = sqlc.arg(key_id)::text,
    prepared_at = clock_timestamp()
WHERE outbox_id = sqlc.arg(outbox_id)::text
  AND retired_at IS NULL
  AND authority_granted_at IS NULL
  AND published_at IS NULL
  AND prepared_signed_envelope_bytes IS NULL
  AND deadline > clock_timestamp();

-- name: LockToolkitAvailableToolsPublication :one
SELECT j.execution_id,
       j.generation,
       j.state,
       o.prepared_signed_envelope_digest,
       o.published_envelope_digest,
       (o.published_at IS NOT NULL)::boolean AS published,
       (o.retired_at IS NOT NULL)::boolean AS retired,
       (o.deadline <= clock_timestamp())::boolean AS deadline_expired,
       (o.authority_granted_at IS NOT NULL)::boolean AS authority_granted
FROM elitea_runtime.execution_jobs AS j
JOIN elitea_runtime.command_outbox AS o
  ON o.execution_id = j.execution_id AND o.generation = j.generation
WHERE o.outbox_id = sqlc.arg(outbox_id)::text
  AND o.stream_name = sqlc.arg(stream_name)::text
  AND j.capability_id = 'toolkit.available_tools.v1'
  AND j.generation = 1
  AND j.desired_state = 'RUNNING'
FOR UPDATE OF j, o;

-- name: MarkToolkitAvailableToolsPublished :execrows
UPDATE elitea_runtime.command_outbox
SET published_at = clock_timestamp(),
    published_envelope_digest = sqlc.arg(envelope_digest)::bytea,
    publish_attempts = publish_attempts + 1,
    last_error_code = NULL,
    last_visibility_at = clock_timestamp()
WHERE outbox_id = sqlc.arg(outbox_id)::text
  AND published_at IS NULL
  AND retired_at IS NULL
  AND authority_granted_at IS NULL
  AND deadline > clock_timestamp()
  AND prepared_signed_envelope_digest = sqlc.arg(envelope_digest)::bytea;

-- name: MarkToolkitAvailableToolsDispatched :execrows
UPDATE elitea_runtime.execution_jobs
SET state = 'DISPATCHED'
WHERE execution_id = sqlc.arg(execution_id)::text
  AND generation = sqlc.arg(generation)::bigint
  AND capability_id = 'toolkit.available_tools.v1'
  AND desired_state = 'RUNNING'
  AND state = 'PENDING';

-- name: LockExpiredNoAuthorityToolkitAvailableToolsExecutions :many
SELECT o.outbox_id,
       j.execution_id,
       j.generation,
       j.projection_project_id,
       j.desired_state
FROM elitea_runtime.command_outbox AS o
JOIN elitea_runtime.execution_jobs AS j
  ON j.execution_id = o.execution_id AND j.generation = o.generation
WHERE o.deadline <= clock_timestamp()
  AND o.retired_at IS NULL
  AND o.authority_granted_at IS NULL
  AND o.stream_name = sqlc.arg(stream_name)::text
  AND j.desired_state <> 'CANCELLED'
  AND j.state IN ('PENDING', 'DISPATCHED')
  AND j.capability_id = 'toolkit.available_tools.v1'
  AND j.generation = 1
  AND NOT EXISTS (
      SELECT 1
      FROM elitea_runtime.execution_claims AS c
      WHERE c.execution_id = j.execution_id AND c.generation = j.generation
  )
ORDER BY o.deadline, o.outbox_id
LIMIT sqlc.arg(batch_limit)::integer
FOR UPDATE OF j, o SKIP LOCKED;

-- name: GetToolkitAvailableToolsInputEntries :many
SELECT e.entry_id,
       e.semantic_role,
       e.entry_version,
       e.content_digest
FROM elitea_runtime.execution_jobs AS j
JOIN elitea_runtime.input_bundle_entries AS e
  ON e.input_bundle_id = j.input_bundle_id
WHERE j.execution_id = sqlc.arg(execution_id)::text
  AND j.generation = sqlc.arg(generation)::bigint
  AND j.capability_id = 'toolkit.available_tools.v1'
ORDER BY e.semantic_role;

-- name: GetExpectedToolkitAvailableToolsHeader :one
SELECT j.tenant_id,
       j.resource_project_id,
       j.projection_project_id,
       j.capability_id,
       o.prepared_signed_envelope_bytes,
       o.prepared_signed_envelope_digest,
       j.command_id,
       j.execution_id,
       j.generation,
       b.input_bundle_id,
       b.manifest_digest AS input_bundle_digest
FROM elitea_runtime.execution_jobs AS j
JOIN elitea_runtime.command_outbox AS o ON o.execution_id=j.execution_id AND o.generation=j.generation
JOIN elitea_runtime.input_bundles AS b
  ON b.input_bundle_id = j.input_bundle_id
WHERE j.execution_id = sqlc.arg(execution_id)::text
  AND j.generation = sqlc.arg(generation)::bigint
  AND j.capability_id = 'toolkit.available_tools.v1';

-- name: GetToolkitAvailableToolsArtifactForOutput :one
SELECT a.artifact_id,a.immutable_version,j.input_bundle_id,
       e.entry_id AS settings_entry_id,e.entry_version AS settings_entry_version,
       e.content_digest AS settings_content_digest,a.digest,a.byte_length,
       a.media_type,a.classification,a.bytes_verified_at
FROM elitea_runtime.index_result_artifacts AS a
JOIN elitea_runtime.execution_jobs AS j
  ON j.execution_id=a.execution_id AND j.generation=a.generation
  AND j.resource_project_id=a.resource_project_id
JOIN elitea_runtime.input_bundle_entries AS e
  ON e.input_bundle_id=j.input_bundle_id
  AND e.semantic_role='toolkit.available_tools.settings'
WHERE a.execution_id=sqlc.arg(execution_id)::text
  AND a.generation=sqlc.arg(generation)::bigint
  AND a.artifact_id=sqlc.arg(artifact_id)::text
  AND a.immutable_version=sqlc.arg(immutable_version)::text
  AND j.capability_id='toolkit.available_tools.v1';
