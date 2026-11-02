-- 0218_agent_continuation_paused_writer.sql — the paused writer a continuation re-enters (FU-269).
--
-- WHY. A continuation (an approval, an answer, an MCP authorization or a
-- static-tools decision) re-enters the root checkpoint row the paused
-- execution wrote. When the continuation is lost before its own first root
-- save, the Worker can re-run it from that row only if Main names the row's
-- writer: the paused execution (chat_message_group.task_id) and the
-- generation its paused terminal settled under. Main reads both in the
-- admitting transaction, under the response lock, before the admission
-- consumes the pause, and stores them here. The claim receipt serves them
-- (control.proto ClaimReceiptV1.paused_writer); the client never supplies
-- them.
--
-- WRITTEN ONCE. Only the admitting transaction sets them
-- (repos/continuation_paused_writer.go recordPausedWriter); nothing updates
-- them afterwards. NULL for every other start, and for a continuation whose
-- response has no single settled paused terminal (it then behaves as before).
--
-- ROLLING. An older Main never reads or writes these columns: its one insert
-- names its columns (queries/runtime_agent_execution.sql
-- InsertAgentExecutionBinding) and no query copies the row.
--
-- NO BACKFILL. IDEMPOTENT. No BEGIN/COMMIT: the ledgered runner executes each
-- file inside one transaction with its ledger row (migrate/runner.go apply).
-- Adding nullable columns with no default rewrites nothing; the checks are
-- trivially true on existing rows.

ALTER TABLE elitea_runtime.agent_execution_jobs
    ADD COLUMN IF NOT EXISTS paused_execution_id TEXT,
    ADD COLUMN IF NOT EXISTS paused_generation BIGINT;

ALTER TABLE elitea_runtime.agent_execution_jobs
    DROP CONSTRAINT IF EXISTS agent_execution_jobs_paused_writer;

ALTER TABLE elitea_runtime.agent_execution_jobs
    ADD CONSTRAINT agent_execution_jobs_paused_writer CHECK (
        (paused_execution_id IS NULL AND paused_generation IS NULL)
        OR (
            paused_execution_id ~ '^[0-9a-f]{32}$'
            AND paused_generation >= 1
            AND paused_execution_id <> execution_id
        )
    );
