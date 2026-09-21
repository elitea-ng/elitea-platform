WITH scoped AS (
SELECT event_ordinal, event_payload, event_timestamp, payload_bytes
FROM elitea_runtime.agent_session_events
WHERE tenant_id = $1
  AND resource_project_id = $2
  AND projection_project_id = $3
  AND capability_id = $4
  AND session_family = $5
  AND definition_digest = $6
  AND thread_id = $7
  AND app_name = $8
  AND user_id = $9
  AND session_id = $10
), classified AS MATERIALIZED (
 SELECT event_ordinal, event_payload, event_timestamp, payload_bytes, COALESCE(
   fields.author = 'elitea-recovery'
   AND COALESCE(fields.content, 'null'::jsonb) = 'null'::jsonb
   AND (fields.actions->'state_delta') ? 'elitea.agent.recovery.v1'
   AND COALESCE(fields.actions->'artifact_delta', '{}'::jsonb) = '{}'::jsonb
   AND COALESCE(fields.actions->'transfer_to_agent', 'null'::jsonb) = 'null'::jsonb
   AND COALESCE(fields.actions->'escalate', 'false'::jsonb) = 'false'::jsonb
   AND COALESCE(fields.actions->'tool_confirmation', 'null'::jsonb) = 'null'::jsonb
   AND COALESCE(fields.actions->'tool_confirmation_decision', 'null'::jsonb) = 'null'::jsonb
   AND COALESCE(fields.actions->'compaction', 'null'::jsonb) = 'null'::jsonb
   AND COALESCE(fields.actions->'route', 'null'::jsonb) = 'null'::jsonb
   AND COALESCE(fields.long_running_tool_ids, '[]'::jsonb) = '[]'::jsonb,
 false) AS recovery_marker,
 COALESCE(fields.branch, '') AS branch
 FROM scoped CROSS JOIN LATERAL jsonb_to_record(event_payload::jsonb) AS fields(
   author text, content jsonb, actions jsonb, long_running_tool_ids jsonb, branch text
 )
), ranked AS (
 SELECT *, max(event_ordinal) FILTER (WHERE recovery_marker) OVER (PARTITION BY branch) AS latest_marker
 FROM classified
), active_events AS (
 SELECT * FROM ranked WHERE NOT recovery_marker OR event_ordinal = latest_marker
)
