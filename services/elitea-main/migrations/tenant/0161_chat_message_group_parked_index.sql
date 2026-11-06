-- 0161_chat_message_group_parked_index.sql — "needs you" for a parked
-- fan-out (FU-284).
--
-- A Parallel or Map that waits on its cards parks the response with
-- meta.fanout_parked_v1 (contract §8) instead of a root pause key. 0153's
-- "needs you" predicate and its partial index chat_message_group_paused_idx
-- predate it. This migration adds the park, additively:
--
--   chat_message_group_parked_idx  (conversation_id)
--       WHERE NOT is_streaming AND meta ? 'fanout_parked_v1'
--     The second arm of chatactivity.Paused(): the sidebar reads only the
--     parked groups, as it reads only the paused ones through 0153's index.
--
--   chat_group_meta_paused(meta)  (0153's function, replaced)
--     Also true for fanout_parked_v1, so the conversation sync bump fires when
--     a group starts or stops waiting on fan-out cards, as for a root pause.
--
-- LOCK. Plain CREATE INDEX: the ledgered runner wraps each file in a
-- transaction, where CONCURRENTLY cannot run (0156). The build holds a SHARE
-- lock on chat_message_group for one pass over the table (writes wait, reads
-- go on); the index itself holds only the parked groups.
--
-- GUARDS. As 0153: every relation and column is checked, because integration
-- fixtures apply the tenant chain to minimal hand-built tables. IDEMPOTENT
-- (IF NOT EXISTS, CREATE OR REPLACE). No BEGIN/COMMIT: the ledgered runner
-- wraps each file.
DO $migration$
DECLARE
    tenant text := current_schema();
BEGIN
    IF tenant IS NULL THEN
        RAISE EXCEPTION 'chat parked index migration needs a tenant search_path';
    END IF;

    IF to_regclass('chat_message_group') IS NOT NULL
       AND (SELECT count(*) FROM pg_attribute WHERE attrelid = to_regclass('chat_message_group')
              AND NOT attisdropped
              AND attname = ANY (ARRAY['conversation_id', 'is_streaming', 'meta'])) = 3 THEN
        CREATE INDEX IF NOT EXISTS chat_message_group_parked_idx
            ON chat_message_group (conversation_id)
            WHERE NOT is_streaming AND meta ? 'fanout_parked_v1';
    END IF;

    -- Whether a group's meta holds a pause key: 0153's set and the park.
    EXECUTE format($fn$
        CREATE OR REPLACE FUNCTION %1$I.chat_group_meta_paused(meta jsonb) RETURNS boolean
        LANGUAGE sql IMMUTABLE AS $body$
            SELECT COALESCE(meta ? 'hitl_interrupt' OR meta ? 'hitl_interrupts'
                OR meta ? 'authorization_requests' OR meta->'output_limit_reached' = 'true'::jsonb
                OR meta ? 'pipeline_static_v1' OR meta ? 'node_recovery_required_v1'
                OR meta ? 'fanout_parked_v1', false)
        $body$ $fn$, tenant);
END
$migration$;
