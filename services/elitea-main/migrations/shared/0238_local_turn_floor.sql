-- 0238_local_turn_floor.sql — several agents in a desktop thread (ADR-0050
-- decision 7f, issue 1806).
--
-- A person's message in a desktop thread with two or more agents is routed
-- by Main at start (localturn/floor.go): the first agent answers that turn,
-- and the desktop starts each later one in order. Each local turn records
-- the message it belongs to and the routed order, so a later turn is
-- admitted only for the agent at its position of a message Main routed:
--
--   floor_question_id   the person's message: the first turn's question id
--                       (its own question_id for the first turn). NULL: not
--                       a routed message (one agent, or the floor off).
--   floor_position      the turn's index in floor_targets, from 0.
--   floor_targets       the routed agents, in order; '{}' when not routed.
--   executor            where the answering agent ran: 'local' (the desktop,
--                       with the workspace) or 'cloud' (relayed as a child
--                       turn; no workspace). Copied from the participant's
--                       mapping at start, so a mapping changed mid-turn does
--                       not change what the turn may do.
--
-- No backfill: existing rows are single-agent turns (NULL, 0, '{}', 'local').
-- No permission. Idempotent. No BEGIN/COMMIT: the ledgered runner wraps the
-- file.
ALTER TABLE elitea_runtime.local_turn_executions
    ADD COLUMN IF NOT EXISTS floor_question_id UUID,
    ADD COLUMN IF NOT EXISTS floor_position INTEGER NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS floor_targets INTEGER[] NOT NULL DEFAULT '{}',
    ADD COLUMN IF NOT EXISTS executor TEXT NOT NULL DEFAULT 'local';

DO $$
BEGIN
    ALTER TABLE elitea_runtime.local_turn_executions
        DROP CONSTRAINT IF EXISTS local_turn_executions_floor;
    ALTER TABLE elitea_runtime.local_turn_executions
        ADD CONSTRAINT local_turn_executions_floor CHECK (
            cardinality(floor_targets) <= 16
            AND array_position(floor_targets, NULL) IS NULL
            AND ((floor_question_id IS NULL AND cardinality(floor_targets) = 0 AND floor_position = 0)
                 OR (floor_question_id IS NOT NULL
                     AND floor_position >= 0 AND floor_position < cardinality(floor_targets)
                     AND floor_targets[floor_position + 1] = target_participant_id)));
    ALTER TABLE elitea_runtime.local_turn_executions
        DROP CONSTRAINT IF EXISTS local_turn_executions_executor;
    ALTER TABLE elitea_runtime.local_turn_executions
        ADD CONSTRAINT local_turn_executions_executor CHECK (executor IN ('local', 'cloud'));
END
$$;

-- A later turn reads its message's first turn, and a new message checks
-- whether it is the conversation's newest.
CREATE INDEX IF NOT EXISTS local_turn_executions_floor_idx
    ON elitea_runtime.local_turn_executions (project_id, conversation_uuid, started_at DESC)
    WHERE floor_question_id IS NOT NULL;

COMMENT ON COLUMN elitea_runtime.local_turn_executions.floor_targets IS
    'The agents Main routed the person''s message to, in order (ADR-0050 decision 7f); empty when the message was not routed.';
COMMENT ON COLUMN elitea_runtime.local_turn_executions.executor IS
    'Where the answering agent ran: local (the desktop, with the workspace) or cloud (a relayed child turn, no workspace).';
