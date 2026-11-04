-- I1/I4: the execution row and its frozen identity. Bound: :'exec_id'.
SELECT coalesce(json_agg(r ORDER BY r.generation), '[]'::json) FROM (
  SELECT j.execution_id, j.generation, j.command_id, encode(j.request_digest, 'hex') AS request_digest,
         j.idempotency_key, j.state, j.desired_state, j.invocation_state, j.terminal_error_code,
         j.projection_project_id, j.admitted_at, j.settled_at
    FROM elitea_runtime.execution_jobs j
   WHERE j.execution_id = :'exec_id'
) r;
