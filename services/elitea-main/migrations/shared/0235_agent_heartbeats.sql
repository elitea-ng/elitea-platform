-- 0235_agent_heartbeats.sql — one row per heartbeat wake of a proactive agent
-- (ADR-0050 decision 2, issue 1618).
--
--   elitea_runtime.agent_heartbeats
--       One row per wake that Main's agent.heartbeat.scan.v1 job triaged,
--       and one row each time a daily limit paused the heartbeat. A wake
--       that quiet hours, the kill switch, maintenance or an overlap
--       skipped writes no row: it only moves the next wake.
--
--       outcome   no_op        the triage found nothing to do. This row is
--                              the only thing the wake wrote.
--                 notify       one agent message in the home thread and one
--                              notification for the owner.
--                 act          a run started (execution_id) under the owner's
--                              mandate, with notify_and_wait.
--                 act_degraded the triage asked to act, but the agent could
--                              not (no mandate, the act limit, a refused
--                              start). It notified the owner instead and
--                              reason_code tells why.
--                 failed       the triage or the post failed. Nothing was
--                              written for the owner.
--                 paused       a daily limit was reached. The heartbeat
--                              sleeps until the next day (paused_until).
--       digest_sha256  the SHA-256 of the wake digest the triage read. The
--                      digest itself is never stored: it holds message text.
--       triage_spend_usd  the gateway cost of the triage call
--                      (execution_id 'heartbeat:<id>' in the request log),
--                      as read when the row was written.
--
-- Shared, not tenant: the run ledger and the gateway request log that the
-- daily envelope reads are shared. No permission: only Main reads it. No
-- data. Idempotent. No BEGIN/COMMIT: the ledgered runner wraps the file.

CREATE SCHEMA IF NOT EXISTS elitea_runtime;

CREATE TABLE IF NOT EXISTS elitea_runtime.agent_heartbeats (
    id                 bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    -- The wake id the triage call is attributed to (heartbeat:<wake_id>).
    wake_id            uuid NOT NULL UNIQUE,
    project_id         bigint NOT NULL CHECK (project_id > 0),
    application_id     bigint NOT NULL CHECK (application_id > 0),
    -- The release the wake read; NULL for a pause.
    release_id         bigint CHECK (release_id IS NULL OR release_id > 0),
    version_id         bigint CHECK (version_id IS NULL OR version_id > 0),
    owner_user_id      bigint NOT NULL CHECK (owner_user_id > 0),
    outcome            text NOT NULL CHECK (outcome IN (
                           'no_op', 'notify', 'act', 'act_degraded', 'failed', 'paused')),
    reason_code        text NOT NULL DEFAULT '' CHECK (reason_code ~ '^[a-z0-9_]{0,64}$'),
    reason             text NOT NULL DEFAULT '' CHECK (octet_length(reason) <= 500),
    digest_sha256      text CHECK (digest_sha256 IS NULL OR digest_sha256 ~ '^[0-9a-f]{64}$'),
    triage_spend_usd   numeric(18, 6) NOT NULL DEFAULT 0 CHECK (triage_spend_usd >= 0),
    execution_id       text CHECK (execution_id IS NULL OR execution_id ~ '^[0-9a-f]{32}$'),
    conversation_uuid  uuid,
    paused_until       timestamptz,
    woke_at            timestamptz NOT NULL DEFAULT clock_timestamp(),
    CONSTRAINT agent_heartbeats_act_has_execution CHECK ((outcome = 'act') = (execution_id IS NOT NULL)),
    CONSTRAINT agent_heartbeats_pause_until CHECK ((outcome = 'paused') = (paused_until IS NOT NULL))
);

-- The daily envelope and the history view: one agent's wakes, newest first.
CREATE INDEX IF NOT EXISTS agent_heartbeats_agent_time
    ON elitea_runtime.agent_heartbeats (project_id, application_id, woke_at DESC);
