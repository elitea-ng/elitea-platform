-- I5: claim history. Attempts must be contiguous, epochs strictly increasing. Bound: :'exec_id'.
SELECT coalesce(json_agg(r ORDER BY r.generation, r.claim_attempt), '[]'::json) FROM (
  SELECT c.claim_id, c.generation, c.claim_attempt, c.lease_epoch, c.workload_identity, c.workload_session_id,
         c.recovery_mode, c.claimed_at, c.lease_expires_at, c.released_at, c.release_reason
    FROM elitea_runtime.execution_claims c
   WHERE c.execution_id = :'exec_id'
) r;
