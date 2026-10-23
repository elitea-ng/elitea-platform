-- 0150_chat_conversation_activity_utc.sql — the conversation activity bump
-- (0145) writes the UTC wall clock.
--
-- THE DEFECT
--
-- chat_conversations.updated_at is a `timestamp` WITHOUT time zone. Its
-- meaning is the UTC wall clock: the legacy runtime wrote it with
-- server_default now() in UTC containers, and the web client appends `Z` to a
-- naive timestamp before it parses one. 0145's trigger stamps it with a bare
-- now(), which is a timestamptz, so Postgres stores it in the SESSION time
-- zone. On a server or session that is not UTC the bump wrote local time: the
-- list's "last modified" was off by the offset, and the one-second throttle
-- compared that local stamp with a local now(), so it held only by accident.
--
-- THE CHANGE
--
-- The same function, with now() AT TIME ZONE 'UTC' in the stamp and in the
-- throttle. Every Go writer of the chat tables does the same since this
-- change. Nothing else moves: the trigger (0145) still points at this
-- function, and no row is rewritten. A stamp an earlier non-UTC session
-- wrote keeps its value.
--
-- GUARDS. As 0145: the function body is schema-qualified because a trigger
-- runs under the writer's search_path. CREATE OR REPLACE keeps the trigger
-- bound. IDEMPOTENT. No BEGIN/COMMIT: the ledgered runner wraps each file.
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
            UPDATE %1$I.chat_conversations SET updated_at = now() AT TIME ZONE 'UTC'
             WHERE id = NEW.conversation_id
               AND (updated_at IS NULL
                    OR updated_at < (now() AT TIME ZONE 'UTC') - interval '1 second');
            RETURN NULL;
        END
        $body$ $fn$, tenant);
END
$migration$;
