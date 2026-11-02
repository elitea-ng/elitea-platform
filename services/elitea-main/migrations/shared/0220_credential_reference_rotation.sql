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
-- The four columns are all NULL or all set (CHECK below). Additive only:
-- nullable columns and a CHECK that NULLs pass. An older Main never reads or
-- writes them. Idempotent. No BEGIN/COMMIT: the ledgered runner wraps the
-- file. No data, no permission.

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
