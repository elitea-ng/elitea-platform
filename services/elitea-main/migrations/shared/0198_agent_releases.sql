-- 0198_agent_releases.sql — immutable, digest-addressed agent releases
-- and draft revisions (ADR-0040 decision 2, issue #1492).
--
--   elitea_lifecycle.application_releases
--       One row per publish. A release names the published version row
--       (application_version_id, the publish clone), the draft it was made
--       from, its name and who published it. `definition` is the version's
--       canonical definition (the shared application_version_details_json
--       projection) with every toolkit's `settings` removed: toolkit settings
--       may hold a credential value, and a release never stores one.
--       `toolkit_pins` pins each attached toolkit instead: its id, the
--       SHA-256 of its stored settings and of the settings the agent saw, the
--       agent's tool selection, the configuration each non-private
--       configuration reference resolved to (uuid, project and the SHA-256 of
--       its data) and the credential reference ids. No pin holds a value.
--       `definition_digest` is the canonical definition digest of the
--       version (#1462, agentidentity.VersionDefinitionDigest over the
--       projection bytes), the one an agent identity binding pins.
--       The definition, the digest, the pins and the identity columns never
--       change: a trigger refuses an UPDATE of any of them. Only `state` and
--       the withdrawal stamp move, when the version is unpublished.
--
--   elitea_lifecycle.application_release_heads
--       The published release of an application: the release a run of a
--       published version executes. Publish points it at the new release;
--       unpublish moves it to the newest release still published or removes
--       it; rollback re-points it at an earlier release.
--
--   elitea_lifecycle.version_revisions
--       One row per draft save: the version's saved fields after the save,
--       its author, its time, and a JSON diff against the previous revision.
--       Revision 1 of a version that existed before 0198 is the state the
--       first save found (kind 'baseline').
--
--   The release a run executed is 0199 (agent_execution_jobs.release_id):
--   this file depends on no other schema, so the applications repository
--   tests, which run the baseline tenant schema only, can apply it alone.
--
-- Application and version ids are per tenant schema, so every key carries
-- tenant_project_id. The rows stay when an application is deleted: they
-- are the evidence of what ran. No new permission: the routes reuse the
-- version read and publish permissions. Additive only. Idempotent. No
-- BEGIN/COMMIT: the ledgered runner wraps the file.

CREATE SCHEMA IF NOT EXISTS elitea_lifecycle;

CREATE TABLE IF NOT EXISTS elitea_lifecycle.application_releases (
    id                      bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    tenant_project_id       bigint NOT NULL CHECK (tenant_project_id > 0),
    application_id          bigint NOT NULL CHECK (application_id > 0),
    application_version_id  bigint NOT NULL CHECK (application_version_id > 0),
    source_version_id       bigint CHECK (source_version_id IS NULL OR source_version_id > 0),
    name                    text NOT NULL CHECK (length(name) BETWEEN 1 AND 256),
    definition_digest       bytea NOT NULL CHECK (octet_length(definition_digest) = 32),
    definition              jsonb NOT NULL CHECK (jsonb_typeof(definition) = 'object'),
    toolkit_pins            jsonb NOT NULL DEFAULT '[]'::jsonb
                            CHECK (jsonb_typeof(toolkit_pins) = 'array'),
    state                   text NOT NULL DEFAULT 'published'
                            CHECK (state IN ('published', 'withdrawn')),
    published_by            bigint CHECK (published_by IS NULL OR published_by > 0),
    created_at              timestamptz NOT NULL DEFAULT clock_timestamp(),
    withdrawn_at            timestamptz,
    withdrawn_by            bigint CHECK (withdrawn_by IS NULL OR withdrawn_by > 0),
    CONSTRAINT application_releases_digest_key
        UNIQUE (tenant_project_id, definition_digest),
    CONSTRAINT application_releases_application_key
        UNIQUE (id, tenant_project_id, application_id),
    CONSTRAINT application_releases_withdrawal_complete CHECK (
        (state = 'published' AND withdrawn_at IS NULL AND withdrawn_by IS NULL)
        OR (state = 'withdrawn' AND withdrawn_at IS NOT NULL)
    )
);

CREATE INDEX IF NOT EXISTS application_releases_application
    ON elitea_lifecycle.application_releases (tenant_project_id, application_id, id DESC);

CREATE INDEX IF NOT EXISTS application_releases_version
    ON elitea_lifecycle.application_releases (tenant_project_id, application_version_id);

-- A release is immutable: only its state and withdrawal stamp change.
CREATE OR REPLACE FUNCTION elitea_lifecycle.application_releases_immutable()
RETURNS trigger
LANGUAGE plpgsql
AS $$
BEGIN
    IF NEW.id IS DISTINCT FROM OLD.id
       OR NEW.tenant_project_id IS DISTINCT FROM OLD.tenant_project_id
       OR NEW.application_id IS DISTINCT FROM OLD.application_id
       OR NEW.application_version_id IS DISTINCT FROM OLD.application_version_id
       OR NEW.source_version_id IS DISTINCT FROM OLD.source_version_id
       OR NEW.name IS DISTINCT FROM OLD.name
       OR NEW.definition_digest IS DISTINCT FROM OLD.definition_digest
       OR NEW.definition IS DISTINCT FROM OLD.definition
       OR NEW.toolkit_pins IS DISTINCT FROM OLD.toolkit_pins
       OR NEW.published_by IS DISTINCT FROM OLD.published_by
       OR NEW.created_at IS DISTINCT FROM OLD.created_at THEN
        RAISE EXCEPTION 'an application release is immutable'
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    RETURN NEW;
END
$$;

DROP TRIGGER IF EXISTS application_releases_immutable
    ON elitea_lifecycle.application_releases;
CREATE TRIGGER application_releases_immutable
    BEFORE UPDATE ON elitea_lifecycle.application_releases
    FOR EACH ROW EXECUTE FUNCTION elitea_lifecycle.application_releases_immutable();

CREATE TABLE IF NOT EXISTS elitea_lifecycle.application_release_heads (
    tenant_project_id  bigint NOT NULL CHECK (tenant_project_id > 0),
    application_id     bigint NOT NULL CHECK (application_id > 0),
    release_id         bigint NOT NULL,
    pointed_by         bigint CHECK (pointed_by IS NULL OR pointed_by > 0),
    pointed_at         timestamptz NOT NULL DEFAULT clock_timestamp(),
    -- publish | unpublish | rollback: what moved the pointer last.
    cause              text NOT NULL CHECK (cause IN ('publish', 'unpublish', 'rollback')),
    reason             text NOT NULL DEFAULT '' CHECK (length(reason) <= 200),
    PRIMARY KEY (tenant_project_id, application_id),
    CONSTRAINT application_release_heads_release_same_application
        FOREIGN KEY (release_id, tenant_project_id, application_id)
        REFERENCES elitea_lifecycle.application_releases (id, tenant_project_id, application_id)
);

CREATE TABLE IF NOT EXISTS elitea_lifecycle.version_revisions (
    id                      bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    tenant_project_id       bigint NOT NULL CHECK (tenant_project_id > 0),
    application_id          bigint NOT NULL CHECK (application_id > 0),
    application_version_id  bigint NOT NULL CHECK (application_version_id > 0),
    revision                integer NOT NULL CHECK (revision >= 1),
    kind                    text NOT NULL DEFAULT 'save' CHECK (kind IN ('baseline', 'save')),
    actor_id                bigint CHECK (actor_id IS NULL OR actor_id > 0),
    created_at              timestamptz NOT NULL DEFAULT clock_timestamp(),
    snapshot                jsonb NOT NULL CHECK (jsonb_typeof(snapshot) = 'object'),
    diff                    jsonb NOT NULL DEFAULT '[]'::jsonb CHECK (jsonb_typeof(diff) = 'array'),
    CONSTRAINT version_revisions_number_key
        UNIQUE (tenant_project_id, application_version_id, revision)
);
