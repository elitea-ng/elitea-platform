-- I4/I5: checkpoint writer identity, and checkpoints or session events written by a superseded claim
-- after the takeover. Bound: :'exec_id', :'last_claim_id' and :'t_takeover' (empty when no takeover).
SELECT json_build_object(
  'writers', coalesce((SELECT json_agg(json_build_object('thread_id_sha256', encode(sha256(convert_to(w.thread_id, 'UTF8')), 'hex'),
      'definition_digest', encode(w.definition_digest, 'hex'), 'checkpoint_family', w.checkpoint_family,
      'writer_claim_attempt', w.writer_claim_attempt, 'writer_lease_epoch', w.writer_lease_epoch,
      'writer_generation', w.writer_generation, 'writer_claimed_at', w.writer_claimed_at))
    FROM elitea_runtime.agent_graph_checkpoint_writers w WHERE w.writer_execution_id = :'exec_id'), '[]'::json),
  'checkpoints', (SELECT count(*) FROM elitea_runtime.agent_graph_checkpoints c WHERE c.writer_execution_id = :'exec_id'),
  'checkpoints_by_attempt', coalesce((SELECT json_object_agg(a, n) FROM (
      SELECT c.writer_claim_attempt AS a, count(*) AS n FROM elitea_runtime.agent_graph_checkpoints c
       WHERE c.writer_execution_id = :'exec_id' GROUP BY 1) t), '{}'::json),
  'stale_checkpoints_after_takeover', (SELECT count(*) FROM elitea_runtime.agent_graph_checkpoints c
      WHERE c.writer_execution_id = :'exec_id' AND nullif(:'last_claim_id', '') IS NOT NULL
        AND c.writer_claim_id::text <> :'last_claim_id'
        AND c.stored_at > nullif(:'t_takeover', '')::timestamptz),
  'stale_session_events_after_takeover', (SELECT count(*) FROM elitea_runtime.agent_session_events e
      WHERE e.writer_execution_id = :'exec_id' AND nullif(:'last_claim_id', '') IS NOT NULL
        AND e.writer_claim_id::text <> :'last_claim_id'
        AND e.stored_at > nullif(:'t_takeover', '')::timestamptz));
