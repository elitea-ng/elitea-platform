-- Inspect the old Rust rehearsal before the main integration cutover.
-- Run with psql -X -v ON_ERROR_STOP=1. This script changes no durable state.
-- A zero psql exit means the inspection succeeded, not that cutover is safe.
BEGIN READ ONLY;
SET LOCAL statement_timeout = '10s';
SET LOCAL lock_timeout = '2s';

SELECT target_kind, target_id, version, name, encode(checksum, 'hex') AS checksum
FROM elitea_runtime.schema_migrations
WHERE target_kind = 'shared' AND target_id = 'platform' AND version >= 110
ORDER BY version
LIMIT 32;

SELECT capability_id, state, count(*) AS executions
FROM elitea_runtime.execution_jobs
GROUP BY capability_id, state
ORDER BY capability_id, state;

SELECT j.capability_id,
       count(*) FILTER (WHERE o.retired_at IS NULL AND s.committed_at IS NULL) AS unsettled_outbox,
       count(*) FILTER (WHERE o.prepared_at IS NOT NULL) AS prepared_commands,
       count(*) FILTER (WHERE s.committed_at IS NOT NULL) AS settled_commands
FROM elitea_runtime.command_outbox o
JOIN elitea_runtime.execution_jobs j USING (execution_id, generation)
LEFT JOIN elitea_runtime.execution_settlements s USING (execution_id, generation)
GROUP BY j.capability_id
ORDER BY j.capability_id;

-- Limit details to the oldest unresolved commands with the conflicting wire type.
-- Do not print signed bytes, input bundles, output payloads, or fence tokens.
SELECT j.execution_id, j.generation, j.state, j.desired_state, j.invocation_state,
       o.deadline, o.deadline <= clock_timestamp() AS deadline_expired,
       c.claim_attempt, c.lease_expires_at, c.released_at
FROM elitea_runtime.execution_jobs j
JOIN elitea_runtime.command_outbox o USING (execution_id, generation)
LEFT JOIN elitea_runtime.execution_settlements s USING (execution_id, generation)
LEFT JOIN LATERAL (
    SELECT claim_attempt, lease_expires_at, released_at
    FROM elitea_runtime.execution_claims c
    WHERE c.execution_id = j.execution_id AND c.generation = j.generation
    ORDER BY claim_attempt DESC
    LIMIT 1
) c ON true
WHERE j.capability_id = 'toolkit.execute.read.v1'
  AND o.retired_at IS NULL AND s.committed_at IS NULL
ORDER BY j.admitted_at, j.execution_id, j.generation
LIMIT 20;

SELECT payload_type, count(*) AS outputs,
       count(*) FILTER (WHERE projected_at IS NULL) AS unprojected
FROM elitea_runtime.output_inbox
GROUP BY payload_type
ORDER BY payload_type;

ROLLBACK;
