-- 0155_chat_conversation_fork_id_index.sql — index the forks by the caller's
-- fork_id.
--
-- A fork's request may carry a `fork_id` (issue 1234) so a retry after a lost
-- response answers the fork it already made rather than a second one. The
-- fork looks the id up among the caller's conversations
-- (meta->'forked_from'->>'fork_id', replayFork) under its fork_id lock, once
-- per fork that names one. chat_conversations has no index on that
-- expression, so each lookup would scan every conversation the author has in
-- the project. Partial: only forks made with a fork_id are indexed. Not
-- unique: the id is the caller's, and the lookup is per author.
--
-- GUARDED. A tenant schema that predates 0123 on a mixed deployment still has
-- to migrate; the index is created only where the table and its columns exist.
-- IDEMPOTENT (IF NOT EXISTS). No BEGIN/COMMIT: the ledgered runner wraps each
-- file. Plain CREATE INDEX rather than CONCURRENTLY, which cannot run inside
-- that transaction.
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
        CREATE INDEX IF NOT EXISTS chat_conversations_fork_id_idx
            ON chat_conversations (author_id, ((meta->'forked_from'->>'fork_id')))
            WHERE (meta->'forked_from'->>'fork_id') IS NOT NULL;
    END IF;
END
$migration$;
