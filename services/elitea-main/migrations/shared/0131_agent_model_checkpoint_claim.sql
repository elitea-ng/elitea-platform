-- Keep checkpoint inspection distinct from ordinary execution authority.
ALTER TABLE elitea_runtime.execution_claims
    ADD COLUMN recovery_mode TEXT NOT NULL DEFAULT 'NONE',
    ADD COLUMN model_checkpoint_digest BYTEA;

ALTER TABLE elitea_runtime.execution_claims
    ADD CONSTRAINT execution_claims_recovery_mode
        CHECK (recovery_mode IN ('NONE', 'AGENT_MODEL_CHECKPOINT')),
    ADD CONSTRAINT execution_claims_model_checkpoint_digest
        CHECK (model_checkpoint_digest IS NULL OR
               (recovery_mode = 'AGENT_MODEL_CHECKPOINT' AND
                octet_length(model_checkpoint_digest) = 32));
