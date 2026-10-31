-- Preflight: no non-terminal execution admitted since the stack baseline may exist before a scenario starts.
-- Executions older than :'since' come from the restored dump (stale rows of the source stack); they are
-- counted separately and never touched. Also the migration heads and the 0147 pin.
SELECT json_build_object(
  'open_executions', (SELECT count(*) FROM elitea_runtime.execution_jobs
      WHERE state NOT IN ('SUCCEEDED', 'FAILED', 'CANCELLED', 'QUARANTINED') AND admitted_at >= :'since'::timestamptz),
  'open_execution_ids', coalesce((SELECT json_agg(execution_id) FROM (SELECT execution_id FROM elitea_runtime.execution_jobs
      WHERE state NOT IN ('SUCCEEDED', 'FAILED', 'CANCELLED', 'QUARANTINED') AND admitted_at >= :'since'::timestamptz
      ORDER BY admitted_at DESC LIMIT 10) t), '[]'::json),
  'inherited_open_executions', (SELECT count(*) FROM elitea_runtime.execution_jobs
      WHERE state NOT IN ('SUCCEEDED', 'FAILED', 'CANCELLED', 'QUARANTINED') AND admitted_at < :'since'::timestamptz),
  'migration_heads', coalesce((SELECT json_object_agg(target_kind, v) FROM (
      SELECT target_kind, max(version) AS v FROM elitea_runtime.schema_migrations GROUP BY 1) m), '{}'::json),
  'migration_0147', (SELECT name FROM elitea_runtime.schema_migrations WHERE target_kind = 'shared' AND version = 147 LIMIT 1),
  'original_code_visits_table', to_regclass('elitea_runtime.original_code_visits') IS NOT NULL);
