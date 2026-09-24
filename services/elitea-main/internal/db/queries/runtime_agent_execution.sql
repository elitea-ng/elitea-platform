-- name: GetAgentExecutionAdmissionByIdempotency :one
SELECT j.execution_id,
       j.command_id,
       j.generation,
       j.request_digest,
       j.admitted_at,
       o.deadline
FROM elitea_runtime.execution_jobs AS j
JOIN elitea_runtime.command_outbox AS o
  ON o.execution_id = j.execution_id AND o.generation = j.generation
JOIN elitea_runtime.agent_execution_jobs AS a
  ON a.execution_id = j.execution_id AND a.generation = j.generation
WHERE j.idempotency_scope = sqlc.arg(idempotency_scope)::text
  AND j.idempotency_key = sqlc.arg(idempotency_key)::text
  AND j.capability_id IN (
      'agent.execute.application.v1',
      'agent.execute.adhoc.v1'
  );

-- name: InsertAgentExecutionJob :one
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
    sqlc.arg(capability_id)::text,
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

-- name: InsertAgentExecutionBinding :exec
INSERT INTO elitea_runtime.agent_execution_jobs (
    execution_id, generation, capability_id, input_bundle_id,
    request_entry_id, client_stream_id, client_message_id,
    client_execution_generation, sio_event
) VALUES (
    sqlc.arg(execution_id)::text,
    sqlc.arg(generation)::bigint,
    sqlc.arg(capability_id)::text,
    sqlc.arg(input_bundle_id)::text,
    sqlc.arg(request_entry_id)::text,
    sqlc.arg(client_stream_id)::text,
    sqlc.arg(client_message_id)::text,
    sqlc.arg(client_execution_generation)::text,
    sqlc.arg(sio_event)::text
);

-- name: ReserveAgentAdmission :one
INSERT INTO elitea_runtime.agent_admission_reservations (
    capability_id, idempotency_scope, idempotency_key,
    execution_id, configured_max
) VALUES (
    sqlc.arg(capability_id)::text,
    sqlc.arg(idempotency_scope)::text,
    sqlc.arg(idempotency_key)::text,
    sqlc.arg(execution_id)::text,
    sqlc.arg(configured_max)::bigint
)
ON CONFLICT (capability_id, idempotency_scope, idempotency_key) DO NOTHING
RETURNING execution_id;

-- name: MarkAgentAdmissionMaterialized :execrows
UPDATE elitea_runtime.agent_admission_reservations
SET materialized_at = clock_timestamp()
WHERE capability_id = sqlc.arg(capability_id)::text
  AND idempotency_scope = sqlc.arg(idempotency_scope)::text
  AND idempotency_key = sqlc.arg(idempotency_key)::text
  AND materialized_at IS NULL;

-- name: ReleaseAgentAdmissionReservation :execrows
-- Compensates a reserve whose materialize did not commit a new job (an error,
-- a cancelled request, or a replay resolved against the durable job). Only an
-- unmaterialized row is released, so this is a no-op after a successful
-- materialize and safe to run on every non-created outcome.
DELETE FROM elitea_runtime.agent_admission_reservations
WHERE capability_id = sqlc.arg(capability_id)::text
  AND idempotency_scope = sqlc.arg(idempotency_scope)::text
  AND idempotency_key = sqlc.arg(idempotency_key)::text
  AND materialized_at IS NULL;

-- name: CountAgentAdmissionSlots :one
-- The live cap the reservation guard trigger computes, in ONE statement so both
-- counts share a snapshot. Used by materialize to re-check the cap under the
-- policy row lock when its reservation was reaped before it committed.
SELECT ((
    SELECT count(*)
    FROM elitea_runtime.execution_jobs
    WHERE capability_id = sqlc.arg(capability_id)::text
      AND state IN ('PENDING', 'DISPATCHED', 'CLAIMED', 'RUNNING', 'SETTLING')
) + (
    SELECT count(*)
    FROM elitea_runtime.agent_admission_reservations
    WHERE capability_id = sqlc.arg(capability_id)::text
      AND materialized_at IS NULL
))::bigint AS slots;

-- now() (STABLE) rather than clock_timestamp() (VOLATILE): the planner can only
-- turn a STABLE cutoff into an index condition, and the DELETE is a single
-- statement, so the two are the same instant here. With clock_timestamp() the
-- planner ignored both partial indexes and heap-scanned the table every pass.
-- name: ReapAgentAdmissionReservations :execrows
DELETE FROM elitea_runtime.agent_admission_reservations
WHERE (
    materialized_at IS NULL
    AND reserved_at < now()
        - (sqlc.arg(stale_seconds)::bigint * interval '1 second')
)
   OR (
    materialized_at IS NOT NULL
    AND materialized_at < now()
        - (sqlc.arg(gc_seconds)::bigint * interval '1 second')
);

-- name: GetExpectedAgentExecutionHeader :one
SELECT j.tenant_id,
       j.resource_project_id,
       j.projection_project_id,
       j.capability_id,
       j.command_id,
       j.execution_id,
       j.generation,
       b.input_bundle_id,
       b.manifest_digest AS input_bundle_digest,
       a.request_entry_id,
       e.entry_version AS request_immutable_version,
       e.content_digest AS request_content_digest,
       a.client_stream_id,
       a.client_message_id,
       a.client_execution_generation,
       a.sio_event,
       o.limits_revision
FROM elitea_runtime.execution_jobs AS j
JOIN elitea_runtime.agent_execution_jobs AS a
  ON a.execution_id = j.execution_id
 AND a.generation = j.generation
JOIN elitea_runtime.input_bundles AS b
  ON b.input_bundle_id = j.input_bundle_id
JOIN elitea_runtime.input_bundle_entries AS e
  ON e.input_bundle_id = b.input_bundle_id
 AND e.entry_id = a.request_entry_id
JOIN elitea_runtime.command_outbox AS o
  ON o.execution_id = j.execution_id
 AND o.generation = j.generation
WHERE j.execution_id = sqlc.arg(execution_id)::text
  AND j.generation = sqlc.arg(generation)::bigint
  AND j.capability_id IN (
      'agent.execute.application.v1',
      'agent.execute.adhoc.v1'
  );

-- name: GetAgentExecutionTerminalNodeEvent :one
SELECT last_node_cursor,
       last_node_sequence,
       last_node_event_bytes,
       last_node_event_digest
FROM elitea_runtime.execution_replay_state
WHERE execution_id = sqlc.arg(execution_id)::text
  AND generation = sqlc.arg(generation)::bigint
  AND projection_project_id = sqlc.arg(projection_project_id)::integer
FOR UPDATE;

-- name: GetCurrentAgentTraceBinding :one
SELECT a.client_stream_id,
       a.client_message_id,
       a.client_execution_generation,
       a.sio_event
FROM elitea_runtime.execution_jobs AS j
JOIN elitea_runtime.agent_execution_jobs AS a
  ON a.execution_id = j.execution_id
 AND a.generation = j.generation
 AND a.capability_id = j.capability_id
WHERE j.execution_id = sqlc.arg(execution_id)::text
  AND j.generation = sqlc.arg(generation)::bigint
  AND j.resource_project_id = sqlc.arg(project_id)::integer
  AND j.projection_project_id = sqlc.arg(project_id)::integer
  AND j.capability_id IN (
      'agent.execute.application.v1',
      'agent.execute.adhoc.v1'
  );

-- name: GetPendingAgentExecutionDispatch :one
SELECT o.outbox_id,
       j.command_id,
       j.execution_id,
       j.generation,
       o.dispatch_ordinal,
       j.tenant_id,
       j.resource_project_id,
       j.projection_project_id,
       j.principal_ref,
       b.input_bundle_id,
       b.immutable_version,
       b.media_type,
       b.manifest_size,
       b.manifest_digest,
       j.capability_id,
       j.capability_version,
       o.resource_class,
       o.isolation_class,
       o.priority,
       o.deadline,
       o.limits_revision,
       o.traceparent,
       o.tracestate,
       a.request_entry_id,
       a.client_stream_id,
       a.client_message_id,
       a.sio_event
FROM elitea_runtime.command_outbox AS o
JOIN elitea_runtime.execution_jobs AS j
  ON j.execution_id = o.execution_id AND j.generation = o.generation
JOIN elitea_runtime.agent_execution_jobs AS a
  ON a.execution_id = j.execution_id
 AND a.generation = j.generation
 AND a.capability_id = j.capability_id
 AND a.input_bundle_id = j.input_bundle_id
JOIN elitea_runtime.input_bundles AS b
  ON b.input_bundle_id = a.input_bundle_id
WHERE o.outbox_id = sqlc.arg(outbox_id)::text
  AND o.stream_name = sqlc.arg(stream_name)::text
  AND o.published_at IS NULL
  AND o.retired_at IS NULL
  AND o.authority_granted_at IS NULL
  AND o.deadline > clock_timestamp()
  AND j.state = 'PENDING'
  AND j.desired_state = 'RUNNING'
  AND j.capability_id IN (
      'agent.execute.application.v1',
      'agent.execute.adhoc.v1'
  )
  AND j.generation = 1;

-- name: GetPreparedAgentExecutionEnvelope :one
SELECT o.prepared_signed_envelope_bytes,
       o.prepared_signed_envelope_digest,
       COALESCE(o.prepared_signature_profile, 0) AS prepared_signature_profile,
       COALESCE(o.prepared_key_id, '') AS prepared_key_id,
       (o.published_at IS NOT NULL)::boolean AS published,
       (o.retired_at IS NOT NULL)::boolean AS retired,
       (o.deadline <= clock_timestamp() AND o.authority_granted_at IS NULL)::boolean AS deadline_expired,
       (o.authority_granted_at IS NOT NULL)::boolean AS authority_granted,
       j.state
FROM elitea_runtime.command_outbox AS o
JOIN elitea_runtime.execution_jobs AS j
  ON j.execution_id = o.execution_id AND j.generation = o.generation
JOIN elitea_runtime.agent_execution_jobs AS a
  ON a.execution_id = j.execution_id AND a.generation = j.generation
WHERE o.outbox_id = sqlc.arg(outbox_id)::text
  AND o.stream_name = sqlc.arg(stream_name)::text
  AND j.capability_id IN (
      'agent.execute.application.v1',
      'agent.execute.adhoc.v1'
  )
  AND j.generation = 1
  AND j.desired_state = 'RUNNING';

-- name: LockAgentExecutionEnvelope :one
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
JOIN elitea_runtime.agent_execution_jobs AS a
  ON a.execution_id = j.execution_id AND a.generation = j.generation
WHERE o.outbox_id = sqlc.arg(outbox_id)::text
  AND o.stream_name = sqlc.arg(stream_name)::text
  AND j.capability_id IN (
      'agent.execute.application.v1',
      'agent.execute.adhoc.v1'
  )
  AND j.generation = 1
  AND j.desired_state = 'RUNNING'
FOR UPDATE OF j, o;

-- name: StorePreparedAgentExecutionEnvelope :execrows
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

-- name: LockAgentExecutionPublication :one
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
JOIN elitea_runtime.agent_execution_jobs AS a
  ON a.execution_id = j.execution_id AND a.generation = j.generation
WHERE o.outbox_id = sqlc.arg(outbox_id)::text
  AND o.stream_name = sqlc.arg(stream_name)::text
  AND j.capability_id IN (
      'agent.execute.application.v1',
      'agent.execute.adhoc.v1'
  )
  AND j.generation = 1
  AND j.desired_state = 'RUNNING'
FOR UPDATE OF j, o;

-- name: RefreshAgentExecutionPublication :execrows
UPDATE elitea_runtime.command_outbox
SET last_visibility_at = clock_timestamp(),
    publish_attempts = publish_attempts + 1,
    last_error_code = NULL
WHERE outbox_id = sqlc.arg(outbox_id)::text
  AND published_at IS NOT NULL
  AND retired_at IS NULL
  AND authority_granted_at IS NULL
  AND deadline > clock_timestamp()
  AND prepared_signed_envelope_digest = sqlc.arg(envelope_digest)::bytea
  AND published_envelope_digest = sqlc.arg(envelope_digest)::bytea;

-- name: MarkAgentExecutionPublished :execrows
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

-- name: MarkAgentExecutionDispatched :execrows
UPDATE elitea_runtime.execution_jobs
SET state = 'DISPATCHED'
WHERE execution_id = sqlc.arg(execution_id)::text
  AND generation = sqlc.arg(generation)::bigint
  AND capability_id IN (
      'agent.execute.application.v1',
      'agent.execute.adhoc.v1'
  )
  AND desired_state = 'RUNNING'
  AND state = 'PENDING';

-- name: ListPendingAgentExecutionIDs :many
SELECT candidate.outbox_id
FROM (
    (
        SELECT o.outbox_id, o.created_at AS visibility_order
        FROM elitea_runtime.command_outbox AS o
        JOIN elitea_runtime.execution_jobs AS j
          ON j.execution_id = o.execution_id AND j.generation = o.generation
        JOIN elitea_runtime.agent_execution_jobs AS a
          ON a.execution_id = j.execution_id AND a.generation = j.generation
        WHERE o.stream_name = sqlc.arg(stream_name)::text
          AND o.published_at IS NULL
          AND o.retired_at IS NULL
          AND o.authority_granted_at IS NULL
          AND o.deadline > statement_timestamp()
          AND j.state = 'PENDING'
          AND j.desired_state = 'RUNNING'
          AND j.capability_id IN (
              'agent.execute.application.v1',
              'agent.execute.adhoc.v1'
          )
          AND j.generation = 1
        ORDER BY o.created_at, o.outbox_id
        LIMIT sqlc.arg(batch_limit)::integer
    )
    UNION ALL
    (
        SELECT o.outbox_id,
               COALESCE(o.last_visibility_at, o.published_at) AS visibility_order
        FROM elitea_runtime.command_outbox AS o
        JOIN elitea_runtime.execution_jobs AS j
          ON j.execution_id = o.execution_id AND j.generation = o.generation
        JOIN elitea_runtime.agent_execution_jobs AS a
          ON a.execution_id = j.execution_id AND a.generation = j.generation
        WHERE o.stream_name = sqlc.arg(stream_name)::text
          AND o.published_at IS NOT NULL
          AND o.retired_at IS NULL
          AND o.authority_granted_at IS NULL
          AND o.deadline > statement_timestamp()
          AND COALESCE(o.last_visibility_at, o.published_at)
              <= statement_timestamp()
                 - (sqlc.arg(visibility_millis)::bigint * interval '1 millisecond')
          AND j.state IN ('PENDING', 'DISPATCHED')
          AND j.desired_state = 'RUNNING'
          AND j.capability_id IN (
              'agent.execute.application.v1',
              'agent.execute.adhoc.v1'
          )
          AND j.generation = 1
        ORDER BY COALESCE(o.last_visibility_at, o.published_at), o.outbox_id
        LIMIT sqlc.arg(batch_limit)::integer
    )
) AS candidate
ORDER BY candidate.visibility_order, candidate.outbox_id
LIMIT sqlc.arg(batch_limit)::integer;

-- name: LockCancelledNoAuthorityAgentExecutions :many
SELECT o.outbox_id,
       j.execution_id,
       j.generation,
       j.projection_project_id,
       j.desired_state
FROM elitea_runtime.execution_jobs AS j
JOIN elitea_runtime.command_outbox AS o
  ON o.execution_id = j.execution_id AND o.generation = j.generation
JOIN elitea_runtime.agent_execution_jobs AS a
  ON a.execution_id = j.execution_id AND a.generation = j.generation
WHERE j.desired_state = 'CANCELLED'
  AND j.state IN ('PENDING', 'DISPATCHED')
  AND j.capability_id IN (
      'agent.execute.application.v1',
      'agent.execute.adhoc.v1'
  )
  AND j.generation = 1
  AND o.stream_name = sqlc.arg(stream_name)::text
  AND o.retired_at IS NULL
  AND o.authority_granted_at IS NULL
  AND NOT EXISTS (
      SELECT 1
      FROM elitea_runtime.execution_claims AS c
      WHERE c.execution_id = j.execution_id AND c.generation = j.generation
  )
ORDER BY j.admitted_at, j.execution_id, j.generation
LIMIT sqlc.arg(batch_limit)::integer
FOR UPDATE OF j, o SKIP LOCKED;

-- name: LockExpiredNoAuthorityAgentExecutions :many
SELECT o.outbox_id,
       j.execution_id,
       j.generation,
       j.projection_project_id,
       j.desired_state
FROM elitea_runtime.command_outbox AS o
JOIN elitea_runtime.execution_jobs AS j
  ON j.execution_id = o.execution_id AND j.generation = o.generation
JOIN elitea_runtime.agent_execution_jobs AS a
  ON a.execution_id = j.execution_id AND a.generation = j.generation
WHERE o.deadline <= clock_timestamp()
  AND o.retired_at IS NULL
  AND o.authority_granted_at IS NULL
  AND o.stream_name = sqlc.arg(stream_name)::text
  AND j.desired_state <> 'CANCELLED'
  AND j.state IN ('PENDING', 'DISPATCHED')
  AND j.capability_id IN (
      'agent.execute.application.v1',
      'agent.execute.adhoc.v1'
  )
  AND j.generation = 1
  AND NOT EXISTS (
      SELECT 1
      FROM elitea_runtime.execution_claims AS c
      WHERE c.execution_id = j.execution_id AND c.generation = j.generation
  )
ORDER BY o.deadline, o.outbox_id
LIMIT sqlc.arg(batch_limit)::integer
FOR UPDATE OF j, o SKIP LOCKED;
