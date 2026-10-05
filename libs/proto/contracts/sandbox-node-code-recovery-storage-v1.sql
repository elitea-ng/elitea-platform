-- Storage reference for Main AgentState migration 0013_sandbox_whole_code_recovery.sql.
-- Apply the owning migration corpus. Do not execute this reference separately.
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
