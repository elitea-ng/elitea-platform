-- 0145_chat_conversation_activity.sql — a new message moves its
-- conversation's `updated_at`.
--
-- THE DEFECT
--
-- The conversation list orders by `updated_at DESC` and answers
-- COALESCE(updated_at, created_at) as the row's "last modified", but only a
-- rename or a settings write stamped `updated_at`. A conversation someone
-- chatted in a minute ago therefore sorted, and aged, by the day it was last
-- renamed: the web rail kept it where it was, and a native client (ADR-0025)
-- that showed "2 weeks ago" next to a conversation it had just used had to
-- recompute activity from its own message cache (Agent Zefir E2E DEF-R7).
--
-- THE CHANGE
--
-- AFTER INSERT on chat_message_group sets the owning conversation's
-- `updated_at = now()`. 0144 kept `updated_at` out of its triggers on purpose
-- ("its wire meaning is fixed"); that meaning is "last modified", and a
-- conversation that gained a message was modified. A message group's own
-- `updated_at` is untouched: null still means "never rewritten".
--
-- Only an INSERT counts. A group rewrite (an answer that finished streaming,
-- an edit, feedback) and a delete leave `updated_at` alone; they still bump
-- `sync_at` through 0144's triggers, so a synced client still sees them.
--
-- THROTTLE. One bump per conversation per second, like 0144's: a turn writes
-- the question and the answer group back to back, and a pipeline can write
-- many. A suppressed bump is less than a second behind the message that
-- caused it.
--
-- SYNC ORDERING. The UPDATE passes through 0144's BEFORE trigger, which
-- stamps `sync_at = clock_timestamp()`, so the conversation with its new
-- `updated_at` is re-delivered on the next delta under the usual settle
-- window, and 0144's own throttled bump that fires after it finds the stamp
-- fresh and does nothing. The AFTER UPDATE trigger on chat_conversations
-- writes no tombstone: neither privacy, source nor the filtered meta keys
-- change.
--
-- A conversation delete (`SET LOCAL elitea.sync_cascade = 'conversation'`)
-- inserts no groups, but the guard is kept for symmetry with 0144.
--
-- GUARDS. As 0144: tables and columns are checked, the function body is
-- schema-qualified because a trigger runs under the writer's search_path.
-- IDEMPOTENT. No BEGIN/COMMIT: the ledgered runner wraps each file.
DO $migration$
DECLARE
    tenant text := current_schema();
BEGIN
    IF tenant IS NULL THEN
        RAISE EXCEPTION 'chat activity migration needs a tenant search_path';
    END IF;

    EXECUTE format($fn$
        CREATE OR REPLACE FUNCTION %1$I.chat_conversation_activity() RETURNS trigger
        LANGUAGE plpgsql AS $body$
        BEGIN
            IF NEW.conversation_id IS NULL
               OR current_setting('elitea.sync_cascade', true) = 'conversation' THEN
                RETURN NULL;
            END IF;
            UPDATE %1$I.chat_conversations SET updated_at = now()
             WHERE id = NEW.conversation_id
               AND (updated_at IS NULL OR updated_at < now() - interval '1 second');
            RETURN NULL;
        END
        $body$ $fn$, tenant);

    IF to_regclass('chat_message_group') IS NOT NULL
       AND to_regclass('chat_conversations') IS NOT NULL
       AND EXISTS (SELECT 1 FROM pg_attribute WHERE attrelid = to_regclass('chat_message_group')
                     AND attname = 'conversation_id' AND NOT attisdropped)
       AND EXISTS (SELECT 1 FROM pg_attribute WHERE attrelid = to_regclass('chat_conversations')
                     AND attname = 'updated_at' AND NOT attisdropped) THEN
        DROP TRIGGER IF EXISTS chat_conversation_activity ON chat_message_group;
        CREATE TRIGGER chat_conversation_activity AFTER INSERT ON chat_message_group
            FOR EACH ROW EXECUTE FUNCTION chat_conversation_activity();
    END IF;
END
$migration$;
