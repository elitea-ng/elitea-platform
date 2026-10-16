-- 0149_chat_conversation_memory_taint_cleanup.sql — a conversation's
-- personal-memory taint goes with the conversation (worker unit M9).
--
-- elitea_runtime.memory_conversation_taints (shared/0166) records, per
-- project and conversation uuid, that personal memory was read in the
-- conversation; while the row exists every execution bound to the
-- conversation is refused shared writes. The row must not outlive the
-- conversation: a conversation is deleted on several paths (the conversation
-- route, the MCP run cleanup, pipeline trigger runs, bulk and project
-- deletes), so the cleanup is a trigger on the tenant table rather than a step
-- each path has to remember.
--
-- AFTER DELETE on chat_conversations deletes the taint keyed by this tenant's
-- project id (from the schema name, p_<id>) and the row's uuid. The shared
-- table may not exist yet when a tenant migrates first (shared and tenant
-- histories apply independently), so the function looks for it each time and
-- does nothing without it. A project delete drops the tenant schema; its taint
-- rows go by the shared table's foreign key to centry.project.
--
-- Idempotent. No BEGIN/COMMIT: the ledgered runner wraps the file.

DO $migration$
DECLARE
    tenant text := current_schema();
BEGIN
    IF tenant IS NULL THEN
        RAISE EXCEPTION 'memory taint cleanup migration needs a tenant search_path';
    END IF;

    EXECUTE format($fn$
        CREATE OR REPLACE FUNCTION %1$I.chat_conversation_memory_taint_cleanup() RETURNS trigger
        LANGUAGE plpgsql AS $body$
        DECLARE
            project integer;
        BEGIN
            IF OLD.uuid IS NULL
               OR to_regclass('elitea_runtime.memory_conversation_taints') IS NULL
               OR TG_TABLE_SCHEMA !~ '^p_[1-9][0-9]{0,8}$' THEN
                RETURN NULL;
            END IF;
            project := substr(TG_TABLE_SCHEMA, 3)::integer;
            DELETE FROM elitea_runtime.memory_conversation_taints
             WHERE project_id = project AND conversation_uuid = OLD.uuid;
            RETURN NULL;
        END
        $body$ $fn$, tenant);

    IF to_regclass('chat_conversations') IS NOT NULL
       AND EXISTS (SELECT 1 FROM pg_attribute WHERE attrelid = to_regclass('chat_conversations')
                     AND attname = 'uuid' AND NOT attisdropped) THEN
        DROP TRIGGER IF EXISTS chat_conversation_memory_taint_cleanup ON chat_conversations;
        CREATE TRIGGER chat_conversation_memory_taint_cleanup AFTER DELETE ON chat_conversations
            FOR EACH ROW EXECUTE FUNCTION chat_conversation_memory_taint_cleanup();
    END IF;
END
$migration$;
