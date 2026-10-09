-- I2/I4/I5/I6: Code dispatch journal and the sandbox ledger rows bound to it.
-- sandbox_jobs has no execution column. Its job_key is derived from the execution and the dispatch
-- activation (services/elitea-worker-rust/src/sandbox/code_recovery.rs:343-352, protocol/sandbox_grant.rs:309-318):
--   SHA256("elitea.sandbox.activation.v1\0" || be64(len(exec)) || exec || be64(len(hex(act))) || hex(act))
-- `job_matched` in the output proves the derivation holds for every dispatch of the run.
-- `runtime_label` is the Docker label `io.elitea.code.job` of the job's containers. It is NOT the job_key:
--   SHA256("elitea.sandbox.runtime-job.v1\0" || be64(len(tenant)) || tenant || be32(project) || job_key)
-- (services/elitea-worker-rust/src/sandbox/ledger.rs:46-66). All local stacks share one Docker daemon and
-- runtime names carry no stack label, so container checks match only these labels and runtime ids.
-- Bound: :'exec_id'.
WITH d AS (
  SELECT d.tenant_id, d.project_id, d.generation, d.activation_id, d.request_digest, d.audience, d.resolved,
         d.created_at, d.compiled_descriptor_json IS NOT NULL AS compiled,
         sha256(convert_to('elitea.sandbox.activation.v1', 'UTF8') || '\x00'::bytea
                || int8send(octet_length(d.execution_id)::bigint) || convert_to(d.execution_id, 'UTF8')
                || int8send(64::bigint) || convert_to(encode(d.activation_id, 'hex'), 'UTF8')) AS job_key
    FROM elitea_runtime.sandbox_dispatches d
   WHERE d.execution_id = :'exec_id'
)
SELECT coalesce(json_agg(r ORDER BY r.dispatch_created_at), '[]'::json) FROM (
  SELECT encode(d.activation_id, 'hex') AS activation_id, d.generation, d.audience, d.resolved, d.compiled,
         d.created_at AS dispatch_created_at, encode(d.request_digest, 'hex') AS dispatch_request_digest,
         j.job_key IS NOT NULL AS job_matched, encode(d.job_key, 'hex') AS job_key,
         encode(sha256(convert_to('elitea.sandbox.runtime-job.v1', 'UTF8') || '\x00'::bytea
                       || int8send(octet_length(d.tenant_id)::bigint) || convert_to(d.tenant_id, 'UTF8')
                       || int4send(d.project_id) || d.job_key), 'hex') AS runtime_label,
         encode(j.request_digest, 'hex') AS job_request_digest, j.phase, j.owner_id, j.lease_epoch, j.lease_until,
         j.runtime_id, j.runtime_bound_at, j.dispatched_at, j.failure_code, j.cancellation_requested,
         j.preparation_bundle_json IS NOT NULL AS has_preparation_bundle, j.result_json IS NOT NULL AS has_result,
         j.code_recovery_receipt_json IS NOT NULL AS has_recovery_receipt, j.code_recovery_cleanup_at,
         j.code_recovery_cleanup_failure, j.runtime_cleanup_confirmed_at, j.compiled_purpose,
         j.created_at AS job_created_at, j.updated_at AS job_updated_at
    FROM d LEFT JOIN elitea_runtime.sandbox_jobs j
      ON j.tenant_id = d.tenant_id AND j.project_id = d.project_id AND j.job_key = d.job_key
) r;
