-- Keep whole Code recovery proof in the original AgentState sandbox owner row.
-- Nullable additions preserve all existing rows. Do not backfill or synthesize proof.
ALTER TABLE elitea_runtime.sandbox_jobs
    ADD COLUMN code_recovery_binding_json TEXT
        CHECK (octet_length(code_recovery_binding_json) BETWEEN 1 AND 8192),
    ADD COLUMN code_recovery_receipt_json TEXT
        CHECK (octet_length(code_recovery_receipt_json) BETWEEN 1 AND 1048576),
    ADD COLUMN code_recovery_cleanup_at TIMESTAMPTZ,
    ADD COLUMN code_recovery_cleanup_failure TEXT
        CHECK (code_recovery_cleanup_failure IN ('termination_unconfirmed','cleanup_unconfirmed'));
ALTER TABLE elitea_runtime.sandbox_jobs ADD CONSTRAINT sandbox_whole_code_receipt_phase
    CHECK (code_recovery_receipt_json IS NULL OR
        (code_recovery_binding_json IS NOT NULL AND
            (phase='completed' OR
                (phase='failed' AND failure_code='recovery_verified_no_effect' AND dispatched_at IS NULL))));
ALTER TABLE elitea_runtime.sandbox_jobs ADD CONSTRAINT sandbox_whole_code_cleanup_phase
    CHECK ((code_recovery_cleanup_at IS NULL AND code_recovery_cleanup_failure IS NULL) OR
        (code_recovery_receipt_json IS NOT NULL AND phase='failed' AND
            failure_code='recovery_verified_no_effect' AND dispatched_at IS NULL));

ALTER TABLE elitea_runtime.sandbox_jobs ADD COLUMN code_platform_binding_json TEXT
    CHECK (octet_length(code_platform_binding_json) BETWEEN 1 AND 8192);

-- Keep the parameter-bound failure code in the key for generic prepared plans.
-- The lease expiry remains a runtime predicate because database time changes.
CREATE INDEX sandbox_jobs_code_no_effect_cleanup
    ON elitea_runtime.sandbox_jobs
        (owner_id, failure_code, updated_at, tenant_id, project_id, job_key)
    WHERE phase = 'failed' AND dispatched_at IS NULL
        AND NOT cancellation_requested
        AND code_recovery_receipt_json IS NOT NULL
        AND runtime_id IS NOT NULL
        AND code_recovery_cleanup_at IS NULL;
