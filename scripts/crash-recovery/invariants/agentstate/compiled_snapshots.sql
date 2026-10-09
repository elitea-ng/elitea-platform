-- I4 (P11 only): compiled snapshots produced by this execution's compilation jobs. Bound: :'exec_id'.
WITH k AS (
  SELECT d.tenant_id, d.project_id,
         sha256(convert_to('elitea.sandbox.activation.v1', 'UTF8') || '\x00'::bytea
                || int8send(octet_length(d.execution_id)::bigint) || convert_to(d.execution_id, 'UTF8')
                || int8send(64::bigint) || convert_to(encode(d.activation_id, 'hex'), 'UTF8')) AS job_key
    FROM elitea_runtime.sandbox_dispatches d WHERE d.execution_id = :'exec_id'
)
SELECT coalesce(json_agg(r ORDER BY r.created_at), '[]'::json) FROM (
  SELECT encode(s.snapshot_key, 'hex') AS snapshot_key, s.state, encode(s.compilation_job_key, 'hex') AS compilation_job_key,
         s.compilation_lease_epoch, encode(s.compilation_receipt_sha256, 'hex') AS compilation_receipt_sha256,
         s.created_at, s.expires_at
    FROM elitea_runtime.rust_compiled_snapshots s
    JOIN k ON k.tenant_id = s.tenant_id AND k.project_id = s.project_id AND k.job_key = s.compilation_job_key
) r;
