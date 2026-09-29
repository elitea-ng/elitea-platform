-- Route already-authorized stop intent to the stable supervisor deployment
-- owner, including a stop admitted before any dispatch lease exists.
ALTER TABLE elitea_runtime.sandbox_jobs
    ADD COLUMN cancellation_owner TEXT
        CHECK (length(cancellation_owner) BETWEEN 1 AND 128);

UPDATE elitea_runtime.sandbox_jobs
SET cancellation_owner = owner_id
WHERE cancellation_requested AND owner_id IS NOT NULL;

CREATE INDEX sandbox_jobs_pending_stop
    ON elitea_runtime.sandbox_jobs (cancellation_owner, updated_at, tenant_id, project_id, job_key)
    WHERE cancellation_requested AND phase IN ('reserved', 'dispatched');
