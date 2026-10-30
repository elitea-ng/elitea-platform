-- 0213_agent_circuit_breaker.sql — the agent circuit breaker (ADR-0041
-- decision 2, issue #1498).
--
-- The main.agent_breaker routine (every minute, one replica per pass) reads
-- the outcomes of agent runs and trips a rule by writing a suspension of
-- source breaker at agent scope (shared 0194). This file adds:
--
--   elitea_runtime.agent_breaker_runs
--       One row per settled agent run (execution job generation) that the
--       routine has seen: the project and agent, when the run started and
--       ended, its terminal state, and the longest streak of consecutive
--       failures of one tool inside the run (from run_steps, shared 0205).
--       The routine copies the runs that ended in the last few minutes on
--       every pass (ON CONFLICT updates the row, so a late step record is
--       counted), and deletes rows older than seven days. The table is the
--       breaker's own window: a run that ended before the routine was first
--       enabled is never in it, so turning the breaker on does not judge an
--       agent on old runs.
--
--   execution_jobs_agent_settled (index)
--       The routine's read of "agent runs that ended since <t>". Partial: only
--       application runs that ended. It is built here, in the upgrade
--       transaction (the runner has no non-transactional mode, so it cannot be
--       CONCURRENTLY): on a large execution_jobs table the build holds writes
--       to that table for its duration. docs/UPGRADING.md says so.
--
--   execution_suspensions.detail
--       The rule and the observed values of a breaker suspension, as a JSON
--       object (rule, observed, threshold, window, unit, tool_name). NULL for
--       every other source.
--
--   execution_suspensions.resume_approved_by / resume_approved_at
--       The first of two persons for a two_person suspension that the system
--       wrote (actor_id 0: the breaker). The rule "a platform administrator
--       who did not suspend it" means nothing when nobody suspended it, so a
--       system-written two_person suspension needs two different platform
--       administrators: the first records the approval, the second resumes.
--       The CHECK below is the database's copy of that rule.
--
--   elitea_lifecycle.agent_registry.circuit_breaker
--       The per-agent override of the breaker rules (a JSON object, empty for
--       none). The server accepts only an override that is tighter than the
--       tier default; the routine applies the tighter of the two.
--
-- No permission: the rules are a platform-config section under
-- configuration.governance, the override is a registry edit, and the
-- suspension uses safety.suspend (0194).
--
-- Additive only. Idempotent. No BEGIN/COMMIT: the ledgered runner wraps the
-- file.

CREATE SCHEMA IF NOT EXISTS elitea_runtime;

CREATE TABLE IF NOT EXISTS elitea_runtime.agent_breaker_runs (
    execution_id        text NOT NULL CHECK (length(execution_id) BETWEEN 1 AND 128),
    generation          bigint NOT NULL CHECK (generation > 0),
    project_id          bigint NOT NULL CHECK (project_id > 0),
    application_id      bigint NOT NULL CHECK (application_id > 0),
    admitted_at         timestamptz NOT NULL,
    settled_at          timestamptz NOT NULL,
    state               text NOT NULL CHECK (state IN ('SUCCEEDED', 'FAILED', 'CANCELLED')),
    tool_failure_streak integer NOT NULL DEFAULT 0 CHECK (tool_failure_streak >= 0),
    tool_failure_name   text NOT NULL DEFAULT '' CHECK (length(tool_failure_name) <= 128),
    recorded_at         timestamptz NOT NULL DEFAULT clock_timestamp(),
    PRIMARY KEY (execution_id, generation)
);

-- The window of one agent: its latest runs, newest first.
CREATE INDEX IF NOT EXISTS agent_breaker_runs_agent
    ON elitea_runtime.agent_breaker_runs (project_id, application_id, settled_at DESC);
-- The seven-day prune.
CREATE INDEX IF NOT EXISTS agent_breaker_runs_settled
    ON elitea_runtime.agent_breaker_runs (settled_at);

CREATE INDEX IF NOT EXISTS execution_jobs_agent_settled
    ON elitea_runtime.execution_jobs (settled_at)
    WHERE capability_id = 'agent.execute.application.v1' AND settled_at IS NOT NULL;

ALTER TABLE elitea_runtime.execution_suspensions
    ADD COLUMN IF NOT EXISTS detail jsonb,
    ADD COLUMN IF NOT EXISTS resume_approved_by bigint,
    ADD COLUMN IF NOT EXISTS resume_approved_at timestamptz;

DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint
        WHERE conname = 'execution_suspensions_detail_object'
          AND conrelid = 'elitea_runtime.execution_suspensions'::regclass
    ) THEN
        ALTER TABLE elitea_runtime.execution_suspensions
            ADD CONSTRAINT execution_suspensions_detail_object
            CHECK (detail IS NULL OR (jsonb_typeof(detail) = 'object' AND octet_length(detail::text) <= 4096));
    END IF;
    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint
        WHERE conname = 'execution_suspensions_resume_approval'
          AND conrelid = 'elitea_runtime.execution_suspensions'::regclass
    ) THEN
        ALTER TABLE elitea_runtime.execution_suspensions
            ADD CONSTRAINT execution_suspensions_resume_approval
            CHECK ((resume_approved_by IS NULL) = (resume_approved_at IS NULL)
                   AND (resume_approved_by IS NULL OR resume_approved_by > 0));
    END IF;
    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint
        WHERE conname = 'execution_suspensions_system_two_person'
          AND conrelid = 'elitea_runtime.execution_suspensions'::regclass
    ) THEN
        ALTER TABLE elitea_runtime.execution_suspensions
            ADD CONSTRAINT execution_suspensions_system_two_person
            CHECK (resume_requires <> 'two_person' OR actor_id <> 0 OR resumed_by IS NULL
                   OR (resume_approved_by IS NOT NULL AND resume_approved_by <> resumed_by));
    END IF;
END
$$;

ALTER TABLE elitea_lifecycle.agent_registry
    ADD COLUMN IF NOT EXISTS circuit_breaker jsonb NOT NULL DEFAULT '{}'::jsonb;

DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint
        WHERE conname = 'agent_registry_circuit_breaker_object'
          AND conrelid = 'elitea_lifecycle.agent_registry'::regclass
    ) THEN
        ALTER TABLE elitea_lifecycle.agent_registry
            ADD CONSTRAINT agent_registry_circuit_breaker_object
            CHECK (jsonb_typeof(circuit_breaker) = 'object' AND octet_length(circuit_breaker::text) <= 1024);
    END IF;
END
$$;
