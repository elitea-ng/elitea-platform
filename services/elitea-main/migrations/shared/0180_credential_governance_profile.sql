-- 0180_credential_governance_profile.sql — the enterprise credential
-- governance profile (ADR-0039, issue #1111).
--
--   credential_policy_revisions  immutable, operator-written policy
--                                revisions. `canonical` holds the exact
--                                bytes `digest` hashes (jsonb would
--                                reformat them); `document` is the same
--                                content for reading.
--   execution_admission_profiles the profile and revision one execution
--                                was admitted under. Written in the
--                                admission transaction, only while the
--                                ELITEA_ENTERPRISE_CREDENTIAL_GOVERNANCE_ENABLED
--                                flag is on. An execution with no row is
--                                legacy, so a flag-off deployment writes
--                                nothing here.
--
-- Both tables reject UPDATE (and the revisions DELETE): a pin never
-- downgrades and a revision is never edited. Additive only; no data.
-- Idempotent throughout. No BEGIN/COMMIT: the ledgered runner wraps the file.

CREATE SCHEMA IF NOT EXISTS elitea_identity;

CREATE TABLE IF NOT EXISTS elitea_identity.credential_policy_revisions (
    id          text PRIMARY KEY CHECK (id ~ '^[a-z0-9][a-z0-9._-]{0,62}$'),
    digest      bytea NOT NULL CHECK (octet_length(digest) = 32),
    canonical   bytea NOT NULL CHECK (octet_length(canonical) BETWEEN 2 AND 65536),
    document    jsonb NOT NULL CHECK (document ->> 'schema' = 'elitea.credential-policy.v1'),
    -- The operator identity that ran `elitea-main credential-policy put`.
    created_by  text NOT NULL CHECK (length(created_by) BETWEEN 1 AND 256),
    created_at  timestamptz NOT NULL DEFAULT clock_timestamp()
);

CREATE OR REPLACE FUNCTION elitea_identity.credential_policy_revision_immutable()
RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'credential policy revisions are immutable';
END
$$;

DROP TRIGGER IF EXISTS credential_policy_revisions_immutable
    ON elitea_identity.credential_policy_revisions;
CREATE TRIGGER credential_policy_revisions_immutable
    BEFORE UPDATE OR DELETE ON elitea_identity.credential_policy_revisions
    FOR EACH ROW EXECUTE FUNCTION elitea_identity.credential_policy_revision_immutable();

CREATE TABLE IF NOT EXISTS elitea_runtime.execution_admission_profiles (
    execution_id        text PRIMARY KEY CHECK (length(execution_id) BETWEEN 1 AND 128),
    profile             text NOT NULL CHECK (profile IN (
                            'elitea.credential-profile.legacy.v1',
                            'elitea.credential-profile.governed.v1')),
    policy_revision_id  text NULL REFERENCES elitea_identity.credential_policy_revisions (id),
    policy_digest       bytea NULL CHECK (policy_digest IS NULL OR octet_length(policy_digest) = 32),
    -- The logical run a continuation belongs to (an agent conversation): a
    -- continuation copies the newest governed pin of its scope, so a HITL
    -- resume after a flag change never downgrades (profile spec item 5.2).
    continuation_scope  text NULL CHECK (continuation_scope IS NULL OR length(continuation_scope) BETWEEN 1 AND 128),
    pinned_at           timestamptz NOT NULL DEFAULT clock_timestamp(),
    CONSTRAINT execution_admission_profiles_revision_shape CHECK (
        (profile = 'elitea.credential-profile.governed.v1'
            AND policy_revision_id IS NOT NULL AND policy_digest IS NOT NULL)
        OR (profile = 'elitea.credential-profile.legacy.v1'
            AND policy_revision_id IS NULL AND policy_digest IS NULL)
    )
);

CREATE INDEX IF NOT EXISTS execution_admission_profiles_scope_governed
    ON elitea_runtime.execution_admission_profiles (continuation_scope, pinned_at DESC)
    WHERE continuation_scope IS NOT NULL
      AND profile = 'elitea.credential-profile.governed.v1';

CREATE OR REPLACE FUNCTION elitea_runtime.execution_admission_profile_immutable()
RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'execution admission profiles are immutable';
END
$$;

DROP TRIGGER IF EXISTS execution_admission_profiles_immutable
    ON elitea_runtime.execution_admission_profiles;
CREATE TRIGGER execution_admission_profiles_immutable
    BEFORE UPDATE ON elitea_runtime.execution_admission_profiles
    FOR EACH ROW EXECUTE FUNCTION elitea_runtime.execution_admission_profile_immutable();
