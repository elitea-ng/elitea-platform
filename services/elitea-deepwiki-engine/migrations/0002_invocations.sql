-- Durable invocation state.
--
-- spec-provider-service, "Legacy task lifecycle and restart behavior":
--
--   DeepWiki generation, Inventory ingestion, and any provider operation with
--   an external side effect MUST have durable provider-side operation state or
--   an external durable job handle. A restarted service MUST NOT silently
--   reinterpret a known accepted operation as never having existed.
--
-- The legacy service could not satisfy that. Its registry was an in-process
-- dict inside an Arbiter TaskNode: a restart lost every accepted invocation,
-- and the next poll returned 404 — indistinguishable from an id that was never
-- issued. `GET /health` has been reporting `durable_invocations: false` since
-- the SPI shell landed precisely so nobody could mistake the gap for a feature.
--
-- WHY THIS IS NOT THE PRODUCT DATABASE.
--
-- Same reasoning as the index tables in 0001: this is service-operational
-- state, owned and migrated by the service. It lives in the dedicated
-- `deepwiki` database.
--
-- WHY custom_events IS A TABLE AND NOT A JSONB COLUMN.
--
-- The events are drained on read — a poll returns what accumulated since the
-- previous poll and clears it. Doing that to a jsonb array means read, modify,
-- write, under a lock, on the hottest path in the service. Rows let a drain be
-- a single DELETE ... RETURNING, which is atomic without one: two concurrent
-- pollers cannot both receive the same event, and neither can lose one.
--
-- Read-once remains the contract. It is the LEGACY contract, and the P0
-- fixtures pin it. Making events durable does not make them re-readable.

CREATE TABLE IF NOT EXISTS invocations (
    invocation_id  TEXT        PRIMARY KEY,
    toolkit_name   TEXT        NOT NULL,
    tool_name      TEXT        NOT NULL,
    -- pending | running | stopped. 'pruned' is not a state: it is the absence
    -- of the row, which is what the legacy registry's pruning produced.
    status         TEXT        NOT NULL,
    stop_requested BOOLEAN     NOT NULL DEFAULT FALSE,
    -- The terminal body, verbatim, as the poll returns it.
    result         JSONB,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    finished_at    TIMESTAMPTZ,
    -- Which process accepted it. On restart, a row still 'running' whose owner
    -- is this process's predecessor is one nobody is working on any more; the
    -- reconciler turns those into a terminal error rather than leaving a poll
    -- to hang forever on InProgress.
    owner          TEXT
);

-- The poll is keyed by all three segments, because the wire path carries them
-- and an id belonging to a different tool must 404 exactly as an unknown id
-- does — the legacy behaviour the fixtures record.
CREATE INDEX IF NOT EXISTS idx_invocations_lookup
    ON invocations (toolkit_name, tool_name, invocation_id);

-- Housekeeping scans terminal rows by age.
CREATE INDEX IF NOT EXISTS idx_invocations_finished
    ON invocations (finished_at) WHERE finished_at IS NOT NULL;

-- Restart reconciliation scans in-flight rows by owner.
CREATE INDEX IF NOT EXISTS idx_invocations_owner
    ON invocations (owner) WHERE finished_at IS NULL;

CREATE TABLE IF NOT EXISTS invocation_events (
    id            BIGINT      GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    invocation_id TEXT        NOT NULL
        REFERENCES invocations (invocation_id) ON DELETE CASCADE,
    message       TEXT        NOT NULL,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS idx_invocation_events_drain
    ON invocation_events (invocation_id, id);
