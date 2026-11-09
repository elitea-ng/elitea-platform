-- 0240_agent_heartbeat_settings.sql — the heartbeat of each proactive agent
-- in this project, and the project switch (ADR-0050 decision 2, issue 1618).
--
-- agent_heartbeat_settings
--   One row per agent (the registry key, application_id) whose heartbeat an
--   owner turned on. The `proactivity` block itself is in the version meta:
--   the heartbeat reads the block of the agent's current RELEASE at every
--   wake, never the block of a draft. This row holds the state the block
--   does not: who turned it on, the mandate it acts under and the clock.
--
--   enabled                false stops the heartbeat; the row and its
--                          history stay.
--   owner_user_id          the person the agent notifies and acts for: the
--                          person who turned the heartbeat on.
--   mandate_id             the ADR-0039 mandate an act runs under. NULL: the
--                          agent cannot act, and an act degrades to notify.
--   home_conversation_uuid the agent's home thread in this project, created
--                          at the first notify or act. NULL until then.
--   release_id             the release the last wake read.
--   cursor_at              the newest watched message the last triaged wake
--                          read: the next digest holds only newer ones. It is
--                          a column here and not an agent_memory_entries
--                          `cursor` entry, because a memory entry keeps a
--                          version row per change and the cursor changes at
--                          every wake.
--   next_wake_at           when the scan job wakes the agent next.
--   paused_until           a daily limit was reached; no wake before this.
--   last_skip_reason       why the last due wake did not triage (quiet
--                          hours, kill switch, no release, ...). Empty after
--                          a triaged wake.
--
-- project_proactivity_settings
--   One row at most (id = 1). proactive_agents_enabled = false stops every
--   heartbeat of the project. No row: on.
--
-- application_id is NOT a foreign key, as in 0163: an agent deleted later
-- leaves a row that the scan skips because the agent has no release.
-- No to_regclass guard: no table here references another. Unqualified names:
-- the runner pins search_path to the tenant schema. No BEGIN/COMMIT.
CREATE TABLE IF NOT EXISTS agent_heartbeat_settings (
    application_id          bigint      PRIMARY KEY CHECK (application_id > 0),
    enabled                 boolean     NOT NULL DEFAULT true,
    owner_user_id           integer     NOT NULL CHECK (owner_user_id > 0),
    mandate_id              uuid,
    home_conversation_uuid  uuid,
    release_id              bigint      CHECK (release_id IS NULL OR release_id > 0),
    cursor_at               timestamptz,
    next_wake_at            timestamptz NOT NULL DEFAULT clock_timestamp(),
    last_wake_at            timestamptz,
    paused_until            timestamptz,
    last_skip_reason        text        NOT NULL DEFAULT ''
                            CHECK (last_skip_reason ~ '^[a-z0-9_]{0,64}$'),
    updated_by              integer     CHECK (updated_by IS NULL OR updated_by > 0),
    created_at              timestamptz NOT NULL DEFAULT clock_timestamp(),
    updated_at              timestamptz NOT NULL DEFAULT clock_timestamp()
);

-- The scan: the enabled rows that are due, oldest first.
CREATE INDEX IF NOT EXISTS ix_agent_heartbeat_settings_due
    ON agent_heartbeat_settings (next_wake_at)
    WHERE enabled;

CREATE TABLE IF NOT EXISTS project_proactivity_settings (
    id                        smallint    PRIMARY KEY DEFAULT 1 CHECK (id = 1),
    proactive_agents_enabled  boolean     NOT NULL DEFAULT true,
    updated_by                integer     CHECK (updated_by IS NULL OR updated_by > 0),
    updated_at                timestamptz NOT NULL DEFAULT clock_timestamp()
);
