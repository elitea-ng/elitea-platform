-- I5/I1: accepted outputs per claim, and outputs a superseded claim delivered after the takeover. Bound: :'exec_id'.
WITH last_claim AS (
  SELECT claim_attempt, claimed_at FROM elitea_runtime.execution_claims
   WHERE execution_id = :'exec_id' ORDER BY generation DESC, claim_attempt DESC LIMIT 1)
SELECT json_build_object(
  'by_claim', coalesce((SELECT json_agg(t ORDER BY t.claim_attempt, t.payload_type) FROM (
      SELECT o.claim_attempt, o.payload_type, count(*) AS outputs, min(o.received_at) AS first_at,
             max(o.received_at) AS last_at, count(*) FILTER (WHERE o.projected_at IS NOT NULL) AS projected
        FROM elitea_runtime.output_inbox o WHERE o.execution_id = :'exec_id' GROUP BY 1, 2) t), '[]'::json),
  'terminal_outputs', (SELECT count(*) FROM elitea_runtime.output_inbox o
      WHERE o.execution_id = :'exec_id' AND o.payload_type IN ('AGENT_EXECUTION_RESULT', 'RUNTIME_FAILURE')),
  'stale_after_takeover', (SELECT count(*) FROM elitea_runtime.output_inbox o, last_claim l
      WHERE o.execution_id = :'exec_id' AND o.claim_attempt < l.claim_attempt AND o.received_at > l.claimed_at));
