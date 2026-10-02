-- Worker-owned delivery identities survive graph/worker loss. Supervisor
-- receipts remain authoritative for the actual runtime outcome.
CREATE TABLE elitea_runtime.sandbox_dispatches (
    tenant_id TEXT NOT NULL CHECK (octet_length(tenant_id) BETWEEN 1 AND 256),
    project_id INTEGER NOT NULL CHECK (project_id > 0),
    execution_id TEXT NOT NULL CHECK (octet_length(execution_id) BETWEEN 1 AND 256),
    generation BIGINT NOT NULL CHECK (generation > 0),
    activation_id BYTEA NOT NULL CHECK (octet_length(activation_id) = 32),
    request_digest BYTEA NOT NULL CHECK (octet_length(request_digest) = 32),
    audience TEXT NOT NULL CHECK (octet_length(audience) BETWEEN 1 AND 256),
    resolved BOOLEAN NOT NULL DEFAULT FALSE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    PRIMARY KEY (tenant_id, project_id, execution_id, generation, activation_id)
);
CREATE INDEX sandbox_dispatches_pending
    ON elitea_runtime.sandbox_dispatches (tenant_id, project_id, execution_id, generation, activation_id)
    WHERE NOT resolved;
