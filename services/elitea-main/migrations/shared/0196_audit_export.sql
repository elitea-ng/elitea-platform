-- 0196_audit_export.sql — state for the SIEM audit export (ADR-0042 decision 3,
-- issue #1494).
--
--   audit_export_cursors      the id cursor of a tailed stream. Today one row,
--                             stream 'admin_audit': the last centry.audit_events
--                             id a receiver accepted. The relay advances it in
--                             the same transaction that read the batch, after
--                             the sink accepted it, so a crash or a receiver
--                             outage re-sends and never skips. The security
--                             audit stream needs no cursor: its outbox rows
--                             carry exported_at (0183).
--   audit_export_dead_letters events given up on after the receiver refused
--                             the same batch as malformed (HTTP 400, 413 or
--                             422) five times. Only a permanent rejection is
--                             dead-lettered; an outage never is. The event
--                             also stays in its source table, so this is a
--                             work list for an operator, not the only copy.
--
-- Additive only; no data. Idempotent throughout. No BEGIN/COMMIT: the ledgered
-- runner wraps the file.

CREATE SCHEMA IF NOT EXISTS elitea_runtime;

CREATE TABLE IF NOT EXISTS elitea_runtime.audit_export_cursors (
    stream     text PRIMARY KEY CHECK (stream ~ '^[a-z_]{1,64}$'),
    last_id    bigint NOT NULL DEFAULT 0 CHECK (last_id >= 0),
    updated_at timestamptz NOT NULL DEFAULT clock_timestamp()
);

CREATE TABLE IF NOT EXISTS elitea_runtime.audit_export_dead_letters (
    id               bigserial PRIMARY KEY,
    -- "<source>:<row id>", for example security_audit:42 or admin_audit:1001.
    source_ref       text NOT NULL,
    event_id         text NOT NULL,
    sink             text NOT NULL,
    reason           text NOT NULL,
    attempts         integer NOT NULL CHECK (attempts >= 1),
    -- The elitea.audit.v1 envelope that was refused.
    event            jsonb NOT NULL,
    dead_lettered_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    UNIQUE (source_ref, sink)
);

CREATE INDEX IF NOT EXISTS audit_export_dead_letters_time
    ON elitea_runtime.audit_export_dead_letters (dead_lettered_at);
