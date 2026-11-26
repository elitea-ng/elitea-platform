-- 0282_local_floor_hands.sql — agents hand the floor to each other in a
-- desktop thread (ADR-0061 decision 3c, issue 3063).
--
-- A local agent's `hand_floor` call (handLocalFloor) is decided by Main with
-- the cloud's guard (turntaking.Hand), under a row lock on the routed
-- message's first local turn. The agents it hands the floor to are queued
-- here, one row each, in the order Main answered; the desktop then starts
-- each as a later turn of the message (`floor_turn.hand_id`), and Main
-- admits it QUEUED -> STARTED only for the agent it was handed to:
--
--   question_id           the person's message: its first turn's question id.
--   from_execution_id     the handing local turn; the receiver starts only
--                         once it is committed.
--   from_participant_id   the handing agent, to_participant_id the receiver.
--   ord                   the order within the message, from 1, unique.
--   message               the handing agent's text (at most 4,000 bytes):
--                         the receiver reads it framed as untrusted, and its
--                         commit stores it as the side message.
--   status                QUEUED, STARTED (started_execution_id set) or
--                         SKIPPED (skip_code: a guard's code, superseded,
--                         stopped_by_person or skipped_by_person).
--
-- local_turn_executions.floor_stopped is the guard that stopped the routed
-- message (on its first turn's row), so the guard's system message is
-- written once and every later hand-off of the message is refused.
--
-- The rows cascade from the project, as the local turns do (shared 0155),
-- and the project delete names them too. No backfill: nothing was queued
-- before. No permission. Idempotent. No BEGIN/COMMIT: the ledgered runner
-- wraps the file.
CREATE TABLE IF NOT EXISTS elitea_runtime.local_floor_hands (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    project_id INTEGER NOT NULL REFERENCES centry.project(id) ON DELETE CASCADE,
    conversation_uuid UUID NOT NULL,
    question_id UUID NOT NULL,
    from_execution_id TEXT NOT NULL,
    from_participant_id INTEGER NOT NULL,
    to_participant_id INTEGER NOT NULL,
    ord INTEGER NOT NULL,
    message TEXT NOT NULL,
    status TEXT NOT NULL DEFAULT 'QUEUED',
    skip_code TEXT,
    started_execution_id TEXT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    CONSTRAINT local_floor_hands_order UNIQUE (question_id, ord),
    CONSTRAINT local_floor_hands_started UNIQUE (started_execution_id),
    CONSTRAINT local_floor_hands_ord CHECK (ord >= 1),
    CONSTRAINT local_floor_hands_participants
        CHECK (from_participant_id > 0 AND to_participant_id > 0 AND from_participant_id <> to_participant_id),
    CONSTRAINT local_floor_hands_message
        CHECK (octet_length(message) BETWEEN 1 AND 4000),
    CONSTRAINT local_floor_hands_execution
        CHECK (from_execution_id ~ '^[0-9a-f]{32}$'
               AND (started_execution_id IS NULL OR started_execution_id ~ '^[0-9a-f]{32}$')),
    CONSTRAINT local_floor_hands_status CHECK (
        (status = 'QUEUED' AND skip_code IS NULL AND started_execution_id IS NULL)
        OR (status = 'STARTED' AND skip_code IS NULL AND started_execution_id IS NOT NULL)
        OR (status = 'SKIPPED' AND skip_code IS NOT NULL AND started_execution_id IS NULL)),
    CONSTRAINT local_floor_hands_skip_code CHECK (skip_code IS NULL OR skip_code ~ '^[a-z_]{1,32}$')
);

CREATE INDEX IF NOT EXISTS local_floor_hands_message_idx
    ON elitea_runtime.local_floor_hands (project_id, question_id, ord);

ALTER TABLE elitea_runtime.local_turn_executions
    ADD COLUMN IF NOT EXISTS floor_stopped TEXT;

DO $$
BEGIN
    ALTER TABLE elitea_runtime.local_turn_executions
        DROP CONSTRAINT IF EXISTS local_turn_executions_floor_stopped;
    ALTER TABLE elitea_runtime.local_turn_executions
        ADD CONSTRAINT local_turn_executions_floor_stopped
        CHECK (floor_stopped IS NULL OR (floor_stopped ~ '^[a-z_]{1,32}$' AND floor_question_id = question_id));
END
$$;

COMMENT ON TABLE elitea_runtime.local_floor_hands IS
    'The agents a local turn handed the floor to (ADR-0061 decision 3c), queued in Main''s order until the desktop starts them.';
COMMENT ON COLUMN elitea_runtime.local_turn_executions.floor_stopped IS
    'The loop guard that stopped this routed message''s exchange (on its first turn only); NULL while it runs.';
