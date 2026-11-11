-- 0170_chat_message_subagent_lanes.sql — one row per sub-agent lane
-- (issue 1923, #1823 follow-up).
--
-- `chat_message_group.meta.subagents_v1` held every lane of an answer as
-- one JSON array and every accepted `agent_subagent` frame re-encoded the
-- whole array and rewrote the whole `meta` TOASTed value with it: O(n^2)
-- bytes per turn (measured: a 12-child, 48-item-per-lane turn cost 111 MB
-- of WAL with a targeted `jsonb_set`). From here each lane is its own row,
-- updated in place; a turn now costs what one lane's own row holds (9.8 MB
-- WAL on the same benchmark).
--
-- chat_message_subagent_lanes
--   One row per (answer, sub-agent). The projection UPDATEs this row for
--   every frame of that child; DELETE CASCADEs with the answer.
--
--   message_group_id  the answer (chat_message_group.id); the lane
--                      projection already keys on this integer id, not the
--                      uuid, so this FK matches it rather than 0167's uuid
--                      FK.
--   agent_id           the child's id ("a_" and 8 hex digits).
--   ordinal             1 for the run's first child, then in spawn order
--                      (unique with message_group_id: one row per child).
--   lane                the lane as `meta.subagents_v1` kept it: parent_call_id,
--                      label, display_name, avatar, status, items, steps,
--                      tokens, result, error — everything but agent_id and
--                      ordinal, which are columns here. #1872: depth and
--                      parent_agent_id travel inside this JSON (not
--                      promoted to columns, since nesting is read-side only).
--
-- On read, every answer's rows fold back into `meta.subagents_v1`
-- (`SubAgentLane[]`, client contract 1.48/1.50), ordered by ordinal, so the
-- wire shape is unchanged; #1872 nests a depth-2 row under its parent's
-- `children` at fold time.
--
-- Guarded on chat_message_group for the reason 0167 is: several
-- integration fixtures apply the tenant chain to a schema without it.
-- Unqualified names: the runner pins search_path to the tenant schema.
DO $$
BEGIN
    IF to_regclass('chat_message_group') IS NULL THEN
        RETURN;
    END IF;
    CREATE TABLE IF NOT EXISTS chat_message_subagent_lanes (
        message_group_id integer     NOT NULL
                         REFERENCES chat_message_group (id) ON DELETE CASCADE,
        agent_id         text        NOT NULL CHECK (length(agent_id) BETWEEN 1 AND 128),
        ordinal          integer     NOT NULL CHECK (ordinal >= 1),
        lane             jsonb       NOT NULL CHECK (jsonb_typeof(lane) = 'object'),
        created_at       timestamptz NOT NULL DEFAULT clock_timestamp(),
        updated_at       timestamptz NOT NULL DEFAULT clock_timestamp(),
        PRIMARY KEY (message_group_id, agent_id)
    );
    CREATE UNIQUE INDEX IF NOT EXISTS chat_message_subagent_lanes_ordinal
        ON chat_message_subagent_lanes (message_group_id, ordinal);
END
$$;
