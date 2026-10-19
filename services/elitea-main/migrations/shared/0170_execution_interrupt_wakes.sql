-- 0170_execution_interrupt_wakes.sql — park and wake for per-interrupt decisions.
--
-- WHY (libs/proto/contracts/fanout-interrupt-decisions-v1.md v1.1 §8, §8.1).
-- A fan-out execution parks when nothing can run and at least one card is
-- open. A decision that arrives after the park must start exactly one
-- continuation (kind fanout_wake). The park settlement and the decision both
-- run under the chat_message_group response row lock and then the
-- execution_interrupt_responses row lock; whichever commits second writes one
-- wake row. The row is the durable intent: Main admits the continuation job
-- from it after commit (and from a sweep) with the idempotency key below, so a
-- crash between the two steps still produces exactly one job.
--
-- No backfill, no permission. Nothing is parked before Wave 2a-2 Workers.

-- The response's current park: the parked execution and the decision revision
-- its final fetch saw. All set or all NULL.
ALTER TABLE elitea_runtime.execution_interrupt_responses
    ADD COLUMN parked_execution_id TEXT CHECK (parked_execution_id ~ '^[0-9a-f]{32}$'),
    ADD COLUMN parked_generation BIGINT CHECK (parked_generation > 0),
    ADD COLUMN parked_revision_seen BIGINT CHECK (parked_revision_seen >= 0),
    ADD COLUMN parked_at TIMESTAMPTZ,
    ADD CONSTRAINT execution_interrupt_responses_park_columns CHECK (
        (parked_execution_id IS NULL AND parked_generation IS NULL AND parked_revision_seen IS NULL AND parked_at IS NULL)
        OR (parked_execution_id IS NOT NULL AND parked_generation IS NOT NULL AND parked_revision_seen IS NOT NULL
            AND parked_at IS NOT NULL));

CREATE TABLE elitea_runtime.execution_interrupt_wakes (
    root_response_id UUID NOT NULL,
    -- The response decision_revision the wake was admitted for.
    decision_revision BIGINT NOT NULL CHECK (decision_revision > 0),
    project_id BIGINT NOT NULL CHECK (project_id > 0),
    -- The parked execution the wake continues.
    parked_execution_id TEXT NOT NULL CHECK (parked_execution_id ~ '^[0-9a-f]{32}$'),
    parked_generation BIGINT NOT NULL CHECK (parked_generation > 0),
    idempotency_key TEXT NOT NULL UNIQUE,
    -- QUEUED: intent recorded; ADMITTED: the continuation job exists;
    -- DONE: that execution settled or parked; FAILED: admission was refused
    -- with a typed, permanent reason (the cards stay DECIDED and visible).
    state TEXT NOT NULL CHECK (state IN ('QUEUED', 'ADMITTED', 'DONE', 'FAILED')),
    wake_execution_id TEXT CHECK (wake_execution_id ~ '^[0-9a-f]{32}$'),
    failure_code TEXT CHECK (failure_code ~ '^[a-z][a-z0-9_.]{0,63}$'),
    admission_attempts INTEGER NOT NULL DEFAULT 0 CHECK (admission_attempts BETWEEN 0 AND 1000),
    created_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    PRIMARY KEY (root_response_id, decision_revision),
    FOREIGN KEY (root_response_id, project_id)
        REFERENCES elitea_runtime.execution_interrupt_responses(root_response_id, project_id),
    FOREIGN KEY (parked_execution_id, parked_generation)
        REFERENCES elitea_runtime.execution_jobs(execution_id, generation),
    -- The key is derived, never chosen: continue-interrupt/{response}/{revision}.
    CONSTRAINT execution_interrupt_wakes_key CHECK (
        idempotency_key = 'continue-interrupt/' || root_response_id::text || '/' || decision_revision::text),
    CONSTRAINT execution_interrupt_wakes_state_coherence CHECK (
        (state = 'QUEUED' AND wake_execution_id IS NULL AND failure_code IS NULL)
        OR (state IN ('ADMITTED', 'DONE') AND wake_execution_id IS NOT NULL AND failure_code IS NULL)
        OR (state = 'FAILED' AND failure_code IS NOT NULL))
);

-- One active continuation per response (contract §8): at most one wake that is
-- queued or admitted. A decision that lands meanwhile is picked up by that
-- continuation's restore fetch.
CREATE UNIQUE INDEX execution_interrupt_wakes_one_active
    ON elitea_runtime.execution_interrupt_wakes (root_response_id)
    WHERE state IN ('QUEUED', 'ADMITTED');

-- The admission sweep reads queued wakes oldest first.
CREATE INDEX execution_interrupt_wakes_queued
    ON elitea_runtime.execution_interrupt_wakes (created_at)
    WHERE state = 'QUEUED';

-- FK support for job deletes (project deprovisioning).
CREATE INDEX execution_interrupt_wakes_parked_execution
    ON elitea_runtime.execution_interrupt_wakes (parked_execution_id, parked_generation);

-- The claim path asks whether an execution is a wake (claim opt-in, §8.1).
CREATE INDEX execution_interrupt_wakes_wake_execution
    ON elitea_runtime.execution_interrupt_wakes (wake_execution_id) WHERE wake_execution_id IS NOT NULL;

-- A new turn supersedes the open cards of the conversation's earlier
-- responses in its admission transaction; this keeps that lookup to the open
-- cards of one conversation.
CREATE INDEX execution_interrupts_open_by_conversation
    ON elitea_runtime.execution_interrupts (project_id, conversation_id)
    WHERE state IN ('PENDING', 'DECIDED');
