-- I1/I8/I10: replay log summary. Terminal agent success is `execution.node_event` whose JSON `type` is
-- `full_message` (repos/agent_execution_results.go); failure and cancellation are `execution.failed`
-- (repos/configuration_validation_results.go:26). Only codes and safe messages are read, never answer text.
-- Bound: :'exec_id'.
WITH ev AS (
  SELECT e.cursor, e.event_type, e.created_at,
         CASE WHEN e.event_type = 'execution.node_event'
              THEN convert_from(e.event_bytes, 'UTF8')::jsonb ->> 'type' END AS node_type,
         CASE WHEN e.event_type = 'execution.failed'
              THEN convert_from(e.event_bytes, 'UTF8')::jsonb END AS failure
    FROM elitea_runtime.execution_replay_events e
   WHERE e.execution_id = :'exec_id'
)
SELECT json_build_object(
  'events', (SELECT count(*) FROM ev),
  'first_at', (SELECT min(created_at) FROM ev),
  'last_at', (SELECT max(created_at) FROM ev),
  'by_type', coalesce((SELECT json_object_agg(k, n) FROM (
      SELECT coalesce(event_type || ':' || node_type, event_type) AS k, count(*) AS n FROM ev GROUP BY 1) t), '{}'::json),
  'terminal', coalesce((SELECT json_agg(json_build_object(
      'cursor', cursor, 'event_type', event_type, 'node_type', node_type, 'at', created_at,
      'code', failure ->> 'code', 'safe_message', failure ->> 'safe_message',
      'retryable', failure -> 'retryable') ORDER BY cursor)
    FROM ev WHERE event_type = 'execution.failed' OR node_type = 'full_message'), '[]'::json),
  'first_progress_after_fault', (SELECT min(created_at) FROM ev
    WHERE created_at > nullif(:'t_fault', '')::timestamptz));
