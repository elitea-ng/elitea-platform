-- 0230_agent_idle_compactions.sql — idle compaction per (conversation,
-- agent) in the cloud (ADR-0050 decision 9b2, issue 1610).
--
-- WHY. An agent's prompt cache lives for its model's cache lifetime (five
-- minutes by default) after its last model request started. Summarising the
-- conversation while that cache is still warm costs a cached read of the last
-- request plus one short continuation prompt; summarising it on the next turn,
-- after the cache expired, costs the whole conversation again at full input
-- price. So when an agent's turn settles, Main arms one idle check for that
-- agent in that conversation, due at its last request's start plus a delay
-- inside the cache lifetime (internal/domain/idlecompaction).
--
-- ONE ROW PER CHECK, ONE ACTIVE CHECK PER AGENT. A settled turn writes a
-- QUEUED row and ends the agent's earlier QUEUED row (CANCELLED,
-- superseded_by_agent_turn). A new turn of the SAME participant cancels its
-- QUEUED row in that turn's admission transaction; a peer's turn does not.
-- The sweep (agentexecution.IdleCompactionAdmitter) admits a due row as a
-- quiet `idle_compaction` continuation of the source execution (no user turn,
-- no tools, no transcript write), with the idempotency key below, and moves it
-- QUEUED -> ADMITTED with the job in one transaction. The settled idle run
-- moves it to DONE and records the measurement it produced
-- (result_context), which the participant list's per-agent meter reads.
--
-- No backfill, no permission: nothing is armed before this release.
-- Additive only. No BEGIN/COMMIT: the ledgered runner wraps the file.

CREATE TABLE IF NOT EXISTS elitea_runtime.agent_idle_compactions (
    id BIGSERIAL PRIMARY KEY,
    project_id BIGINT NOT NULL CHECK (project_id > 0),
    conversation_uuid UUID NOT NULL,
    -- The chat_participants row id (in the project's tenant schema) of the
    -- agent, as agent_execution_jobs.participant_id (shared 0227).
    participant_id INTEGER NOT NULL CHECK (participant_id > 0),
    -- The settled turn the check continues, and its last model request.
    source_execution_id TEXT NOT NULL CHECK (source_execution_id ~ '^[0-9a-f]{32}$'),
    source_generation BIGINT NOT NULL CHECK (source_generation > 0),
    request_started_at TIMESTAMPTZ NOT NULL,
    cache_ttl_seconds INTEGER NOT NULL CHECK (cache_ttl_seconds BETWEEN 60 AND 86400),
    due_at TIMESTAMPTZ NOT NULL,
    idempotency_key TEXT NOT NULL UNIQUE,
    -- QUEUED: armed; ADMITTED: the idle job exists; DONE: it settled;
    -- SKIPPED: the sweep found it not worth running (skip_code);
    -- CANCELLED: the same agent's next turn, or its next settled turn,
    -- replaced it (skip_code).
    state TEXT NOT NULL CHECK (state IN ('QUEUED', 'ADMITTED', 'DONE', 'SKIPPED', 'CANCELLED')),
    skip_code TEXT CHECK (skip_code ~ '^[a-z][a-z0-9_]{0,63}$'),
    idle_execution_id TEXT CHECK (idle_execution_id ~ '^[0-9a-f]{32}$'),
    -- The measurement the idle run produced (contextsettings.RuntimeContext);
    -- NULL until DONE, and when the run produced none.
    result_context JSONB,
    admission_attempts INTEGER NOT NULL DEFAULT 0 CHECK (admission_attempts BETWEEN 0 AND 1000),
    lease_until TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    FOREIGN KEY (source_execution_id, source_generation)
        REFERENCES elitea_runtime.execution_jobs(execution_id, generation) ON DELETE CASCADE,
    -- The key is derived, never chosen: idle-compaction/{source execution}.
    CONSTRAINT agent_idle_compactions_key CHECK (
        idempotency_key = 'idle-compaction/' || source_execution_id),
    CONSTRAINT agent_idle_compactions_due CHECK (due_at >= request_started_at),
    CONSTRAINT agent_idle_compactions_state_coherence CHECK (
        (state = 'QUEUED' AND idle_execution_id IS NULL AND skip_code IS NULL)
        OR (state IN ('ADMITTED', 'DONE') AND idle_execution_id IS NOT NULL AND skip_code IS NULL)
        OR (state IN ('SKIPPED', 'CANCELLED') AND skip_code IS NOT NULL))
);

-- One active check per (conversation, agent).
CREATE UNIQUE INDEX IF NOT EXISTS agent_idle_compactions_one_active
    ON elitea_runtime.agent_idle_compactions (project_id, conversation_uuid, participant_id)
    WHERE state IN ('QUEUED', 'ADMITTED');

-- The sweep reads due rows first.
CREATE INDEX IF NOT EXISTS agent_idle_compactions_due_queued
    ON elitea_runtime.agent_idle_compactions (due_at)
    WHERE state = 'QUEUED';

-- The settle hook finds the row of an idle run.
CREATE INDEX IF NOT EXISTS agent_idle_compactions_idle_execution
    ON elitea_runtime.agent_idle_compactions (idle_execution_id)
    WHERE idle_execution_id IS NOT NULL;

-- The meter reads each agent's latest DONE row of a conversation.
CREATE INDEX IF NOT EXISTS agent_idle_compactions_conversation_done
    ON elitea_runtime.agent_idle_compactions (project_id, conversation_uuid, participant_id, updated_at DESC)
    WHERE state = 'DONE';

-- FK support for job deletes (project deprovisioning).
CREATE INDEX IF NOT EXISTS agent_idle_compactions_source_execution
    ON elitea_runtime.agent_idle_compactions (source_execution_id, source_generation);
