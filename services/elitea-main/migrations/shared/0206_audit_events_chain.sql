-- 0206_audit_events_chain.sql — centry.audit_events is hash-chained from now
-- on (ADR-0042 decision 3, issue #1499).
--
--   centry.audit_events.partition_key, partition_seq, prev_hash, hash
--                     the row's place in the chain. NULL on every row written
--                     before this file: those rows were never chained and
--                     the verifier skips them. Nullable columns without a
--                     default are a catalogue-only change (no table rewrite).
--   elitea_runtime.audit_events_chain_heads
--                     one row per chain partition: the last link
--                     (last_seq, last_hash), locked FOR UPDATE by every
--                     insert, and the retention prune anchor (pruned_seq,
--                     pruned_hash: the position and hash of the last row the
--                     retention sweep deleted). Verification starts at
--                     pruned_seq + 1 with pruned_hash as the expected
--                     prev_hash, so a pruned chain still verifies instead of
--                     reporting its deleted prefix as missing rows.
--   centry.audit_event_canonical(row)
--                     the canonical text the hash covers: a JSON array of a
--                     version tag, the chain position, the row id and every
--                     content column, numbers and timestamps rendered as
--                     text in a session-independent form (UTC, microseconds,
--                     extra_float_digits pinned). Changing it breaks the
--                     verification of every chained row: a new form needs a
--                     new version tag and a new function.
--   zz_audit_events_chain (BEFORE INSERT, FOR EACH ROW)
--                     chains every inserted row in the inserting transaction:
--                     lock the head, partition_seq = last_seq + 1,
--                     hash = sha256(last_hash || canonical), move the head.
--                     A trigger and not the Go writers, because three writers
--                     exist (internal/audit's batched recorder, the browser
--                     session relay's multi-row INSERT ... SELECT, the cloud
--                     executor attestation) plus the E2E seeder and test
--                     fixtures, and a trigger is the one place a future
--                     writer cannot bypass. sha256(prev || canonical) is
--                     securityaudit.ChainHash; internal/audit verifies with
--                     securityaudit.ChainVerifier. Named zz_ so it fires after
--                     any other BEFORE INSERT trigger that could still change
--                     the row it hashes.
--   audit_events_append_only (BEFORE UPDATE, FOR EACH ROW)
--                     refuses an UPDATE that changes what a chained row's
--                     hash covers (its canonical form or its links).
--                     Nothing in the product updates this table; the
--                     verifier would report the change anyway, this makes
--                     the mistake loud at the time. Columns outside the hash
--                     (export_txid, the export order) stay writable.
--
-- PARTITION. One platform-wide chain, partition '0' (the platform partition,
-- as in the security audit). These rows are admin and security requests:
-- single-digit rows per second on a busy platform, written by an
-- asynchronous recorder in batches of up to 64 rows per transaction, so one
-- head lock is not a contention point. Per-project partitions buy nothing
-- here: most rows (admin routes, sessions, platform tasks) carry no project,
-- the retention sweep is platform-wide, and the audit trail is read
-- platform-wide.
--
-- No data apart from the head row. No permission: the verification route
-- reuses models.admin.audit_trail.view. Idempotent; tolerates a database
-- without centry.audit_events. No BEGIN/COMMIT: the ledgered runner wraps the
-- file.

CREATE SCHEMA IF NOT EXISTS elitea_runtime;

CREATE TABLE IF NOT EXISTS elitea_runtime.audit_events_chain_heads (
    partition_key text PRIMARY KEY CHECK (partition_key ~ '^(0|[1-9][0-9]{0,18})$'),
    last_seq      bigint NOT NULL DEFAULT 0 CHECK (last_seq >= 0),
    last_hash     bytea NOT NULL DEFAULT decode(repeat('00', 32), 'hex')
                  CHECK (octet_length(last_hash) = 32),
    -- The last position retention deleted, and that row's hash: the anchor
    -- the first surviving row's prev_hash must equal. 0 and zero bytes until
    -- the first prune.
    pruned_seq    bigint NOT NULL DEFAULT 0 CHECK (pruned_seq >= 0),
    pruned_hash   bytea NOT NULL DEFAULT decode(repeat('00', 32), 'hex')
                  CHECK (octet_length(pruned_hash) = 32),
    pruned_at     timestamptz,
    updated_at    timestamptz NOT NULL DEFAULT clock_timestamp(),
    CHECK (pruned_seq <= last_seq)
);

INSERT INTO elitea_runtime.audit_events_chain_heads (partition_key)
VALUES ('0')
ON CONFLICT (partition_key) DO NOTHING;

DO $$
BEGIN
IF to_regclass('centry.audit_events') IS NULL THEN
    RAISE NOTICE '0206: centry.audit_events absent, no chain';
    RETURN;
END IF;

ALTER TABLE centry.audit_events
    ADD COLUMN IF NOT EXISTS partition_key text,
    ADD COLUMN IF NOT EXISTS partition_seq bigint,
    ADD COLUMN IF NOT EXISTS prev_hash bytea,
    ADD COLUMN IF NOT EXISTS hash bytea;

-- Plain, not CONCURRENTLY: the runner applies the file in a transaction.
-- Partial, so the rows from before this file cost nothing.
CREATE UNIQUE INDEX IF NOT EXISTS audit_events_chain_position
    ON centry.audit_events (partition_key, partition_seq)
    WHERE partition_seq IS NOT NULL;

CREATE OR REPLACE FUNCTION centry.audit_event_canonical(e centry.audit_events)
RETURNS text
LANGUAGE sql
STABLE
SET extra_float_digits = 1
AS $fn$
    SELECT json_build_array(
        'elitea.audit_events.v1',
        e.partition_key,
        e.partition_seq,
        e.id,
        to_char(e.timestamp AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS.US"Z"'),
        e.user_id,
        e.user_email,
        e.project_id,
        e.event_type,
        e.action,
        e.http_method,
        e.http_route,
        e.status_code,
        e.duration_ms::text,
        e.is_error,
        e.entity_type,
        e.entity_id,
        e.entity_name,
        e.tool_name,
        e.model_name,
        e.input_tokens,
        e.output_tokens,
        e.llm_cost::text,
        e.token_source,
        e.cost_source,
        e.trace_id,
        e.span_id,
        e.parent_span_id
    )::text
$fn$;

CREATE OR REPLACE FUNCTION centry.audit_events_chain()
RETURNS trigger
LANGUAGE plpgsql
AS $fn$
DECLARE
    head_seq  bigint;
    head_hash bytea;
BEGIN
    -- Create the head on first use and lock it. ON CONFLICT DO UPDATE takes
    -- the row lock, so two first writers serialise here instead of both
    -- reading last_seq 0 (the securityaudit appender's lockHeadSQL).
    INSERT INTO elitea_runtime.audit_events_chain_heads AS head (partition_key)
    VALUES ('0')
    ON CONFLICT (partition_key) DO UPDATE SET partition_key = head.partition_key
    RETURNING head.last_seq, head.last_hash INTO head_seq, head_hash;

    NEW.partition_key := '0';
    NEW.partition_seq := head_seq + 1;
    NEW.prev_hash := head_hash;
    NEW.hash := sha256(head_hash || convert_to(centry.audit_event_canonical(NEW), 'UTF8'));

    UPDATE elitea_runtime.audit_events_chain_heads
       SET last_seq = NEW.partition_seq, last_hash = NEW.hash, updated_at = clock_timestamp()
     WHERE partition_key = '0';
    RETURN NEW;
END
$fn$;

DROP TRIGGER IF EXISTS zz_audit_events_chain ON centry.audit_events;
CREATE TRIGGER zz_audit_events_chain
    BEFORE INSERT ON centry.audit_events
    FOR EACH ROW EXECUTE FUNCTION centry.audit_events_chain();

CREATE OR REPLACE FUNCTION centry.audit_events_append_only()
RETURNS trigger
LANGUAGE plpgsql
AS $fn$
BEGIN
    IF OLD.partition_seq IS NOT NULL AND (
           centry.audit_event_canonical(NEW) IS DISTINCT FROM centry.audit_event_canonical(OLD)
           OR NEW.prev_hash IS DISTINCT FROM OLD.prev_hash
           OR NEW.hash IS DISTINCT FROM OLD.hash) THEN
        RAISE EXCEPTION 'a chained audit event is append-only'
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    RETURN NEW;
END
$fn$;

DROP TRIGGER IF EXISTS audit_events_append_only ON centry.audit_events;
CREATE TRIGGER audit_events_append_only
    BEFORE UPDATE ON centry.audit_events
    FOR EACH ROW EXECUTE FUNCTION centry.audit_events_append_only();
END
$$;
