-- 0157_chat_participant_mapping_conversation_cascade.sql — a conversation's
-- participant mappings go with it (#1429).
--
-- 0123 declared chat_participant_mapping.conversation_id as a plain
-- REFERENCES chat_conversations(id), with no ON DELETE action. Every deleter
-- that did not remove the mappings first failed with 23503 and left the
-- conversation behind: the refused-admission discards of the MCP agent call
-- and the pipeline trigger run did, and their errors were dropped. The dev
-- bootstrap schema (001_initial.sql), which most handler tests build on,
-- always declared the key ON DELETE CASCADE, so those tests passed against a
-- shape no deployment ran. This file gives deployments the bootstrap's shape.
--
-- The deleters that remove the mappings explicitly (ConversationsRepo.Delete,
-- repos.DiscardUnadmittedConversation) keep doing so: they run with
-- `elitea.sync_cascade = 'conversation'`, so the sync triggers of 0144 record
-- the participant removals the same way either way.
--
-- The key is found by its columns, not its name, and replaced only when it
-- does not cascade already. Added NOT VALID, then validated: the rows already
-- satisfied the same reference, and VALIDATE takes a lighter lock than a
-- validating ADD.
--
-- GUARDED. A tenant schema without the table (one that predates 0123) is left
-- alone. IDEMPOTENT. No BEGIN/COMMIT: the ledgered runner wraps each file.
DO $migration$
DECLARE
    existing record;
BEGIN
    IF to_regclass('chat_participant_mapping') IS NULL OR to_regclass('chat_conversations') IS NULL THEN
        RETURN;
    END IF;
    FOR existing IN
        SELECT constraint_row.conname, constraint_row.confdeltype
          FROM pg_constraint AS constraint_row
         WHERE constraint_row.conrelid = to_regclass('chat_participant_mapping')
           AND constraint_row.contype = 'f'
           AND constraint_row.confrelid = to_regclass('chat_conversations')
           AND constraint_row.conkey = ARRAY[(
               SELECT attnum FROM pg_attribute
                WHERE attrelid = to_regclass('chat_participant_mapping')
                  AND attname = 'conversation_id' AND NOT attisdropped)]
    LOOP
        IF existing.confdeltype = 'c' THEN
            RETURN;
        END IF;
        EXECUTE format('ALTER TABLE chat_participant_mapping DROP CONSTRAINT %I', existing.conname);
    END LOOP;
    ALTER TABLE chat_participant_mapping
        ADD CONSTRAINT chat_participant_mapping_conversation_id_fkey
        FOREIGN KEY (conversation_id) REFERENCES chat_conversations(id) ON DELETE CASCADE NOT VALID;
    ALTER TABLE chat_participant_mapping
        VALIDATE CONSTRAINT chat_participant_mapping_conversation_id_fkey;
END
$migration$;
