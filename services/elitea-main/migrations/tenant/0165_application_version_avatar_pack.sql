-- The avatar pack of an agent version (ADR-0050 decision 6, issue 1619).
--
-- `application_versions.avatar_pack` holds the version's look as the JSON
-- form of `AgentAvatarPackV1` (api/openapi/v2.yaml): `schema` and `slots`,
-- one slot per state (still, idle, thinking, speaking, working, waiting,
-- success, error, waking), each an uploaded `agent-media` object URL. Main
-- checks it before it writes (internal/domain/agentmedia), and it checks
-- every media file when it is uploaded.
--
-- NULL is "this version has no pack", which is every existing row: no
-- backfill. A client then reads `meta.icon_meta` as before. A version write
-- that sets a pack also writes its still into `meta.icon_meta`, so a client
-- that reads only the old field keeps showing the right image.
--
-- Guarded on the table for the reason 0134 and 0160 are: several
-- integration fixtures apply the tenant chain to a schema with no
-- `application_versions`.
DO $$
BEGIN
    IF to_regclass('application_versions') IS NULL THEN
        RETURN;
    END IF;
    ALTER TABLE application_versions ADD COLUMN IF NOT EXISTS avatar_pack jsonb NULL;
END
$$;
