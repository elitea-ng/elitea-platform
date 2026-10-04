-- 0140_execution_trigger_origin.sql — record how a runtime execution started,
-- and index the request log for one execution's calls.
--
-- WHY (legacy issues 6802 and 6881). An unattended run executes as the person
-- who configured it: a pipeline schedule runs as its author, an inbound
-- webhook trigger as the trigger's creator, an index ingest as the person who
-- started the index. Billing must keep that attribution. The Analytics "active
-- users", Users tab, daily active users and top adopters must not: they
-- counted a person as active because a cron job ran under their name.
--
-- Nothing recorded HOW an execution started, so the analytics reads could not
-- tell the two apart. trigger_origin is that record:
--
--   manual    a person started it (the chat composer, the UI)
--   api       a programmatic client started it (the MCP tools/call path)
--   schedule  a pipeline schedule fired it
--   webhook   an inbound pipeline trigger fired it
--   index     an index ingest
--
-- `manual` and `api` are both people acting. The other three are automated.
-- The analytics reads resolve gateway.llm_request_logs.execution_id to this
-- column at READ time (internal/infra/db/repos/analytics.go), the same way
-- they already resolve an execution to its agent.
--
-- BACKFILL. Two origins are recoverable from rows that already exist:
--
--   * every `index.ingest.v1` execution is an index ingest;
--   * public.pipeline_runs (shared 0124) records the origin of every
--     unattended pipeline run it tracked, keyed by execution id.
--
-- Everything else stays `manual`, which is what it was counted as before.
--
-- THE SECOND INDEX. The per-execution analytics read
-- (GET /analytics_execution/prompt_lib/{projectID}/{executionID}) selects one
-- execution's calls, and its evaluation-run twin selects every id under an
-- `eval:<run>:` prefix. 0100's index is (project_id, occurred_at), which
-- serves a WINDOW, not an id: without this one each lookup scans the
-- project's whole retained log. text_pattern_ops makes the same index serve
-- the equality and the prefix LIKE.
--
-- NO NEW PERMISSION. The reads that use this column are gated on the
-- analytics permission shared 0063 already grants.
--
-- IDEMPOTENT. No BEGIN/COMMIT: the ledgered runner executes each file inside
-- one transaction with its ledger row (migrate/runner.go apply).

ALTER TABLE elitea_runtime.execution_jobs
    ADD COLUMN IF NOT EXISTS trigger_origin TEXT NOT NULL DEFAULT 'manual';

DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint
         WHERE conname = 'execution_jobs_trigger_origin'
           AND conrelid = 'elitea_runtime.execution_jobs'::regclass
    ) THEN
        ALTER TABLE elitea_runtime.execution_jobs
            ADD CONSTRAINT execution_jobs_trigger_origin
            CHECK (trigger_origin IN ('manual', 'api', 'schedule', 'webhook', 'index'));
    END IF;
END
$$;

UPDATE elitea_runtime.execution_jobs
   SET trigger_origin = 'index'
 WHERE capability_id = 'index.ingest.v1'
   AND trigger_origin = 'manual';

UPDATE elitea_runtime.execution_jobs AS job
   SET trigger_origin = CASE run.origin
                            WHEN 'Schedule' THEN 'schedule'
                            ELSE 'webhook'
                        END
  FROM public.pipeline_runs AS run
 WHERE run.execution_id = job.execution_id
   AND run.origin IN ('Schedule', 'Webhook')
   AND job.trigger_origin = 'manual';

CREATE INDEX IF NOT EXISTS idx_llm_request_logs_project_execution
    ON gateway.llm_request_logs (project_id, execution_id text_pattern_ops)
    WHERE execution_id IS NOT NULL;
