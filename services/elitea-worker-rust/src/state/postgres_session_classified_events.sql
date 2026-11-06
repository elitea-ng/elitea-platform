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
  AND event_ordinal > {ORDINAL_AFTER}
), classified AS MATERIALIZED (
 SELECT event_ordinal, event_payload, event_timestamp, payload_bytes, fields.author, COALESCE(
   fields.author = 'elitea-recovery'
   AND COALESCE(fields.content, 'null'::jsonb) = 'null'::jsonb
   AND (fields.actions->'state_delta') ? 'elitea.agent.recovery.v1'
   AND COALESCE(fields.actions->'artifact_delta', '{}'::jsonb) = '{}'::jsonb
   AND COALESCE(fields.actions->'transfer_to_agent', 'null'::jsonb) = 'null'::jsonb
   AND COALESCE(fields.actions->'escalate', 'false'::jsonb) = 'false'::jsonb
   AND COALESCE(fields.actions->'skip_summarization', 'false'::jsonb) = 'false'::jsonb
   AND COALESCE(fields.actions->'tool_confirmation', 'null'::jsonb) = 'null'::jsonb
   AND COALESCE(fields.actions->'tool_confirmation_decision', 'null'::jsonb) = 'null'::jsonb
   AND COALESCE(fields.actions->'compaction', 'null'::jsonb) = 'null'::jsonb
   AND COALESCE(fields.actions->'route', 'null'::jsonb) = 'null'::jsonb
   AND COALESCE(fields.long_running_tool_ids, '[]'::jsonb) = '[]'::jsonb,
 false) AS recovery_marker,
 COALESCE(fields.branch, '') AS branch,
 CASE WHEN fields.actions#>>'{state_delta,elitea.agent.history_snapshot.v1,version}' = '1'
   AND fields.actions#>>'{state_delta,elitea.agent.recovery.v1,phase}' = 'model_pending'
   AND jsonb_typeof(fields.actions#>'{state_delta,elitea.agent.recovery.v1,model,request,contents}') = 'array'
 THEN NULLIF(fields.actions#>>'{state_delta,elitea.agent.history_snapshot.v1,agent_name}', '') END AS snapshot_agent,
 (COALESCE(fields.actions->'state_delta', '{}'::jsonb) <> '{}'::jsonb
  OR COALESCE(fields.actions->'artifact_delta', '{}'::jsonb) <> '{}'::jsonb
  OR COALESCE(fields.actions->'transfer_to_agent', 'null'::jsonb) <> 'null'::jsonb
  OR COALESCE(fields.actions->'escalate', 'false'::jsonb) <> 'false'::jsonb
  OR COALESCE(fields.actions->'skip_summarization', 'false'::jsonb) <> 'false'::jsonb
  OR COALESCE(fields.actions->'tool_confirmation', 'null'::jsonb) <> 'null'::jsonb
  OR COALESCE(fields.actions->'tool_confirmation_decision', 'null'::jsonb) <> 'null'::jsonb
  OR COALESCE(fields.actions->'compaction', 'null'::jsonb) <> 'null'::jsonb
  OR COALESCE(fields.actions->'route', 'null'::jsonb) <> 'null'::jsonb
  OR COALESCE(fields.long_running_tool_ids, '[]'::jsonb) <> '[]'::jsonb) AS has_control
 FROM scoped CROSS JOIN LATERAL jsonb_to_record(event_payload::jsonb) AS fields(
   author text, content jsonb, actions jsonb, long_running_tool_ids jsonb, branch text
 )
)
