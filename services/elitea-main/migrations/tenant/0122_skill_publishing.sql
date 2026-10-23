-- Skill-level publishing (#249).
--
-- The Go bootstrap schema modelled skills as project-local content only: a
-- skill_versions row had no publish state, and a skills row had no link back to
-- the project a public copy was published from. Both columns exist in the
-- legacy runtime schema this platform is drop-in compatible with
-- (legacy/plugins/elitea_core/models/skill.py), so this migration brings a
-- migrated database up to the same shape rather than inventing one.
--
-- `status` mirrors application_versions.status and carries the same
-- PublishStatus vocabulary; only 'draft' and 'published' are written by the Go
-- publish surface today.
ALTER TABLE skill_versions
    ADD COLUMN IF NOT EXISTS status VARCHAR NOT NULL DEFAULT 'draft';

CREATE INDEX IF NOT EXISTS ix_skill_versions_status
    ON skill_versions (status);

-- (shared_owner_id, shared_id) points a public-catalog skill at the source
-- project + skill it was published from. It is the lookup key for "does this
-- skill already have a public twin", so the partial unique index is load-bearing
-- and not just hygiene: without it two concurrent first publishes of the same
-- source skill would each create their own catalog entry.
ALTER TABLE skills
    ADD COLUMN IF NOT EXISTS shared_owner_id INTEGER;
ALTER TABLE skills
    ADD COLUMN IF NOT EXISTS shared_id INTEGER;

CREATE UNIQUE INDEX IF NOT EXISTS uq_skills_shared_owner
    ON skills (shared_owner_id, shared_id)
    WHERE shared_owner_id IS NOT NULL;
