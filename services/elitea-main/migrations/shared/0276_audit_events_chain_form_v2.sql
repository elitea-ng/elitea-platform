-- 0276_audit_events_chain_form_v2.sql — a versioned canonical form for the
-- centry.audit_events chain, and form v2 that hashes the role-use columns
-- (ADR-0059 decision 8, "Recording role use"; story R1.4b, issue 2576).
--
-- The role-use columns of shared 0268 and the elevation mark of shared 0275
-- were written outside the row hash, so a writer with UPDATE rights could
-- change them without breaking the chain. This file makes them
-- tamper-evident for every row chained from now on, and leaves every link
-- already written verifying exactly as it did:
--
--   elitea_runtime.audit_events_chain_forms
--                     one row per (partition, form version >= 2): the first
--                     chain position hashed in that form. A position below
--                     every recorded from_seq is form v1. Append-only:
--                     UPDATE and DELETE raise. This file records form 2 of
--                     partition '0' at the head's last_seq + 1, with the head
--                     row locked, so the cut-over is exactly one position: no
--                     row is hashed in v1 after it and none in v2 before it.
--   centry.audit_event_canonical_v1(row, partition_key, partition_seq)
--                     the form of shared 0206, byte for byte (tag
--                     elitea.audit_events.v1). It is a copy, not a change:
--                     0206 is applied and immutable.
--   centry.audit_event_canonical_v2(row, partition_key, partition_seq)
--                     v1's columns in v1's order, the tag
--                     elitea.audit_events.v2, then appended: acting_roles,
--                     elevation_id, credential_kind, central_override,
--                     reason_code, reason_missing, elevation_flagged. The
--                     reason TEXT is not hashed: it stays erasable, and no
--                     export or chained event ever carries it.
--   centry.audit_event_canonical(row, partition_key, partition_seq)
--                     same signature as 0206, now a dispatcher: it picks the
--                     form by the chain POSITION (assigned under the head
--                     lock, monotonic), never by row id. The chain trigger
--                     and the verifier (internal/audit/chain.go) both call
--                     it, so the bytes hashed and the bytes checked come from
--                     one place. A later form (E11.1b, issue 2709) adds a
--                     forms row and one branch here.
--   centry.audit_events_append_only()
--                     0206 compared canonical(NEW, '0', 0) with
--                     canonical(OLD, '0', 0): position 0 is always v1, so a
--                     v2 row's role-use columns would stay writable. It now
--                     looks the row's position up through
--                     audit_events_chain.audit_id and compares the form that
--                     row was hashed in. A v1 row's role-use columns stay
--                     writable (its hash never covered them).
--
-- Rolling upgrade: an older binary still writes rows during the deploy; the
-- trigger hashes them in v2 with whatever role-use values they carry (NULL),
-- which verifies. No table rewrite: centry.audit_events is not altered.
-- Idempotent (a second run keeps the recorded cut-over). Tolerates a
-- database without centry.audit_events. No BEGIN/COMMIT: the ledgered runner
-- wraps the file.

CREATE TABLE IF NOT EXISTS elitea_runtime.audit_events_chain_forms (
    partition_key text NOT NULL CHECK (partition_key ~ '^(0|[1-9][0-9]{0,18})$'),
    form_version  integer NOT NULL CHECK (form_version >= 2),
    from_seq      bigint NOT NULL CHECK (from_seq >= 1),
    created_at    timestamptz NOT NULL DEFAULT clock_timestamp(),
    PRIMARY KEY (partition_key, form_version),
    UNIQUE (partition_key, from_seq)
);

COMMENT ON TABLE elitea_runtime.audit_events_chain_forms IS
    'ADR-0059 R1.4b: the first chain position hashed in each canonical form version >= 2; a position below every from_seq is form v1. Append-only.';

CREATE OR REPLACE FUNCTION elitea_runtime.audit_events_chain_forms_append_only()
RETURNS trigger
LANGUAGE plpgsql
AS $fn$
BEGIN
    RAISE EXCEPTION 'an audit chain form cut-over is append-only'
        USING ERRCODE = 'integrity_constraint_violation';
END
$fn$;

DROP TRIGGER IF EXISTS audit_events_chain_forms_append_only ON elitea_runtime.audit_events_chain_forms;
CREATE TRIGGER audit_events_chain_forms_append_only
    BEFORE UPDATE OR DELETE ON elitea_runtime.audit_events_chain_forms
    FOR EACH ROW EXECUTE FUNCTION elitea_runtime.audit_events_chain_forms_append_only();

DO $$
BEGIN
IF to_regclass('centry.audit_events') IS NULL THEN
    RAISE NOTICE '0276: centry.audit_events absent, no chain form v2';
    RETURN;
END IF;

-- The cut-over, under the head lock every chained insert takes: an insert
-- that commits before this file has its position below from_seq, one that
-- waits on the lock is hashed after the new functions are visible.
INSERT INTO elitea_runtime.audit_events_chain_heads (partition_key)
VALUES ('0')
ON CONFLICT (partition_key) DO NOTHING;
PERFORM 1 FROM elitea_runtime.audit_events_chain_heads WHERE partition_key = '0' FOR UPDATE;
INSERT INTO elitea_runtime.audit_events_chain_forms (partition_key, form_version, from_seq)
SELECT '0', 2, last_seq + 1 FROM elitea_runtime.audit_events_chain_heads WHERE partition_key = '0'
ON CONFLICT (partition_key, form_version) DO NOTHING;

CREATE OR REPLACE FUNCTION centry.audit_event_canonical_v1(e centry.audit_events, partition_key text, partition_seq bigint)
RETURNS text
LANGUAGE sql
STABLE
SET extra_float_digits = 1
AS $fn$
    SELECT json_build_array(
        'elitea.audit_events.v1',
        partition_key,
        partition_seq,
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

CREATE OR REPLACE FUNCTION centry.audit_event_canonical_v2(e centry.audit_events, partition_key text, partition_seq bigint)
RETURNS text
LANGUAGE sql
STABLE
SET extra_float_digits = 1
AS $fn$
    SELECT json_build_array(
        'elitea.audit_events.v2',
        partition_key,
        partition_seq,
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
        e.parent_span_id,
        e.acting_roles,
        e.elevation_id,
        e.credential_kind,
        e.central_override,
        e.reason_code,
        e.reason_missing,
        e.elevation_flagged
    )::text
$fn$;

CREATE OR REPLACE FUNCTION centry.audit_event_canonical(e centry.audit_events, partition_key text, partition_seq bigint)
RETURNS text
LANGUAGE sql
STABLE
SET extra_float_digits = 1
AS $fn$
    SELECT CASE
        WHEN EXISTS (
            SELECT 1 FROM elitea_runtime.audit_events_chain_forms f
             WHERE f.partition_key = $2 AND f.form_version = 2 AND f.from_seq <= $3)
        THEN centry.audit_event_canonical_v2(e, $2, $3)
        ELSE centry.audit_event_canonical_v1(e, $2, $3)
    END
$fn$;

CREATE OR REPLACE FUNCTION centry.audit_events_append_only()
RETURNS trigger
LANGUAGE plpgsql
AS $fn$
DECLARE
    link_key text;
    link_seq bigint;
BEGIN
    SELECT l.partition_key, l.partition_seq INTO link_key, link_seq
      FROM elitea_runtime.audit_events_chain l
     WHERE l.audit_id = OLD.id;
    IF FOUND AND centry.audit_event_canonical(NEW, link_key, link_seq)
                 IS DISTINCT FROM centry.audit_event_canonical(OLD, link_key, link_seq) THEN
        RAISE EXCEPTION 'a chained audit event is append-only'
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    RETURN NEW;
END
$fn$;
END
$$;
