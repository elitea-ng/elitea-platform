-- 0156_chat_conversation_local_child_index.sql — index the cloud child
-- conversations of desktop local turns (issue 1239).
--
-- A nested agent of a turn the desktop runs locally runs as a cloud child
-- turn in a hidden conversation (`source` 'local_child'). The desktop polls
-- each child (readLocalChildTurn, LocalChildTurnsRepo.ReadChildTurn) by
-- meta->>'child_execution_id' under meta->>'parent_execution_id', every
-- half second to three seconds while it runs; each start looks its
-- conversation up by meta->>'parent_execution_id' and
-- meta->>'idempotency_key' (PrepareChildConversation). chat_conversations has
-- no index on either expression, so every poll would scan every conversation
-- of the project. Partial: only child conversations are indexed. Not unique:
-- the lookups also filter on the author, and an unstamped child has no
-- execution id yet.
--
-- GUARDED. A tenant schema that predates 0123 on a mixed deployment still has
-- to migrate; the indexes are created only where the table and its columns
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
             AND attname IN ('source', 'meta')
             AND NOT attisdropped
       ) = 2 THEN
        CREATE INDEX IF NOT EXISTS chat_conversations_local_child_execution_idx
            ON chat_conversations ((meta->>'child_execution_id'), (meta->>'parent_execution_id'))
            WHERE source = 'local_child';
        CREATE INDEX IF NOT EXISTS chat_conversations_local_child_key_idx
            ON chat_conversations ((meta->>'parent_execution_id'), (meta->>'idempotency_key'))
            WHERE source = 'local_child';
    END IF;
END
$migration$;
