-- 0183_security_audit_outbox.sql — the durable security audit outbox
-- (ADR-0039, issue #1114).
--
--   security_audit_heads   one row per chain partition (the tenant project
--                          id; '0' is the platform partition). The appender
--                          locks it FOR UPDATE, so the writers of one
--                          partition are serialised and the chain never forks.
--   security_audit_outbox  the events, written in the SAME transaction as the
--                          governed change they record. A rolled-back change
--                          leaves no event; a failed append fails the change.
--   security_audit_events  the durable store. The relay copies outbox rows
--                          here in seq order. Append-only: UPDATE, DELETE and
--                          TRUNCATE raise an error.
--
-- CHAIN. hash = sha256(prev_hash || canonical CloudEvents JSON). The first
-- event of a partition has 32 zero bytes as prev_hash. `event` is jsonb for
-- reading; the verifier re-encodes it canonically before it hashes, so the
-- jsonb reformat does not matter (internal/securityaudit).
--
-- NOTHING IS DROPPED. `stored_at` is set when the row is in the durable
-- store, `exported_at` when the configured exporter accepted it (or with the
-- store when no exporter is configured). The scheduler's retention job
-- deletes only outbox rows that are exported and older than its window, and
-- only when the durable store has the same seq and hash. Its default is no
-- deletion. The durable store itself is never deleted from.
--
-- Events carry references and digests only: no token, secret or assertion.
--
-- With the governance flag off the tables stay empty. Additive only; no data.
-- Idempotent throughout. No BEGIN/COMMIT: the ledgered runner wraps the file.

CREATE SCHEMA IF NOT EXISTS elitea_runtime;
CREATE SCHEMA IF NOT EXISTS elitea_identity;

CREATE TABLE IF NOT EXISTS elitea_runtime.security_audit_heads (
    partition_key text PRIMARY KEY CHECK (partition_key ~ '^(0|[1-9][0-9]{0,18})$'),
    last_seq      bigint NOT NULL DEFAULT 0 CHECK (last_seq >= 0),
    last_hash     bytea NOT NULL DEFAULT decode(repeat('00', 32), 'hex')
                  CHECK (octet_length(last_hash) = 32),
    updated_at    timestamptz NOT NULL DEFAULT clock_timestamp()
);

CREATE TABLE IF NOT EXISTS elitea_runtime.security_audit_outbox (
    seq           bigserial PRIMARY KEY,
    partition_key text NOT NULL CHECK (partition_key ~ '^(0|[1-9][0-9]{0,18})$'),
    -- The position in the partition's chain: 1, 2, 3, ... with no gap.
    partition_seq bigint NOT NULL CHECK (partition_seq >= 1),
    event_id      uuid NOT NULL UNIQUE,
    event_type    text NOT NULL CHECK (event_type ~ '^ai\.elitea\.security\.v1\.[a-z.]{1,64}$'),
    event         jsonb NOT NULL CHECK (event ->> 'specversion' = '1.0'),
    prev_hash     bytea NOT NULL CHECK (octet_length(prev_hash) = 32),
    hash          bytea NOT NULL CHECK (octet_length(hash) = 32),
    created_at    timestamptz NOT NULL DEFAULT clock_timestamp(),
    stored_at     timestamptz NULL,
    exported_at   timestamptz NULL,
    UNIQUE (partition_key, partition_seq),
    CONSTRAINT security_audit_outbox_export_after_store CHECK (
        exported_at IS NULL OR stored_at IS NOT NULL
    )
);

-- The relay's two stages and the backlog count read only the pending rows.
CREATE INDEX IF NOT EXISTS security_audit_outbox_unstored
    ON elitea_runtime.security_audit_outbox (seq)
    WHERE stored_at IS NULL;
CREATE INDEX IF NOT EXISTS security_audit_outbox_unexported
    ON elitea_runtime.security_audit_outbox (seq)
    WHERE exported_at IS NULL;
-- The retention job deletes the oldest exported rows first.
CREATE INDEX IF NOT EXISTS security_audit_outbox_exported
    ON elitea_runtime.security_audit_outbox (exported_at)
    WHERE exported_at IS NOT NULL;

CREATE TABLE IF NOT EXISTS elitea_identity.security_audit_events (
    -- The outbox seq, kept so a row is traceable to its outbox row.
    seq           bigint PRIMARY KEY,
    partition_key text NOT NULL CHECK (partition_key ~ '^(0|[1-9][0-9]{0,18})$'),
    partition_seq bigint NOT NULL CHECK (partition_seq >= 1),
    event_id      uuid NOT NULL UNIQUE,
    event_type    text NOT NULL,
    event         jsonb NOT NULL,
    prev_hash     bytea NOT NULL CHECK (octet_length(prev_hash) = 32),
    hash          bytea NOT NULL CHECK (octet_length(hash) = 32),
    created_at    timestamptz NOT NULL,
    stored_at     timestamptz NOT NULL DEFAULT clock_timestamp(),
    UNIQUE (partition_key, partition_seq)
);

CREATE OR REPLACE FUNCTION elitea_identity.security_audit_events_append_only()
RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'security audit events are append-only'
        USING ERRCODE = 'insufficient_privilege';
END
$$;

DROP TRIGGER IF EXISTS security_audit_events_append_only
    ON elitea_identity.security_audit_events;
CREATE TRIGGER security_audit_events_append_only
    BEFORE UPDATE OR DELETE ON elitea_identity.security_audit_events
    FOR EACH ROW EXECUTE FUNCTION elitea_identity.security_audit_events_append_only();

DROP TRIGGER IF EXISTS security_audit_events_no_truncate
    ON elitea_identity.security_audit_events;
CREATE TRIGGER security_audit_events_no_truncate
    BEFORE TRUNCATE ON elitea_identity.security_audit_events
    FOR EACH STATEMENT EXECUTE FUNCTION elitea_identity.security_audit_events_append_only();
