-- Sandbox supervisors own job receipts, not graph checkpoints. This table is
-- in agentstate only; no product or legacy application tables are changed.
-- Keep terminal rows until the enclosing execution's replay horizon expires.
CREATE TABLE elitea_runtime.sandbox_jobs (
    tenant_id TEXT NOT NULL CHECK (octet_length(tenant_id) BETWEEN 1 AND 256),
    project_id INTEGER NOT NULL CHECK (project_id > 0),
    job_key BYTEA NOT NULL CHECK (octet_length(job_key) = 32),
    request_digest BYTEA NOT NULL CHECK (octet_length(request_digest) = 32),
    phase TEXT NOT NULL DEFAULT 'reserved'
        CHECK (phase IN ('reserved','dispatched','completed','failed','cancelled','uncertain')),
    owner_id TEXT,
    lease_epoch BIGINT NOT NULL DEFAULT 0 CHECK (lease_epoch >= 0),
    lease_until TIMESTAMPTZ,
    result_json TEXT CHECK (octet_length(result_json) <= 524288),
    failure_code TEXT CHECK (failure_code ~ '^[a-z][a-z0-9_.]{0,63}$'),
    created_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    PRIMARY KEY (tenant_id, project_id, job_key),
    CHECK ((owner_id IS NULL AND lease_until IS NULL AND lease_epoch = 0)
        OR (owner_id IS NOT NULL AND octet_length(owner_id) BETWEEN 1 AND 128 AND lease_until IS NOT NULL AND lease_epoch > 0)),
    CHECK ((phase = 'completed' AND result_json IS NOT NULL AND failure_code IS NULL)
        OR (phase IN ('failed','cancelled','uncertain') AND result_json IS NULL AND failure_code IS NOT NULL)
        OR (phase IN ('reserved','dispatched') AND result_json IS NULL AND failure_code IS NULL))
);
CREATE INDEX sandbox_jobs_reconciliation
    ON elitea_runtime.sandbox_jobs (lease_until)
    WHERE phase IN ('reserved','dispatched');
