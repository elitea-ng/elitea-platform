-- The soul document of an agent version (ADR-0050 decision 1a, issue 1604).
--
-- `application_versions.soul` holds the version's persona as the JSON form of
-- `AgentSoulV1` (api/openapi/v2.yaml): `schema`, `display_name`, `pronouns`,
-- `role`, `voice`, `values`, `boundaries`, `relationships`,
-- `proactive_goals` and the markdown `body`. Main validates it against that
-- component before it writes (internal/domain/agentsoul), at most 16 KiB.
--
-- NULL is "this version has no soul", which is every existing row: no
-- backfill. The version-details projection adds a `soul` key only for a
-- non-NULL value, so the release digest of every existing version
-- (ADR-0040) is unchanged by this migration.
--
-- Guarded on the table for the reason 0134 is: several integration fixtures
-- apply the tenant chain to a schema with no `application_versions`.
DO $$
BEGIN
    IF to_regclass('application_versions') IS NULL THEN
        RETURN;
    END IF;
    ALTER TABLE application_versions ADD COLUMN IF NOT EXISTS soul jsonb NULL;
END
$$;
