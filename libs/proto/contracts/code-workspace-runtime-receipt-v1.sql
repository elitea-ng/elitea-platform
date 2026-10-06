-- Root assigns the final migration number. The original sandbox job owns this proof.
ALTER TABLE elitea_runtime.sandbox_jobs
    ADD COLUMN workspace_runtime_receipt bytea;
ALTER TABLE elitea_runtime.sandbox_jobs
    ADD CONSTRAINT sandbox_workspace_runtime_receipt_bound
    CHECK (workspace_runtime_receipt IS NULL OR
        (runtime_id IS NOT NULL AND octet_length(workspace_runtime_receipt) BETWEEN 1 AND 4096));
-- No backfill, second registry, inferred owner, or mutation of terminal results.
