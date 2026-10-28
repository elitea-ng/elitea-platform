-- 0193_credential_reference_lifecycle.sql — who stages and who activates a
-- credential reference, and where a migrated one came from (ADR-0039,
-- issue #1124).
--
--   staged_by           the operator who staged the reference
--                       (`elitea-main credential-governance migrate-references
--                       --apply --operator`), an auth_core user id. The
--                       actors are bigint, so the two-person CHECK compares
--                       ids, never spellings ("007" is not a second person).
--   activated_by        the administrator who activated it through
--                       POST /api/v2/admin/credential_references/{id}:activate.
--                       Two-person rule: never the stager, in code and in the
--                       CHECK below.
--   last_transition_by  the actor of the latest state change.
--   migrated_from       the idempotency key of migrate-references
--                       ("configuration:<uuid>:<backend>" or "mcp:<toolkit id>"),
--                       unique per tenant project among the references that
--                       are not retired, so a second apply finds the
--                       reference the first one staged instead of adding one,
--                       and an apply after a retire stages the replacement.
--
-- 0184 shipped the state column with 'staged' as its default; nothing wrote
-- or activated a reference before this release, so every existing row was
-- written by hand and keeps NULL actors.
--
-- Additive only: nullable columns, a CHECK that NULLs pass, a partial unique
-- index. Idempotent. No BEGIN/COMMIT: the ledgered runner wraps the file.

ALTER TABLE elitea_identity.credential_references
    ADD COLUMN IF NOT EXISTS staged_by bigint
        CHECK (staged_by IS NULL OR staged_by > 0),
    ADD COLUMN IF NOT EXISTS activated_by bigint
        CHECK (activated_by IS NULL OR activated_by > 0),
    ADD COLUMN IF NOT EXISTS last_transition_by bigint
        CHECK (last_transition_by IS NULL OR last_transition_by > 0),
    ADD COLUMN IF NOT EXISTS migrated_from text
        CHECK (migrated_from IS NULL OR length(migrated_from) BETWEEN 1 AND 128);

DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint
        WHERE conname = 'credential_references_activator_not_stager'
          AND conrelid = 'elitea_identity.credential_references'::regclass
    ) THEN
        ALTER TABLE elitea_identity.credential_references
            ADD CONSTRAINT credential_references_activator_not_stager
            CHECK (activated_by IS NULL OR staged_by IS NULL OR activated_by <> staged_by);
    END IF;
END
$$;

CREATE UNIQUE INDEX IF NOT EXISTS credential_references_one_live_per_migration_source
    ON elitea_identity.credential_references (tenant_project_id, migrated_from)
    WHERE migrated_from IS NOT NULL AND state <> 'retired';

CREATE INDEX IF NOT EXISTS credential_references_tenant_created
    ON elitea_identity.credential_references (tenant_project_id, created_at DESC);
