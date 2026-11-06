-- 0228_browser_sessions.sql — the cloud browser session table (ADR-0049
-- decision 6; issue #1597).
--
--   elitea_runtime.browser_sessions
--       One row per cloud browser session: the session name (the id a viewer
--       opens), the run (execution, generation, project, the run's owner and
--       its conversation), the supervisor that holds it (its browser profile
--       audience, a configured name and never an address), the session's
--       exact spec (Main binds each view grant's digest to it), the state and
--       the times.
--
--       The worker writes it through ReportBrowserSession on its control
--       channel, under the live claim: LIVE after StartBrowserSession,
--       LINGERING after DestroyBrowserSession(RUN_ENDED), ENDED after
--       DestroyBrowserSession(NOW). Main fills the project, the owner and
--       the conversation from the execution kernel, never from the worker.
--
--       The view relay (GET /api/v2/browser_sessions/{session_id}/view)
--       reads the row on whichever elitea-main replica the viewer reaches,
--       so no replica holds session state of its own.
--
--   state       live, lingering or ended. A row moves forward only:
--               live -> lingering -> ended (repos/browser_sessions.go).
--   generation  the newest claim generation that reported the session. A
--               report under an older generation is refused, as the
--               supervisor fences calls.
--
-- A session is never reused (the supervisor never starts a name twice), so
-- the name is the key.
--
-- A row's life is bounded without any report: the supervisor retires every
-- session by started_at + spec_wall_seconds + its 10 minute linger, so the
-- relay treats a row past that as ended whatever its state says (a worker
-- that crashed or lost its lease reports nothing). A supervisor that answers
-- "not started" or "ended" to a view also marks the row ended.
--
-- Pruning: Main's execution replay retention pass (one minute) deletes, a
-- bounded batch at a time, a row ended more than a day ago and a row whose
-- bound passed more than a day ago (repos/browser_sessions.go Prune). The
-- two partial indexes below serve the two halves of that pass.
--
-- Shared, not tenant: the execution kernel this row belongs to is shared
-- (elitea_runtime.execution_jobs, 0030). No permission: only Main reads it.
-- No data. Idempotent. No BEGIN/COMMIT: the ledgered runner wraps the file.

CREATE SCHEMA IF NOT EXISTS elitea_runtime;

CREATE TABLE IF NOT EXISTS elitea_runtime.browser_sessions (
    session_id          text PRIMARY KEY CHECK (session_id ~ '^elitea-br-[0-9a-f]{24}$'),
    execution_id        text NOT NULL CHECK (char_length(execution_id) BETWEEN 1 AND 256),
    generation          bigint NOT NULL CHECK (generation > 0),
    tenant_id           text NOT NULL CHECK (char_length(tenant_id) BETWEEN 1 AND 256),
    project_id          integer NOT NULL CHECK (project_id > 0),
    -- The run's owner (execution_jobs.actor_id): a decimal user id.
    owner_actor_id      text NOT NULL CHECK (owner_actor_id ~ '^[1-9][0-9]{0,18}$'),
    -- The conversation the run answers (agent_execution_jobs.client_stream_id);
    -- NULL for a run with none.
    conversation_uuid   uuid,
    -- The supervisor's browser profile audience: one of Main's configured
    -- browser supervisors.
    supervisor_audience text NOT NULL CHECK (char_length(supervisor_audience) BETWEEN 1 AND 256),
    -- BrowserSessionSpecV1 as the worker sent it.
    spec_revision       integer NOT NULL CHECK (spec_revision = 1),
    spec_engine         integer NOT NULL CHECK (spec_engine IN (1, 2)),
    spec_cpu_millis     bigint NOT NULL CHECK (spec_cpu_millis > 0),
    spec_memory_bytes   bigint NOT NULL CHECK (spec_memory_bytes > 0),
    spec_wall_seconds   bigint NOT NULL CHECK (spec_wall_seconds > 0),
    spec_max_pages      integer NOT NULL CHECK (spec_max_pages > 0),
    state               text NOT NULL CHECK (state IN ('live', 'lingering', 'ended')),
    linger_until        timestamptz,
    started_at          timestamptz NOT NULL DEFAULT clock_timestamp(),
    ended_at            timestamptz,
    updated_at          timestamptz NOT NULL DEFAULT clock_timestamp(),
    CONSTRAINT browser_sessions_linger CHECK (linger_until IS NULL OR state <> 'live'),
    CONSTRAINT browser_sessions_end CHECK ((state = 'ended') = (ended_at IS NOT NULL))
);

-- The sessions of one run (the step record).
CREATE INDEX IF NOT EXISTS browser_sessions_execution
    ON elitea_runtime.browser_sessions (execution_id);

-- The retention pass: ended rows by their end, open rows by their start.
CREATE INDEX IF NOT EXISTS browser_sessions_ended_at
    ON elitea_runtime.browser_sessions (ended_at) WHERE ended_at IS NOT NULL;
CREATE INDEX IF NOT EXISTS browser_sessions_open_started_at
    ON elitea_runtime.browser_sessions (started_at) WHERE ended_at IS NULL;

COMMENT ON TABLE elitea_runtime.browser_sessions IS
    'Cloud browser sessions (ADR-0049 decision 6): session id -> run, supervisor audience, spec, state. Any elitea-main replica relays a viewer from this row.';
