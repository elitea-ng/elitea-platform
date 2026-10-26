-- 0178_execution_steering_messages.sql — steering during execution.
--
-- WHY. A user can send a message while a cloud chat turn runs. The message is
-- a STEERING message: it waits here until the worker reaches its next safe
-- point (after the tool results of a step, before the next model request).
-- A worker restart or a client reconnect loses nothing: the row is the
-- authority until it is injected.
--
-- TWO STEPS. The worker CLAIMS the message over gRPC
-- (RuntimeControlService.ClaimSteeringMessages) and adds it to the next model
-- request: the row is `claimed` by that lease epoch. When the model
-- checkpoint that holds the request is persisted, the worker ACKNOWLEDGES it:
-- in one transaction Main writes it as a normal user message of the
-- conversation (`message_id`) and marks it `injected`. A worker that stops
-- between the two steps loses nothing: the replacement claim (a larger lease
-- epoch) acknowledges what its restored checkpoint holds and gets the other
-- claimed messages again.
--
-- STATES. `pending` waits for a safe point. `claimed` is in the hands of the
-- claim of `claim_lease_epoch`. `injected` is final and names the
-- conversation message. `removed` is final (the author removed it). There is
-- no stored `turn_ended`: a pending or claimed row whose execution no longer
-- runs is read as `turn_ended`, because no acknowledgement can reach it after
-- the claim fence is gone. A claimed row reads as `pending` to the client.
--
-- HAND-OFF. A `turn_ended` message becomes the next turn's message exactly
-- once. A client takes its own rows (handOffChatExecutionSteering, a
-- `handoff_key` of its choice) and sends them; when no client acts within
-- about 10 seconds of the settlement, the steering hand-off routine of Main
-- takes them (`handoff_by = 'server'`) and starts the next turn itself, with
-- `handoff_question_id` as the question id so a retried start is the same
-- turn. Both take rows with one guarded UPDATE, so the first one wins. The
-- state is `handed_off` (final). `handoff_started_at` records that the
-- server's start was admitted; a server hand-off without it is retried.
-- When the author already sent a newer message in the conversation, the
-- routine starts nothing: the rows become `removed` and the clients get
-- steer.dropped with reason `superseded`.
--
-- A turn stopped before any output is deleted with its question; its rows
-- here are deleted in the same transaction (agent_cancel.go).
--
-- The content is the user's text. It is product data in PostgreSQL, like the
-- question. It never enters the command bus.
--
-- No backfill, no permission (the routes use models.chat.messages.create).
-- No BEGIN/COMMIT: the ledgered runner applies the file in one transaction.

CREATE TABLE IF NOT EXISTS elitea_runtime.execution_steering_messages (
    steering_id UUID PRIMARY KEY,
    queue_order BIGINT GENERATED ALWAYS AS IDENTITY,
    execution_id TEXT NOT NULL,
    generation BIGINT NOT NULL,
    project_id BIGINT NOT NULL CHECK (project_id > 0),
    response_message_id UUID NOT NULL,
    author_user_id BIGINT NOT NULL CHECK (author_user_id > 0),
    idempotency_key TEXT NOT NULL,
    content TEXT NOT NULL,
    state TEXT NOT NULL DEFAULT 'pending',
    revision INTEGER NOT NULL DEFAULT 1 CHECK (revision >= 1),
    message_id UUID,
    created_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    injected_at TIMESTAMPTZ,
    claim_lease_epoch BIGINT CHECK (claim_lease_epoch > 0),
    handoff_key TEXT,
    handoff_by TEXT,
    handoff_question_id UUID,
    handed_off_at TIMESTAMPTZ,
    handoff_started_at TIMESTAMPTZ,
    FOREIGN KEY (execution_id, generation)
        REFERENCES elitea_runtime.execution_jobs (execution_id, generation)
        ON DELETE CASCADE,
    UNIQUE (execution_id, author_user_id, idempotency_key),
    CONSTRAINT execution_steering_messages_state CHECK (
        state IN ('pending', 'claimed', 'injected', 'removed', 'handed_off')
    ),
    CONSTRAINT execution_steering_messages_handoff CHECK (
        (state = 'handed_off') = (handoff_key IS NOT NULL AND handed_off_at IS NOT NULL)
        AND (handoff_by IS NULL OR handoff_by IN ('client', 'server'))
        AND (handoff_by IS NOT DISTINCT FROM 'server') = (handoff_question_id IS NOT NULL)
    ),
    CONSTRAINT execution_steering_messages_claimed CHECK (
        state <> 'claimed' OR claim_lease_epoch IS NOT NULL
    ),
    CONSTRAINT execution_steering_messages_content CHECK (
        octet_length(content) BETWEEN 1 AND 16384
    ),
    CONSTRAINT execution_steering_messages_idempotency_key CHECK (
        octet_length(idempotency_key) BETWEEN 1 AND 128
        AND idempotency_key ~ '^[A-Za-z0-9_-]+$'
    ),
    CONSTRAINT execution_steering_messages_injected CHECK (
        (state = 'injected') = (message_id IS NOT NULL AND injected_at IS NOT NULL)
    )
);

-- The claim and the list read one execution's rows in queue order.
CREATE INDEX IF NOT EXISTS execution_steering_messages_queue_idx
    ON elitea_runtime.execution_steering_messages
       (execution_id, generation, queue_order);

-- The list of one answer reads by its response id.
CREATE INDEX IF NOT EXISTS execution_steering_messages_response_idx
    ON elitea_runtime.execution_steering_messages
       (project_id, response_message_id, queue_order);

-- The hand-off routine finds the waiting rows of ended turns, and its own
-- hand-offs that still wait for their start.
CREATE INDEX IF NOT EXISTS execution_steering_messages_handoff_idx
    ON elitea_runtime.execution_steering_messages (state, execution_id, generation)
    WHERE state IN ('pending', 'claimed')
       OR (state = 'handed_off' AND handoff_by = 'server' AND handoff_started_at IS NULL);
