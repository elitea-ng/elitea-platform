-- 0268_admin_audit_role_use.sql — the capacity an administration-mode audit
-- row was written in (ADR-0059 decision 8, "Recording role use", "Admin
-- audit"; story R1.4a, issue 2508).
--
-- An admin audit row said who did what, and not in which capacity. These
-- columns say it, as plain nullable columns:
--
--   acting_roles      text[]   the caller's standing administration roles at
--                              request time, by name ({} for a credential
--                              that acts with none: a `none` token, a run
--                              token, a system token)
--   elevation_id      uuid     the elevation the request was made under
--                              (elitea_auth.admin_elevations, R2.1); NULL
--                              until R2.1 produces one
--   credential_kind   text     session, form, sealed, token, native, run,
--                              system or scim (the credentials table of
--                              decision 7); NULL when the kind is unknown,
--                              never a guess
--   central_override  boolean  the write went through the central override
--                              (R0.6)
--   reason_code       text     the reason code the caller gave (R3.8)
--   reason            text     the reason text (R3.8). Stored here only; a
--                              chained security event carries the code, never
--                              the text
--   reason_missing    boolean  a reason was required and not given (R3.8)
--
-- OUTSIDE THE CHAIN HASH. centry.audit_event_canonical (shared 0206) names its
-- columns one by one and is not changed here, so a row's hash does not cover
-- these columns and every link written before this file still verifies. The
-- append-only trigger of 0206 guards only the hashed columns, so these stay
-- writable: they are NOT tamper-evident until R1.4b (issue 2576) moves them
-- into a versioned canonical form. docs/ARCHITECTURE.md says so.
--
-- centry.audit_events is large: ADD COLUMN with no default is a catalogue-only
-- change, no table rewrite, and every existing row reads NULL. Additive only.
-- Idempotent. Tolerates a database without centry.audit_events. No
-- BEGIN/COMMIT: the ledgered runner wraps the file.

DO $$
BEGIN
IF to_regclass('centry.audit_events') IS NULL THEN
    RAISE NOTICE '0268: centry.audit_events absent, no role-use columns';
    RETURN;
END IF;

ALTER TABLE centry.audit_events
    ADD COLUMN IF NOT EXISTS acting_roles text[],
    ADD COLUMN IF NOT EXISTS elevation_id uuid,
    ADD COLUMN IF NOT EXISTS credential_kind text,
    ADD COLUMN IF NOT EXISTS central_override boolean,
    ADD COLUMN IF NOT EXISTS reason_code text,
    ADD COLUMN IF NOT EXISTS reason text,
    ADD COLUMN IF NOT EXISTS reason_missing boolean;

COMMENT ON COLUMN centry.audit_events.acting_roles IS
    'ADR-0059 R1.4a: the caller''s standing administration roles at request time. Outside the chain hash until R1.4b.';
COMMENT ON COLUMN centry.audit_events.elevation_id IS
    'ADR-0059 R1.4a: the elevation the request was made under (R2.1). Outside the chain hash until R1.4b.';
COMMENT ON COLUMN centry.audit_events.credential_kind IS
    'ADR-0059 R1.4a: session, form, sealed, token, native, run, system or scim; NULL when unknown. Outside the chain hash until R1.4b.';
COMMENT ON COLUMN centry.audit_events.central_override IS
    'ADR-0059 R1.4a: the write went through the central override (R0.6). Outside the chain hash until R1.4b.';
COMMENT ON COLUMN centry.audit_events.reason_code IS
    'ADR-0059 R1.4a: the reason code (R3.8). Outside the chain hash until R1.4b.';
COMMENT ON COLUMN centry.audit_events.reason IS
    'ADR-0059 R1.4a: the reason text (R3.8), stored in this row only. Outside the chain hash until R1.4b.';
COMMENT ON COLUMN centry.audit_events.reason_missing IS
    'ADR-0059 R1.4a: a required reason was not given (R3.8). Outside the chain hash until R1.4b.';
END
$$;
