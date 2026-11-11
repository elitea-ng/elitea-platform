-- 0240_idle_compaction_run_marker.sql — an idle compaction run marks its own
-- job (ADR-0050 decision 9b2, issue 1921).
--
-- The analytics read a request-log call as an idle compaction's only through
-- its elitea_runtime.agent_idle_compactions row (idle_execution_id). That row
-- CASCADE-deletes with the SOURCE's execution_jobs row (0230), so once source
-- jobs are pruned while the request logs are still in their window, the idle
-- run's calls fell back to the source's trigger_origin and counted the person
-- as active again.
--
--   idle_compaction   true on the execution_jobs rows of an idle compaction
--                     run. The admission transaction sets it when it moves
--                     the agent_idle_compactions row to ADMITTED. The run
--                     keeps its source's trigger_origin (the approval,
--                     vector-claim and browser policies read it there).
--
-- Backfill: the idle runs whose agent_idle_compactions row is still there.
-- A constant default adds the column without rewriting the table. No
-- permission. Idempotent. No BEGIN/COMMIT: the ledgered runner wraps the
-- file.

ALTER TABLE elitea_runtime.execution_jobs
    ADD COLUMN IF NOT EXISTS idle_compaction boolean NOT NULL DEFAULT false;

UPDATE elitea_runtime.execution_jobs AS job
SET idle_compaction = true
FROM elitea_runtime.agent_idle_compactions AS idle
WHERE idle.idle_execution_id = job.execution_id
  AND NOT job.idle_compaction;

COMMENT ON COLUMN elitea_runtime.execution_jobs.idle_compaction IS
    'True for an idle compaction run (ADR-0050 decision 9b2): the analytics tag its calls idle_compaction even after its source job is pruned.';
