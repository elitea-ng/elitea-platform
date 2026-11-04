-- I1/I5/I8: committed settlement(s) bound to a claim. Bound: :'exec_id'.
SELECT coalesce(json_agg(r ORDER BY r.generation), '[]'::json) FROM (
  SELECT s.generation, s.disposition, s.error_code, s.claim_attempt, s.lease_epoch, s.claim_id,
         s.terminal_event_id, s.terminal_sequence, encode(s.terminal_payload_digest, 'hex') AS terminal_payload_digest,
         s.prepared_at, s.committed_at
    FROM elitea_runtime.execution_settlements s
   WHERE s.execution_id = :'exec_id'
) r;
