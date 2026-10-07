-- 0154_agent_stop_question_author.sql — remember whose question a stop removed.
--
-- WHY (client contract 1.3, cancelChatExecution). A stop is admitted for the
-- conversation's author OR the author of the question being answered
-- (internal/db/queries/agent_cancel.sql CancelCurrentAgentExecution). A stop
-- of a turn with no output deletes the question and the empty answer, so a
-- repeated stop can no longer find them, and the contract still promises the
-- same caller 204. The replay check (IsCurrentAgentCancellationReplay)
-- resolved the conversation author through the binding, but stood in the
-- job's actor for the question's author. They differ for a regeneration: the
-- conversation's owner may regenerate another member's question, so the job
-- runs as the owner while the question is the member's. The member's stop
-- worked, and the member's retry answered 409.
--
-- stop_question_author_id is the question author's user id, written by the
-- stop itself while the question still exists, so the replay admits exactly
-- the principals the stop does. NULL until a stop through that route, and
-- for a question whose author participant id is not a user id.
--
-- NO NEW PERMISSION. NO BACKFILL: no stop before this file recorded it, and
-- the replay falls back to the job's actor for such a row.
--
-- IDEMPOTENT. No BEGIN/COMMIT: the ledgered runner executes each file inside
-- one transaction with its ledger row (migrate/runner.go apply). Adding a
-- nullable column with no default rewrites nothing.

ALTER TABLE elitea_runtime.agent_execution_jobs
    ADD COLUMN IF NOT EXISTS stop_question_author_id BIGINT;
