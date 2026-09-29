-- Stop intent is durable before runtime termination. A terminal receipt remains
-- immutable. This execution-only field does not change product data schemas.
ALTER TABLE elitea_runtime.sandbox_jobs
    ADD COLUMN cancellation_requested BOOLEAN NOT NULL DEFAULT FALSE;
