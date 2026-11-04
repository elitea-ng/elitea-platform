-- I2 (conditional): only when shared migration 0147 is applied. The collector checks
-- to_regclass('elitea_runtime.original_code_visits') first. Bound: :'exec_id'.
SELECT coalesce(json_agg(r), '[]'::json) FROM (
  SELECT v.activation_id, max(v.attempt) AS max_attempt, count(*) AS visits
    FROM elitea_runtime.original_code_visits v WHERE v.execution_id = :'exec_id' GROUP BY 1
) r;
