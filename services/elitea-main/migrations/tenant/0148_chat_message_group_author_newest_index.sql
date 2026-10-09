-- 0148_chat_message_group_author_newest_index.sql — index the "this user's
-- newest message in the project" lookup.
--
-- ADR-0029 decision 8 makes memory recall reserve one slot for the user's most
-- recently saved memory when it is newer than the user's previous turn in the
-- project (MemoriesRepo.ResolveCurrentMemoryRecall). The previous turn is the
-- newest chat_message_group the user's own participant rows authored. Recall
-- runs on EVERY turn admission (cloud) and EVERY local turn start (desktop).
--
-- chat_message_group (0123) has no index on `author_participant_id`, so the
-- lookup would scan every message group in the project once per turn. The
-- column order is the query's own: equality on `author_participant_id`, then
-- the newest `created_at`, so max(created_at) per participant is the first
-- index entry.
--
-- GUARDED. A tenant schema that predates 0123 on a mixed deployment still has
-- to migrate; the index is created only where the table and its columns exist.
-- IDEMPOTENT (IF NOT EXISTS). No BEGIN/COMMIT: the ledgered runner wraps each
-- file. Plain CREATE INDEX rather than CONCURRENTLY, which cannot run inside
-- that transaction.
DO $migration$
BEGIN
    IF to_regclass('chat_message_group') IS NOT NULL
       AND (
           SELECT count(*)
           FROM pg_attribute
           WHERE attrelid = to_regclass('chat_message_group')
             AND attname IN ('author_participant_id', 'created_at')
             AND NOT attisdropped
       ) = 2 THEN
        CREATE INDEX IF NOT EXISTS chat_message_group_author_newest_idx
            ON chat_message_group (author_participant_id, created_at DESC);
    END IF;
END
$migration$;
