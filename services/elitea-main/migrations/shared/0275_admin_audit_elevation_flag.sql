-- 0275_admin_audit_elevation_flag.sql — the review mark of an admin audit
-- row written under an elevation (ADR-0059 decision 5, "What an elevation
-- cannot do"; story R2.8a, issue 2522).
--
-- The action table gives every administration action an elevation value.
-- `flag` (and `sealed`, under the sealed super_admin elevation) lets the
-- action go on under an elevation and marks its row for the elevation
-- review. The role-use columns of 0268 have no field for the mark, and the
-- row has no payload column to carry it, so it is one column:
--
--   elevation_flagged  boolean  the action was done under an elevation, by a
--                               permission only the elevation gave, and the
--                               action table flags it. NULL on every row
--                               written before this file; false when not
--                               flagged.
--
-- OUTSIDE THE CHAIN HASH, like the columns of 0268: centry.
-- audit_event_canonical (shared 0206) names its columns one by one and is not
-- changed here, so every link written before this file still verifies. It is
-- not tamper-evident until R1.4b (issue 2576) moves the role-use columns into
-- a versioned canonical form.
--
-- ADD COLUMN with no default is a catalogue-only change, no table rewrite.
-- Additive only. Idempotent. Tolerates a database without
-- centry.audit_events. No BEGIN/COMMIT: the ledgered runner wraps the file.

DO $$
BEGIN
IF to_regclass('centry.audit_events') IS NULL THEN
    RAISE NOTICE '0275: centry.audit_events absent, no elevation flag column';
    RETURN;
END IF;

ALTER TABLE centry.audit_events
    ADD COLUMN IF NOT EXISTS elevation_flagged boolean;

COMMENT ON COLUMN centry.audit_events.elevation_flagged IS
    'ADR-0059 R2.8a: an action the action table flags, done under an elevation; marked for the review. Outside the chain hash until R1.4b.';
END
$$;
