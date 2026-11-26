-- 0255_chat_conversation_client_request_id_index.sql — index the
-- conversations by the create's client_request_id (#3077).
--
-- A create may carry a `client_request_id` (the sending message's client id)
-- so a duplicate (Enter pressed again, a retry after a lost answer) answers
-- the conversation it already made rather than a second one. The create looks
-- the id up among the caller's conversations (meta->>'client_request_id',
-- replayCreate) under its lock, once per create that names one. Without an
-- index on that expression each lookup would scan every conversation the
-- author has in the project. Partial: only conversations made with an id are
-- indexed. Not unique: the id is the caller's, and the lookup is per author.
--
-- GUARDED, as tenant 0155: created only where the table and its columns
-- exist. IDEMPOTENT (IF NOT EXISTS). No BEGIN/COMMIT: the ledgered runner
-- wraps each file. Plain CREATE INDEX rather than CONCURRENTLY, which cannot
-- run inside that transaction.
DO $migration$
BEGIN
    IF to_regclass('chat_conversations') IS NOT NULL
       AND (
           SELECT count(*)
           FROM pg_attribute
           WHERE attrelid = to_regclass('chat_conversations')
             AND attname IN ('author_id', 'meta')
             AND NOT attisdropped
       ) = 2 THEN
        CREATE INDEX IF NOT EXISTS chat_conversations_client_request_id_idx
            ON chat_conversations (author_id, ((meta->>'client_request_id')))
            WHERE (meta->>'client_request_id') IS NOT NULL;
    END IF;
END
$migration$;
