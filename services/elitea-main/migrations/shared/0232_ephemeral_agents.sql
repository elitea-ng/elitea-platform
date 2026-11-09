-- 0232_ephemeral_agents.sql — one row per sub-agent a run started with
-- spawn_agent (ADR-0050 decisions 4 and 9d, issue 1614).
--
--   elitea_runtime.ephemeral_agents
--       One row per spawn: the run (execution, the command that ran it,
--       its project, conversation and parent agent), the child's id, label,
--       display name and avatar key, the digest of its profile, the digest
--       of its brief with the ids the brief named (message ids, attachment
--       ids, memory entry ids and a count of facts), and how it ended (its
--       status, steps, tokens and error code).
--
--       It never holds the soul, a quote, a fact or the task: the digests
--       name them, and the run's step records (0205) hold the spawn_agent
--       call itself.
--
--       The worker writes it through ReportEphemeralAgents on its control
--       channel, under the live claim, before PrepareSettlement. A report is
--       a cumulative snapshot, so a retried report upserts the same rows:
--       (execution_id, agent_id) is the key. Main fills the project, the
--       conversation and the parent agent from the execution kernel, never
--       from the worker.
--
--   retention_class   the data class the row's retention follows. Today
--                     always 'step_record': a spawn is kept as long as the
--                     run's step records are.
--
-- Shared, not tenant: the execution kernel this row belongs to is shared
-- (elitea_runtime.execution_jobs, 0030). No permission: only Main reads it.
-- No data. Idempotent. No BEGIN/COMMIT: the ledgered runner wraps the file.

CREATE SCHEMA IF NOT EXISTS elitea_runtime;

CREATE TABLE IF NOT EXISTS elitea_runtime.ephemeral_agents (
    execution_id            text NOT NULL CHECK (char_length(execution_id) BETWEEN 1 AND 256),
    -- The child's id in the run: a_ and 8 hex digits.
    agent_id                text NOT NULL CHECK (agent_id ~ '^a_[0-9a-f]{8}$'),
    -- The command (generation) whose claim reported the row last.
    command_id              text NOT NULL CHECK (char_length(command_id) BETWEEN 1 AND 256),
    generation              bigint NOT NULL CHECK (generation > 0),
    project_id              bigint NOT NULL CHECK (project_id > 0),
    -- The conversation the run answers (agent_execution_jobs.client_stream_id);
    -- NULL for a run with none.
    conversation_uuid       uuid,
    -- The agent that started the child (agent_execution_jobs.application_id);
    -- NULL for an ad hoc run.
    parent_application_id   bigint CHECK (parent_application_id IS NULL OR parent_application_id > 0),
    parent_call_id          text NOT NULL CHECK (char_length(parent_call_id) BETWEEN 1 AND 256),
    ordinal                 integer NOT NULL CHECK (ordinal BETWEEN 1 AND 64),
    label                   text CHECK (label IS NULL OR char_length(label) BETWEEN 1 AND 60),
    display_name            text CHECK (display_name IS NULL OR char_length(display_name) BETWEEN 1 AND 60),
    avatar                  text CHECK (avatar IS NULL OR octet_length(avatar) BETWEEN 1 AND 128),
    profile_digest          text NOT NULL CHECK (profile_digest ~ '^[0-9a-f]{64}$'),
    brief_digest            text CHECK (brief_digest IS NULL OR brief_digest ~ '^[0-9a-f]{64}$'),
    brief_refs              jsonb CHECK (brief_refs IS NULL OR jsonb_typeof(brief_refs) = 'object'),
    status                  text NOT NULL CHECK (status IN ('running', 'completed', 'failed', 'budget_exhausted', 'cancelled')),
    steps                   integer NOT NULL DEFAULT 0 CHECK (steps >= 0),
    input_tokens            bigint NOT NULL DEFAULT 0 CHECK (input_tokens >= 0),
    output_tokens           bigint NOT NULL DEFAULT 0 CHECK (output_tokens >= 0),
    error_code              text CHECK (error_code IS NULL OR error_code ~ '^[a-z][a-z0-9_]{0,63}$'),
    started_at              timestamptz NOT NULL,
    finished_at             timestamptz,
    retention_class         text NOT NULL DEFAULT 'step_record' CHECK (retention_class IN ('step_record')),
    created_at              timestamptz NOT NULL DEFAULT clock_timestamp(),
    updated_at              timestamptz NOT NULL DEFAULT clock_timestamp(),
    PRIMARY KEY (execution_id, agent_id),
    CONSTRAINT ephemeral_agents_brief CHECK ((brief_digest IS NULL) = (brief_refs IS NULL))
);

-- A conversation's sub-agents, newest first.
CREATE INDEX IF NOT EXISTS ephemeral_agents_conversation
    ON elitea_runtime.ephemeral_agents (project_id, conversation_uuid, started_at DESC)
    WHERE conversation_uuid IS NOT NULL;

COMMENT ON TABLE elitea_runtime.ephemeral_agents IS
    'One row per sub-agent a run started with spawn_agent (ADR-0050 decisions 4 and 9d): its profile and brief by digest and reference, and how it ended. Never the soul, a quote, a fact or the task.';
