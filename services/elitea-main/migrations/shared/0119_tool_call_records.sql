-- 0119_tool_call_records.sql — the durable per-tool-call record.
--
-- WHAT THIS IS FOR. The Analytics Tools tab. Until now
-- `AnalyticsRepo.GetToolAnalytics` answered `ErrNoSource`, and the note at the
-- top of internal/infra/db/repos/analytics.go says exactly why: there are two
-- places a tool actually runs and neither of them leaves a record the read can
-- group by.
--
--   1. THE EXPLICIT RUN — the toolkit test button, and MCP `tools/call`. It is
--      admitted as `elitea_runtime.execution_jobs` with
--      `capability_id = 'toolkit.call_tool.v1'`, which carries a project and an
--      `admitted_at`. It carries NEITHER `toolkit_id` NOR `tool_name`: that
--      capability deliberately owns no binding table, because it dispatches
--      inline and its command scalars never need to survive the request (see
--      internal/db/queries/runtime_toolkit_call_tool.sql).
--
--   2. THE AGENT TURN — a tool an agent calls inside a chat turn, which is the
--      BULK of tool usage. It is projected into
--      `p_<id>.chat_message_trace_step`, which records `tool_name` and
--      `started_at`/`finished_at` but no toolkit id, and covers chat turns
--      only.
--
-- So a Tools tab built on either producer alone under-reports by an unknown
-- factor, which is worse than the honest 501 it replaced. Issue 618 states the
-- conclusion this table acts on: the dimension needs a RECORD, not an event.
--
-- WHY SHARED AND NOT TENANT. The three reasons the analytics header gives for
-- reading `gateway.llm_request_logs` rather than the chat tables apply here
-- unchanged:
--
--   * ONE PROJECT COLUMN. `execution_jobs` has `resource_project_id` AND
--     `projection_project_id` and they can differ, so "the tools used by
--     project N" had no single answer. This table has `project_id` and nothing
--     else. Both producers set it to the same project the run was authorized
--     in.
--   * ONE CLOCK. The tenant chat tables store `timestamp`, the gateway tables
--     store `timestamptz`. One query spanning both needs dynamic SQL and an
--     explicit cast — the mismatch that yields a plausible wrong window rather
--     than an error. Every column here is `timestamptz`.
--   * ONE STATEMENT. A per-tenant table would make a project-wide read a loop
--     over schemas that a new tenant silently drops out of.
--
-- WHAT IS DELIBERATELY NULLABLE, AND WHY THAT IS NOT THE OLD GAP RETURNING.
-- `toolkit_id` is NULL for an agent-turn row. The worker's tool-call metadata
-- carries `toolkit_name` and `toolkit_type` (both in the trace projector's
-- attribute allowlist) and no id, and resolving a name back to a row at write
-- time would be a guess this table must not store as a fact. So the identity a
-- producer HAS is written and the identity it does not have is left NULL, and
-- the read groups by (toolkit_id, toolkit_name, tool_name) — an aggregate that
-- is complete for `tool_name`, which is what the Tools tab shows. The old
-- refusal was not "the id is sometimes missing"; it was "half the calls are
-- recorded nowhere at all", and that is what stops being true here.
--
-- NO BACKFILL. Nothing written before this migration identifies a tool call, so
-- there is nothing to backfill and none is invented. A window that ends before
-- this migration was applied is reported as UNAVAILABLE — the read finds the
-- moment in `elitea_runtime.schema_migrations` — never as "no tool ran".
--
-- IDEMPOTENT throughout. No BEGIN/COMMIT: the ledgered runner executes each
-- file inside one transaction with its ledger row (migrate/runner.go apply).

CREATE SCHEMA IF NOT EXISTS elitea_runtime;

CREATE TABLE IF NOT EXISTS elitea_runtime.tool_call_records (
    id            BIGSERIAL   PRIMARY KEY,

    -- The project the call was authorized in. For the explicit run this is the
    -- admission identity's project, which is also its tenant and its
    -- projection; for the agent turn it is the tenant schema the trace step
    -- was projected into.
    project_id    BIGINT      NOT NULL,

    -- Which producer wrote the row. It is stored rather than inferred because
    -- the acceptance test of issue 618 is "a tool call made OUTSIDE a chat turn
    -- appears in the breakdown", and that assertion needs to be able to name
    -- the two halves apart.
    source        TEXT        NOT NULL,

    -- The producing row's own natural key, so a replay updates its record
    -- instead of adding a second one. The explicit run uses its execution id,
    -- which an idempotent re-admission reuses. The agent turn uses
    -- '<message_group_id>:<run_id>', the same natural key the trace projector
    -- reconciles its rows by — a streaming turn re-projects the same tool call
    -- many times as it fills in.
    source_ref    TEXT        NOT NULL,

    toolkit_id    BIGINT,
    toolkit_name  TEXT,
    toolkit_type  TEXT,
    tool_name     TEXT        NOT NULL,

    -- started_at is never NULL: a call the producer cannot time is recorded at
    -- the moment its output frame arrived rather than dropped, because a
    -- missing timestamp must not silently shrink a count.
    started_at    TIMESTAMPTZ NOT NULL,
    -- finished_at is NULL while a call is still running. Duration is derived at
    -- read time from the two columns rather than stored, so there is one place
    -- a wrong window can come from instead of two.
    finished_at   TIMESTAMPTZ,

    is_error      BOOLEAN     NOT NULL DEFAULT false,

    -- The person the run was made for, when the producer knows one. The agent
    -- turn knows only its message group's participant, so it leaves this NULL
    -- rather than resolve it through a join that a deleted conversation would
    -- change the answer of.
    actor_user_id BIGINT,
    -- The runtime execution, when there is one. Present for every explicit run;
    -- NULL for an agent turn's inner tool call, which is a step of an execution
    -- rather than one of its own.
    execution_id  TEXT,

    recorded_at   TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),

    CONSTRAINT tool_call_records_project_positive
        CHECK (project_id > 0),
    CONSTRAINT tool_call_records_source_known
        CHECK (source IN ('explicit_run', 'agent_turn')),
    CONSTRAINT tool_call_records_source_ref_present
        CHECK (source_ref <> ''),
    CONSTRAINT tool_call_records_tool_name_present
        CHECK (tool_name <> ''),
    CONSTRAINT tool_call_records_toolkit_positive
        CHECK (toolkit_id IS NULL OR toolkit_id > 0),
    CONSTRAINT tool_call_records_finish_after_start
        CHECK (finished_at IS NULL OR finished_at >= started_at),
    CONSTRAINT tool_call_records_identity_uc
        UNIQUE (project_id, source, source_ref)
);

-- The read is always "one project, one window, grouped by tool", so the window
-- scan is the index this table exists to serve.
CREATE INDEX IF NOT EXISTS tool_call_records_project_started_idx
    ON elitea_runtime.tool_call_records (project_id, started_at DESC);
