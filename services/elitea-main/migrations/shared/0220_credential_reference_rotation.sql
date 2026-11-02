-- 0220_credential_reference_rotation.sql — a pending secret rotation on a
-- credential reference (ADR-0039, issue #1505).
--
-- WHY. A governed static secret must be rotatable without a retire and a
-- restage. POST /api/v2/admin/credential_references/{id}:rotate writes the
-- new value through the secret write ledger (a new Vault version, or a new
-- pgvault name, because a pgvault secret has one version only) and records
-- it here as PENDING. The reference keeps its current locator and generation,
-- so the broker still releases the old value. A second operator then calls
-- :activate: the reference moves to the pending locator and generation, and
-- the pending columns are cleared, in one transaction with its audit event.
--
--   elitea_identity.credential_references
--     rotation_locator     the locator of the pending value (Vault: the same
--                          path; pgvault: a new name). Never a value.
--     rotation_generation  the version the ledgered write created.
--     rotation_staged_by   the operator who wrote it. The activator must be
--                          another operator (two-person rule, in code).
--     rotation_staged_at   when it was written.
--
-- The four columns are all NULL or all set (CHECK below).
--
-- A second :rotate is refused while a rotation is pending. :cancel_rotation
-- and :retire clear a pending rotation. A secret version that the reference
-- no longer uses (the replaced value after :activate, or a cancelled
-- rotation) is retired through the secret write ledger:
--
--   elitea_identity.secret_write_ledger.state  gets 'retired'. A committed
--                          row moves to 'retired' once its backend version
--                          is retired (Vault: destroyed; pgvault: the name
--                          is deleted). It is the only move out of
--                          'committed'.
--
-- Additive only: nullable columns, a CHECK that NULLs pass, and a wider
-- state CHECK. An older Main never reads or writes the new columns and never
-- writes 'retired'. Idempotent. No BEGIN/COMMIT: the ledgered runner wraps
-- the file. No data, no permission.

ALTER TABLE elitea_identity.credential_references
    ADD COLUMN IF NOT EXISTS rotation_locator text
        CHECK (rotation_locator IS NULL OR length(rotation_locator) BETWEEN 1 AND 512),
    ADD COLUMN IF NOT EXISTS rotation_generation bigint
        CHECK (rotation_generation IS NULL OR rotation_generation > 0),
    ADD COLUMN IF NOT EXISTS rotation_staged_by bigint
        CHECK (rotation_staged_by IS NULL OR rotation_staged_by > 0),
    ADD COLUMN IF NOT EXISTS rotation_staged_at timestamptz;

DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint
        WHERE conname = 'credential_references_rotation_complete'
          AND conrelid = 'elitea_identity.credential_references'::regclass
    ) THEN
        ALTER TABLE elitea_identity.credential_references
            ADD CONSTRAINT credential_references_rotation_complete
            CHECK ((rotation_locator IS NULL) = (rotation_generation IS NULL)
               AND (rotation_locator IS NULL) = (rotation_staged_by IS NULL)
               AND (rotation_locator IS NULL) = (rotation_staged_at IS NULL));
    END IF;
END $$;

-- The ledger state 'retired' (see above). The 0182 CHECK on state has the
-- default name secret_write_ledger_state_check.
ALTER TABLE elitea_identity.secret_write_ledger
    DROP CONSTRAINT IF EXISTS secret_write_ledger_state_check;
ALTER TABLE elitea_identity.secret_write_ledger
    ADD CONSTRAINT secret_write_ledger_state_check
        CHECK (state IN ('staged', 'committed', 'abandoned', 'retired'));

-- The 0182 transition rule, plus the one move out of 'committed':
-- committed to retired. The identity columns stay immutable.
CREATE OR REPLACE FUNCTION elitea_identity.secret_write_ledger_transition()
RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF OLD.state = 'committed' AND NEW.state = 'retired' THEN
        NULL;
    ELSIF OLD.state <> 'staged'
        OR (NEW.state = 'staged'
            AND NOT (OLD.created_version IS NULL AND NEW.created_version IS NOT NULL))
        OR NEW.state = 'retired' THEN
        RAISE EXCEPTION 'secret write ledger row % cannot move from % to %', OLD.id, OLD.state, NEW.state;
    END IF;
    IF OLD.created_version IS NOT NULL
        AND NEW.created_version IS DISTINCT FROM OLD.created_version THEN
        RAISE EXCEPTION 'secret write ledger row % created version is immutable', OLD.id;
    END IF;
    IF NEW.credential_ref_id IS DISTINCT FROM OLD.credential_ref_id
        OR NEW.tenant_project_id IS DISTINCT FROM OLD.tenant_project_id
        OR NEW.workload_scope IS DISTINCT FROM OLD.workload_scope
        OR NEW.policy_revision_id IS DISTINCT FROM OLD.policy_revision_id
        OR NEW.backend IS DISTINCT FROM OLD.backend
        OR NEW.locator_mount IS DISTINCT FROM OLD.locator_mount
        OR NEW.locator_path IS DISTINCT FROM OLD.locator_path
        OR NEW.intended_version IS DISTINCT FROM OLD.intended_version
        OR NEW.created_at IS DISTINCT FROM OLD.created_at THEN
        RAISE EXCEPTION 'secret write ledger row % identity is immutable', OLD.id;
    END IF;
    RETURN NEW;
END
$$;
