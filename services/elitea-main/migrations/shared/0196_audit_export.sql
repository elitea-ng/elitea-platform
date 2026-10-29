-- 0196_audit_export.sql — state for the SIEM audit export (ADR-0042 decision 3,
-- issue #1494).
--
--   centry.audit_events.export_txid
--                             the top-level transaction id that inserted the
--                             row (pg_current_xact_id(), as bigint). The relay
--                             tails the table in (export_txid, id) order and
--                             reads only rows whose transaction is older than
--                             the oldest transaction still running
--                             (pg_snapshot_xmin of its own snapshot). Every
--                             such transaction has ended, so no row can still
--                             appear behind the cursor: an id cursor alone
--                             skips a lower id that commits after a higher one
--                             (ids are taken at insert, rows become visible at
--                             commit, and several replicas write), and a
--                             timestamp "settle delay" only makes that less
--                             likely. Rows that existed before this migration
--                             read 0 (a constant default is a catalogue-only
--                             change, no table rewrite); the default for new
--                             rows is then switched to the inserting
--                             transaction. The index is partial, so building it
--                             reads the table once but writes almost nothing.
--   audit_export_cursors      one row per (stream, sink): the (export_txid, id)
--                             position of the last centry.audit_events row
--                             that sink accepted (or dead-lettered). Each sink
--                             has its own position, so a healthy sink never
--                             receives a row twice while another is down, and
--                             a failing sink resumes from where it stopped.
--                             The relay advances a position with a compare-
--                             and-set after the send, outside any transaction.
--                             Rows of a sink that is no longer configured are
--                             removed by the relay at its next pass. The
--                             scheduler's audit retention sweep never deletes
--                             a row beyond the slowest position. The security
--                             audit stream needs no cursor: its outbox rows
--                             carry exported_at (0183).
--   audit_export_dead_letters single events given up on after the receiver
--                             refused that one event, alone, as malformed
--                             (HTTP 400, 413 or 422) on five passes, and
--                             security outbox records that cannot be decoded.
--                             A batch the receiver refuses is split in halves
--                             until the refused event is isolated; its
--                             neighbours are delivered. An outage is never
--                             dead-lettered. The event also stays in its
--                             source table, so this is a work list for an
--                             operator, not the only copy.
--
-- No data. Idempotent throughout. No BEGIN/COMMIT: the ledgered runner wraps
-- the file. Never released before this form, so it was rewritten in place.

CREATE SCHEMA IF NOT EXISTS elitea_runtime;

DO $$
BEGIN
IF to_regclass('centry.audit_events') IS NULL THEN
    RAISE NOTICE '0196: centry.audit_events absent, no export order column';
    RETURN;
END IF;

ALTER TABLE centry.audit_events
    ADD COLUMN IF NOT EXISTS export_txid bigint NOT NULL DEFAULT 0;
ALTER TABLE centry.audit_events
    ALTER COLUMN export_txid SET DEFAULT (pg_current_xact_id()::text::bigint);

-- Plain, not CONCURRENTLY: the runner applies the file in a transaction.
CREATE INDEX IF NOT EXISTS audit_events_export_order
    ON centry.audit_events (export_txid, id) WHERE export_txid <> 0;
END
$$;

CREATE TABLE IF NOT EXISTS elitea_runtime.audit_export_cursors (
    stream     text NOT NULL CHECK (stream ~ '^[a-z_]{1,64}$'),
    sink       text NOT NULL CHECK (sink ~ '^[a-z0-9_]{1,64}$'),
    last_txid  bigint NOT NULL DEFAULT 0 CHECK (last_txid >= 0),
    last_id    bigint NOT NULL DEFAULT 0 CHECK (last_id >= 0),
    updated_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    PRIMARY KEY (stream, sink)
);

CREATE TABLE IF NOT EXISTS elitea_runtime.audit_export_dead_letters (
    id               bigserial PRIMARY KEY,
    -- "<source>:<row id>", for example security_audit:42 or admin_audit:1001.
    source_ref       text NOT NULL,
    event_id         text NOT NULL,
    sink             text NOT NULL,
    reason           text NOT NULL,
    attempts         integer NOT NULL CHECK (attempts >= 1),
    -- The elitea.audit.v1 envelope that was refused (for an undecodable
    -- security record: the identifying fields and the raw stored event).
    event            jsonb NOT NULL,
    dead_lettered_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    UNIQUE (source_ref, sink)
);

CREATE INDEX IF NOT EXISTS audit_export_dead_letters_time
    ON elitea_runtime.audit_export_dead_letters (dead_lettered_at);
