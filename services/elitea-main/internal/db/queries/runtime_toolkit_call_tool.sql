-- toolkit.call_tool.v1 — the producer's durable half.
--
-- WHY THERE IS NO PER-CAPABILITY BINDING TABLE HERE. index.ingest.v1 and
-- agent.execute.*.v1 each own one (`index_ingest_jobs`, `agent_execution_jobs`)
-- because a BACKGROUND publisher rebuilds their command minutes later, from the
-- database alone. A tool run is admitted and dispatched inside ONE bounded
-- synchronous request, so the command scalars the worker needs — toolkit type,
-- tool name, toolkit id and version — never leave the process that built them
-- and never need a row. The two input entry ids are recovered from
-- `input_bundle_entries.semantic_role`, which is already durable.
--
-- What that costs, stated rather than hidden: a crash between admission and the
-- Redis append leaves an outbox row that nothing will ever publish. That is the
-- right answer for a synchronous run — the caller's request died with the
-- process, so "nothing ran" is true — but the row must not go on consuming
-- admission capacity, which is what
-- LockExpiredNoAuthorityToolkitCallToolExecutions below is for.
--
-- Every JOIN the agent queries make to `agent_execution_jobs` is replaced by the
-- capability predicate those queries ALREADY carry beside it. The join was
-- scoping, not data, on every one of them except the pending-dispatch load,
-- which this capability does not have.

-- name: GetToolkitCallToolAdmissionByIdempotency :one
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
  AND j.capability_id = 'toolkit.call_tool.v1';

-- name: InsertToolkitCallToolJob :one
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
    'toolkit.call_tool.v1',
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

-- name: LockToolkitCallToolEnvelope :one
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
  AND j.capability_id = 'toolkit.call_tool.v1'
  AND j.generation = 1
  AND j.desired_state = 'RUNNING'
FOR UPDATE OF j, o;

-- name: StorePreparedToolkitCallToolEnvelope :execrows
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

-- name: LockToolkitCallToolPublication :one
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
  AND j.capability_id = 'toolkit.call_tool.v1'
  AND j.generation = 1
  AND j.desired_state = 'RUNNING'
FOR UPDATE OF j, o;

-- name: MarkToolkitCallToolPublished :execrows
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

-- name: MarkToolkitCallToolDispatched :execrows
UPDATE elitea_runtime.execution_jobs
SET state = 'DISPATCHED'
WHERE execution_id = sqlc.arg(execution_id)::text
  AND generation = sqlc.arg(generation)::bigint
  AND capability_id = 'toolkit.call_tool.v1'
  AND desired_state = 'RUNNING'
  AND state = 'PENDING';

-- LockExpiredNoAuthorityToolkitCallToolExecutions reclaims the capacity a run
-- that never reached Redis would otherwise hold forever. It selects only work
-- past its deadline that no worker ever claimed, which is exactly the crash
-- window this capability accepts in exchange for having no binding table.
--
-- name: LockExpiredNoAuthorityToolkitCallToolExecutions :many
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
  AND j.capability_id = 'toolkit.call_tool.v1'
  AND j.generation = 1
  AND NOT EXISTS (
      SELECT 1
      FROM elitea_runtime.execution_claims AS c
      WHERE c.execution_id = j.execution_id AND c.generation = j.generation
  )
ORDER BY o.deadline, o.outbox_id
LIMIT sqlc.arg(batch_limit)::integer
FOR UPDATE OF j, o SKIP LOCKED;

-- GetToolkitCallToolInputEntries recovers the two entry ids the worker command
-- names, from the roles that are already durable. It is the reason this
-- capability needs no binding table of its own.
--
-- name: GetToolkitCallToolInputEntries :many
SELECT e.entry_id,
       e.semantic_role,
       e.entry_version,
       e.content_digest
FROM elitea_runtime.execution_jobs AS j
JOIN elitea_runtime.input_bundle_entries AS e
  ON e.input_bundle_id = j.input_bundle_id
WHERE j.execution_id = sqlc.arg(execution_id)::text
  AND j.generation = sqlc.arg(generation)::bigint
  AND j.capability_id = 'toolkit.call_tool.v1'
  AND e.semantic_role IN (
      'toolkit.call_tool.settings',
      'toolkit.call_tool.arguments'
  )
ORDER BY e.semantic_role;

-- GetExpectedToolkitCallToolHeader is the output plane's admitted-binding read.
-- Unlike its agent counterpart it needs no capability-owned table: everything
-- it returns is on execution_jobs, input_bundles and input_bundle_entries.
--
-- name: GetExpectedToolkitCallToolHeader :one
SELECT j.tenant_id,
       j.resource_project_id,
       j.projection_project_id,
       j.capability_id,
       j.command_id,
       j.execution_id,
       j.generation,
       b.input_bundle_id,
       b.manifest_digest AS input_bundle_digest
FROM elitea_runtime.execution_jobs AS j
JOIN elitea_runtime.input_bundles AS b
  ON b.input_bundle_id = j.input_bundle_id
WHERE j.execution_id = sqlc.arg(execution_id)::text
  AND j.generation = sqlc.arg(generation)::bigint
  AND j.capability_id = 'toolkit.call_tool.v1';
