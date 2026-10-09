-- Verdict C: node-recovery reconcile evidence (migration 0146). Bound: :'exec_id'.
SELECT json_build_object(
  'visits', coalesce((SELECT json_agg(json_build_object('activation_id', v.activation_id, 'node_id', v.node_id,
      'attempt', v.attempt, 'status', v.status, 'journal_revision', v.journal_revision))
    FROM elitea_runtime.node_recovery_visits v WHERE v.execution_id = :'exec_id'), '[]'::json),
  'audit', coalesce((SELECT json_agg(json_build_object('transition', a.transition, 'activation_id', a.activation_id,
      'at', a.created_at)) FROM elitea_runtime.node_recovery_audit a WHERE a.execution_id = :'exec_id'), '[]'::json));
