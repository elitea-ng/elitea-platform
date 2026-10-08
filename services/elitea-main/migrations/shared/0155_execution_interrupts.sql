-- 0155_execution_interrupts.sql — per-interrupt HITL decision ledger.
--
-- WHY (libs/proto/contracts/fanout-interrupt-decisions-v1.md §5, Track M2).
-- Pending HITL cards live only in chat_message_group.meta.hitl_interrupts, so
-- Main can only resume a complete decision set and cannot consume one card
-- exactly once. Every HITL kind is now decided per interrupt: one row per
-- card, decided by a compare-and-set on (state, revision), consumed once by a
-- claim-fenced ACK, and closed by stop or regenerate.
--
-- Nothing writes these tables yet. Wave 2 wires the frame projection, the
-- private Worker routes and the stop/regenerate call sites; the public
-- decision API is registered behind ELITEA_RUNTIME_EXECUTION_INTERRUPTS_API_ENABLED
-- (default false). No backfill, no permission.

-- One row per root response that has ever had a card. Its row lock orders all
-- ledger writes of the response (after the chat_message_group row), and
-- decision_revision counts decisions for Wave 2 park/wake settlement.
CREATE TABLE elitea_runtime.execution_interrupt_responses (
    root_response_id UUID PRIMARY KEY,
    project_id BIGINT NOT NULL CHECK (project_id > 0),
    conversation_id BIGINT NOT NULL CHECK (conversation_id > 0),
    decision_revision BIGINT NOT NULL DEFAULT 0 CHECK (decision_revision >= 0),
    created_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    UNIQUE (root_response_id, project_id)
);

CREATE TABLE elitea_runtime.execution_interrupts (
    root_response_id UUID NOT NULL,
    interrupt_key TEXT NOT NULL
        CHECK (interrupt_key ~ '^[0-9a-f]{64}$' AND interrupt_key <> repeat('0', 64)),
    project_id BIGINT NOT NULL,
    conversation_id BIGINT NOT NULL CHECK (conversation_id > 0),
    execution_id TEXT NOT NULL CHECK (execution_id ~ '^[0-9a-f]{32}$'),
    generation BIGINT NOT NULL CHECK (generation > 0),
    -- Printable ASCII, 1-512 bytes, no leading space (PostgreSQL caps regex repetition at 255).
    interrupt_id TEXT NOT NULL
        CHECK (octet_length(interrupt_id) BETWEEN 1 AND 512 AND interrupt_id ~ '^[\x21-\x7e][\x20-\x7e]*$'),
    kind TEXT NOT NULL CHECK (kind IN ('tool_guard', 'hitl_node', 'ask_user', 'delegated_auth')),
    available_actions TEXT[] NOT NULL CHECK (
        cardinality(available_actions) BETWEEN 1 AND 4
        AND available_actions <@ ARRAY['approve', 'reject', 'edit', 'block_with_comment',
                                       'answer', 'authorize', 'skip', 'continue']::TEXT[]),
    -- Private: frozen child thread, fan-out node, ordinal. Never returned to a client.
    frontier JSONB NOT NULL
        CHECK (jsonb_typeof(frontier) = 'object' AND octet_length(frontier::text) <= 2048),
    card_json BYTEA NOT NULL CHECK (octet_length(card_json) BETWEEN 1 AND 32768),
    payload_sha256 TEXT NOT NULL CHECK (payload_sha256 ~ '^[0-9a-f]{64}$'),
    source_event_id TEXT NOT NULL UNIQUE CHECK (octet_length(source_event_id) BETWEEN 1 AND 256),
    state TEXT NOT NULL CHECK (state IN ('PENDING', 'DECIDED', 'CONSUMED', 'CANCELLED', 'SUPERSEDED')),
    revision BIGINT NOT NULL CHECK (revision BETWEEN 1 AND 3),
    request_id TEXT CHECK (request_id ~ '^[0-9a-f]{64}$' AND request_id <> repeat('0', 64)),
    -- Canonical decision body. Delegated auth carries a token-store reference, never a token.
    decision_json BYTEA CHECK (octet_length(decision_json) BETWEEN 1 AND 8192),
    decided_by BIGINT CHECK (decided_by > 0),
    decided_at TIMESTAMPTZ,
    consumed_claim_id TEXT REFERENCES elitea_runtime.execution_claims(claim_id),
    consumed_checkpoint_id TEXT CHECK (consumed_checkpoint_id ~ '^[A-Za-z0-9_.:-]{1,128}$'),
    consumed_at TIMESTAMPTZ,
    -- Canonical ACK bytes: a byte-identical replay is accepted, any other is refused.
    ack_json BYTEA CHECK (octet_length(ack_json) BETWEEN 1 AND 1024),
    closed_at TIMESTAMPTZ,
    raised_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    PRIMARY KEY (root_response_id, interrupt_key),
    FOREIGN KEY (root_response_id, project_id)
        REFERENCES elitea_runtime.execution_interrupt_responses(root_response_id, project_id),
    FOREIGN KEY (execution_id, generation)
        REFERENCES elitea_runtime.execution_jobs(execution_id, generation),
    -- The decision columns are written together or not at all.
    CONSTRAINT execution_interrupts_decision_columns CHECK (
        (request_id IS NULL AND decision_json IS NULL AND decided_by IS NULL AND decided_at IS NULL)
        OR (request_id IS NOT NULL AND decision_json IS NOT NULL AND decided_by IS NOT NULL AND decided_at IS NOT NULL)),
    -- The consumption columns likewise.
    CONSTRAINT execution_interrupts_consumption_columns CHECK (
        (consumed_claim_id IS NULL AND consumed_checkpoint_id IS NULL AND consumed_at IS NULL)
        OR (consumed_claim_id IS NOT NULL AND consumed_checkpoint_id IS NOT NULL AND consumed_at IS NOT NULL)),
    -- State coherence (contract §5; pattern 0146 control outbox).
    CONSTRAINT execution_interrupts_state_coherence CHECK (
        (state = 'PENDING' AND revision = 1 AND request_id IS NULL
            AND consumed_at IS NULL AND ack_json IS NULL AND closed_at IS NULL)
        OR (state = 'DECIDED' AND revision = 2 AND request_id IS NOT NULL
            AND consumed_at IS NULL AND ack_json IS NULL AND closed_at IS NULL)
        OR (state = 'CONSUMED' AND revision = 3 AND request_id IS NOT NULL
            AND consumed_at IS NOT NULL AND ack_json IS NOT NULL AND closed_at IS NULL)
        OR (state IN ('CANCELLED', 'SUPERSEDED') AND closed_at IS NOT NULL AND consumed_at IS NULL
            AND ((request_id IS NULL AND revision = 2 AND ack_json IS NULL)
                 OR (request_id IS NOT NULL AND revision = 3
                     AND (ack_json IS NULL OR state = 'SUPERSEDED'))))
    )
);

-- The Worker interrupt id is unique among the open cards of one response
-- (contract §3.2); Web keys cards by it.
CREATE UNIQUE INDEX execution_interrupts_one_open_interrupt_id
    ON elitea_runtime.execution_interrupts (root_response_id, interrupt_id)
    WHERE state IN ('PENDING', 'DECIDED');

CREATE TABLE elitea_runtime.execution_interrupt_audit (
    root_response_id UUID NOT NULL,
    interrupt_key TEXT NOT NULL,
    transition TEXT NOT NULL CHECK (transition IN ('RAISED', 'DECIDED', 'CONSUMED', 'CANCELLED', 'SUPERSEDED')),
    revision BIGINT NOT NULL CHECK (revision BETWEEN 1 AND 3),
    actor_id BIGINT CHECK (actor_id > 0),
    claim_id TEXT CHECK (octet_length(claim_id) BETWEEN 1 AND 256),
    created_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    PRIMARY KEY (root_response_id, interrupt_key, transition),
    FOREIGN KEY (root_response_id, interrupt_key)
        REFERENCES elitea_runtime.execution_interrupts(root_response_id, interrupt_key),
    -- A human decides; a claim raises and consumes; stop and supersede name either.
    CHECK ((actor_id IS NULL) <> (claim_id IS NULL)),
    CHECK (transition <> 'DECIDED' OR actor_id IS NOT NULL),
    CHECK (transition NOT IN ('RAISED', 'CONSUMED') OR claim_id IS NOT NULL)
);
