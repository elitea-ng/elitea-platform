-- Give skill_versions a lineage pointer: which version a named snapshot was
-- created FROM ("Save As Version") or restored FROM (rollback). #874.
--
-- WHY THIS COLUMN. Before this change a skill carried exactly one implicit
-- version, name='base' (internal/api/v2/skills/handler.go's SkillVersion doc
-- comment, since deleted). #874 lets a skill carry several named
-- skill_versions rows the way application_versions already does for
-- agents/pipelines — CreateVersion clones an existing version's content into
-- a new named row, and RestoreVersion copies a named version's content back
-- onto `base`. Neither operation has anywhere to record "copied from" without
-- this column: application_versions has no equivalent either (confirmed by
-- grep — agents don't expose provenance), but skills' own restore/rollback
-- action is new surface #874 adds that agents don't have, and a restore a
-- user cannot trace back to its source is a worse experience than the one
-- being fixed.
--
-- Nullable and ON DELETE SET NULL: the source version can be deleted later
-- (DeleteVersion refuses deleting `base` and the current default, but not an
-- arbitrary ancestor once nothing points at it as default), and a clone or a
-- restored `base` must not become undeletable, or be deleted itself, purely
-- because its ancestor's row was removed. The lineage is provenance, not a
-- constraint on either row's lifecycle.
--
-- Guarded the same way 0134 and 0125 are: several integration fixtures build
-- a tenant schema of their own with no `skills`/`skill_versions` table at
-- all, and ALTER TABLE on a missing relation raises 42P01 and fails the
-- whole tenant chain. Where the table is absent this migration records
-- itself and moves on; where 001_initial's create_tenant_schema has already
-- declared the column (a future fresh install), ADD COLUMN IF NOT EXISTS is
-- a no-op, so both converge on the same shape.
DO $$
BEGIN
    IF to_regclass('skill_versions') IS NULL THEN
        RETURN;
    END IF;

    ALTER TABLE skill_versions ADD COLUMN IF NOT EXISTS parent_version_id INTEGER
        REFERENCES skill_versions(id) ON DELETE SET NULL;
END
$$;
