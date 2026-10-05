ALTER TABLE elitea_runtime.execution_jobs DROP CONSTRAINT execution_jobs_desired_state,
    ADD CONSTRAINT execution_jobs_desired_state CHECK (desired_state IN ('RUNNING','CANCELLED','DRAINING','SUSPENDED'));

-- Keep node recovery separate from terminal HITL and execution command delivery.
ALTER TABLE elitea_runtime.execution_claims
    DROP CONSTRAINT execution_claims_recovery_mode,
    ADD CONSTRAINT execution_claims_recovery_mode
        CHECK (recovery_mode IN ('NONE', 'AGENT_MODEL_CHECKPOINT', 'NODE_RECOVERY'));

CREATE TABLE elitea_runtime.node_recovery_visits (
    execution_id TEXT NOT NULL,
    generation BIGINT NOT NULL CHECK (generation > 0),
    activation_id TEXT NOT NULL CHECK (activation_id ~ '^[0-9a-f]{64}$'),
    journal_revision BIGINT NOT NULL CHECK (journal_revision > 0),
    node_id TEXT NOT NULL,
    graph_thread TEXT NOT NULL,
    step BIGINT NOT NULL CHECK (step >= 0),
    attempt INTEGER NOT NULL CHECK (attempt BETWEEN 1 AND 16),
    receipt_json BYTEA NOT NULL CHECK (octet_length(receipt_json) BETWEEN 1 AND 8192),
    receipt_digest BYTEA NOT NULL CHECK (octet_length(receipt_digest) = 32),
    source_event_id TEXT NOT NULL UNIQUE,
    source_claim_id TEXT NOT NULL REFERENCES elitea_runtime.execution_claims(claim_id),
    status TEXT NOT NULL CHECK (status IN ('SUSPENDED', 'AUTHORIZED', 'RESUMED', 'RECONCILED', 'STOPPED', 'CANCELLED')),
    created_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    PRIMARY KEY (execution_id, generation, activation_id, journal_revision),
    FOREIGN KEY (execution_id, generation)
        REFERENCES elitea_runtime.execution_jobs(execution_id, generation)
);

CREATE INDEX node_recovery_latest_visit
    ON elitea_runtime.node_recovery_visits(execution_id, generation, created_at DESC, journal_revision DESC);

CREATE UNIQUE INDEX node_recovery_one_pending_visit
    ON elitea_runtime.node_recovery_visits(execution_id, generation)
    WHERE status IN ('SUSPENDED', 'AUTHORIZED');

CREATE TABLE elitea_runtime.node_recovery_control_outbox (
    execution_id TEXT NOT NULL,
    generation BIGINT NOT NULL,
    activation_id TEXT NOT NULL,
    expected_revision BIGINT NOT NULL,
    request_id TEXT NOT NULL CHECK (request_id ~ '^[0-9a-f]{64}$'),
    request_json BYTEA NOT NULL,
    action_json BYTEA NOT NULL CHECK (octet_length(action_json) BETWEEN 1 AND 8192),
    actor_id TEXT NOT NULL,
    authorized_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    consumed_at TIMESTAMPTZ,
    consumed_claim_id TEXT REFERENCES elitea_runtime.execution_claims(claim_id),
    applied_revision BIGINT,
    ack_json BYTEA,
    PRIMARY KEY (execution_id, generation, request_id),
    UNIQUE (execution_id, generation, activation_id, expected_revision),
    FOREIGN KEY (execution_id, generation, activation_id, expected_revision)
        REFERENCES elitea_runtime.node_recovery_visits(execution_id, generation, activation_id, journal_revision),
    CHECK ((consumed_at IS NULL AND consumed_claim_id IS NULL AND applied_revision IS NULL AND ack_json IS NULL)
        OR (consumed_at IS NOT NULL AND consumed_claim_id IS NOT NULL AND applied_revision = expected_revision + 1 AND octet_length(ack_json) BETWEEN 1 AND 8192))
);

CREATE TABLE elitea_runtime.node_recovery_audit (
    execution_id TEXT NOT NULL,
    generation BIGINT NOT NULL,
    request_id TEXT NOT NULL,
    transition TEXT NOT NULL CHECK (transition IN ('AUTHORIZED', 'APPLIED', 'CANCELLED')),
    actor_id TEXT NOT NULL,
    activation_id TEXT NOT NULL,
    journal_revision BIGINT NOT NULL,
    claim_id TEXT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    PRIMARY KEY (execution_id, generation, request_id, transition),
    FOREIGN KEY (execution_id, generation, request_id)
        REFERENCES elitea_runtime.node_recovery_control_outbox(execution_id, generation, request_id)
);
