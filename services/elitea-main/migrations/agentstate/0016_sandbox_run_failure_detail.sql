-- The program output a failed Code node ends its run with (#2227, #2234).
--
-- The worker keeps that output (a bounded stderr tail, control characters
-- removed) in a per-run slot in memory until the run's terminal frame is built.
-- A worker restart between the node failure and the terminal lost it, so the
-- error then carried the registered sentence alone. This table is the slot's
-- durable copy: one row per run (execution and generation), replaced by a later
-- failure of the same run, read back only when the terminal is built and the
-- slot is empty. It holds diagnostics only, never code, input or grants.
--
-- Rows are bounded like elitea_runtime.sandbox_jobs.failure_detail (the worker
-- keeps the last 1,500 characters; the check is the byte ceiling).
CREATE TABLE elitea_runtime.sandbox_run_failure_details (
    tenant_id TEXT NOT NULL CHECK (octet_length(tenant_id) BETWEEN 1 AND 256),
    project_id INTEGER NOT NULL CHECK (project_id > 0),
    execution_id TEXT NOT NULL CHECK (octet_length(execution_id) BETWEEN 1 AND 256),
    generation BIGINT NOT NULL CHECK (generation > 0),
    failure_detail TEXT NOT NULL CHECK (octet_length(failure_detail) BETWEEN 1 AND 8192),
    recorded_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    PRIMARY KEY (tenant_id, project_id, execution_id, generation)
);
