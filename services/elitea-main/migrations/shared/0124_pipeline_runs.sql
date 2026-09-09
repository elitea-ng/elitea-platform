-- 0124_pipeline_runs.sql — tracks one row per unattended pipeline run, so its
-- eventual outcome can be reported as pipeline.run.succeeded/failed once the
-- claim-fence settlement machinery finishes it (#876's still-missing half).
--
-- WHY THIS TABLE EXISTS. internal/events.EventPipelineRunSucceeded and
-- EventPipelineRunFailed were declared in the catalogue but never wired: the
-- claim-fence/settlement engine (internal/application/execution/
-- settlement.go, fed by internal/application/output.AgentExecutionService and
-- RuntimeFailureService) is capability-generic — it settles agent turns,
-- config validations, index ingests and toolkit-tool calls alike, keyed only
-- by an opaque execution_id — and carries no notion of "this execution is a
-- PIPELINE run", let alone which pipeline, which version, or which
-- conversation. Threading that context through the fence-verification
-- contract itself was exactly what the first half of #876 declined to do
-- (dispatcher.go's own header: "neither exposes a single safe,
-- already-isolated hook... without editing that fencing logic itself").
--
-- This table is the isolated hook instead: internal/api/v2/pipelinetriggers'
-- admit() (the ONE admission path both the inbound trigger and the schedule
-- tick use) writes one row here the moment it emits pipeline.run.started —
-- an ordinary INSERT, alongside the chat rows it already writes, nowhere
-- near elitea_runtime's claim-fence tables. When settlement later finishes
-- (execution.SettlementService's new AfterSettle hook — see
-- internal/application/pipelineruns), the hook looks this row up by
-- execution_id: no row means "not a pipeline run, say nothing"; a row means
-- "here is the pipeline id, version, conversation and start time this
-- execution_id belongs to".
--
-- error_summary is populated SEPARATELY and EARLIER, by
-- output.RuntimeFailureService's own new observer hook, from
-- RuntimeFailureFrame.Failure.SafeMessage — the one place that text exists,
-- on the OUTPUT plane, which the PrepareSettlement protocol guarantees has
-- already committed (its own query requires the terminal output row's
-- `projected_at IS NOT NULL`) before a worker's settlement RPC can succeed.
-- So by the time the AfterSettle hook reads this row, a FAILED run's
-- error_summary is already there to include in the event payload.
--
-- event_emitted_at is the DEDUPLICATION guard. SettlementService.PrepareSettlement
-- is idempotent (a worker may replay a settlement RPC after a crash or a lost
-- ack), and its AfterSettle hooks fire on every successful call, replay
-- included — the settlement repository's own contract does not distinguish
-- "newly inserted" from "already existed, returned unchanged" to its caller,
-- and teaching it to would touch the exact fence-verification transaction
-- this file exists to stay out of. The hook instead claims emission
-- ATOMICALLY here: `UPDATE ... SET event_emitted_at = now() WHERE
-- execution_id = $1 AND event_emitted_at IS NULL RETURNING ...`. Zero rows
-- back means either "not a pipeline run" or "already emitted", and the hook
-- treats both the same way: say nothing.
--
-- WHY public., NOT a tenant schema. elitea_runtime.execution_settlements
-- (0028/0032 and onward) is itself a SHARED, cross-tenant table keyed only by
-- execution_id/generation — the claim-fence machinery has no tenant-schema
-- concept at all, only a Fence with no project id on it. project_id has to
-- come from somewhere else, so this table (like 0122's webhooks and
-- webhook_deliveries) carries it as a plain text column and lives in the
-- shared schema, matching 0122's `project_id text` reasoning verbatim: the
-- pipelinetriggers admission path takes projectID as a Go string with no
-- int64 parse in between.

CREATE TABLE IF NOT EXISTS public.pipeline_runs (
    execution_id      text PRIMARY KEY,
    project_id        text NOT NULL,
    application_id    bigint NOT NULL,
    version_id        bigint NOT NULL,
    conversation_uuid text NOT NULL,
    -- 'Webhook' or 'Schedule' (pipelinetriggers.OriginWebhook/OriginSchedule)
    -- — mirrors pipeline.run.started's own `origin` field so an operator
    -- correlating the two events sees the same vocabulary.
    origin            text NOT NULL,
    started_at        timestamptz NOT NULL DEFAULT now(),
    -- Populated only for a FAILED run, before settlement — see header.
    error_summary     text,
    -- NULL until the AfterSettle hook claims this row for exactly one
    -- emission. Not indexed on its own: every read of this column is
    -- filtered by execution_id (the primary key) first.
    event_emitted_at  timestamptz
);
