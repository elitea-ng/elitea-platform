-- 0185_entra_delegation_custody.sql — Entra delegation custody and governed
-- credential authorization requirements (ADR-0039 decisions 3 and 4, issue
-- #1117).
--
--   delegations (0181)        gains the encrypted user assertion: the
--                             AES-256-GCM ciphertext, its expiry and audience,
--                             the key label it was sealed under and the
--                             SHA-256 digest of the browser session that
--                             created it (logout revokes by that digest).
--   signin_assertions         the middle-tier access token retained at a
--                             governed Entra sign-in, before any agent binding
--                             exists. A delegation is created from it when a
--                             governed run needs one.
--   credential_authorization_requirements
--                             a governed run paused because Entra asked for
--                             interaction or answered with a claims challenge.
--                             The opaque reference is what the API returns.
--
-- The ciphertext AAD binds the row's identity columns (see
-- internal/infra/identity/entra/delegationstore). Revocation sets state
-- 'revoked', clears the ciphertext and bumps revocation_epoch in one
-- transaction. No column holds a token in plaintext; a browser session id is
-- stored only as its digest.
--
-- With ELITEA_ENTERPRISE_CREDENTIAL_GOVERNANCE_ENABLED off nothing writes or
-- reads these columns and tables. Additive only. Idempotent throughout. No
-- BEGIN/COMMIT: the ledgered runner wraps the file.

ALTER TABLE elitea_identity.delegations
    ADD COLUMN IF NOT EXISTS assertion_ciphertext   bytea,
    ADD COLUMN IF NOT EXISTS assertion_expires_at   timestamptz,
    ADD COLUMN IF NOT EXISTS assertion_audience     text,
    ADD COLUMN IF NOT EXISTS key_label              text,
    ADD COLUMN IF NOT EXISTS browser_session_digest bytea;

DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint
        WHERE conname = 'delegations_assertion_complete'
          AND conrelid = 'elitea_identity.delegations'::regclass
    ) THEN
        -- A sealed assertion always carries what its AAD and its expiry need.
        ALTER TABLE elitea_identity.delegations
            ADD CONSTRAINT delegations_assertion_complete CHECK (
                assertion_ciphertext IS NULL
                OR (assertion_expires_at IS NOT NULL
                    AND assertion_audience IS NOT NULL
                    AND length(assertion_audience) BETWEEN 1 AND 512
                    AND key_label IS NOT NULL
                    AND length(key_label) BETWEEN 1 AND 128)
            );
    END IF;
    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint
        WHERE conname = 'delegations_revoked_is_wiped'
          AND conrelid = 'elitea_identity.delegations'::regclass
    ) THEN
        ALTER TABLE elitea_identity.delegations
            ADD CONSTRAINT delegations_revoked_is_wiped
            CHECK (state <> 'revoked' OR assertion_ciphertext IS NULL);
    END IF;
    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint
        WHERE conname = 'delegations_session_digest_length'
          AND conrelid = 'elitea_identity.delegations'::regclass
    ) THEN
        ALTER TABLE elitea_identity.delegations
            ADD CONSTRAINT delegations_session_digest_length
            CHECK (browser_session_digest IS NULL OR octet_length(browser_session_digest) = 32);
    END IF;
END
$$;

CREATE INDEX IF NOT EXISTS delegations_browser_session
    ON elitea_identity.delegations (browser_session_digest)
    WHERE browser_session_digest IS NOT NULL AND state <> 'revoked';

CREATE TABLE IF NOT EXISTS elitea_identity.signin_assertions (
    id                      uuid PRIMARY KEY,
    browser_session_digest  bytea NOT NULL CHECK (octet_length(browser_session_digest) = 32),
    user_id                 bigint NOT NULL CHECK (user_id > 0),
    issuer                  text NOT NULL CHECK (length(issuer) BETWEEN 1 AND 512),
    tenant_id               text NOT NULL CHECK (length(tenant_id) BETWEEN 1 AND 128),
    subject_object_id       text NOT NULL CHECK (length(subject_object_id) BETWEEN 1 AND 128),
    audience                text NOT NULL CHECK (length(audience) BETWEEN 1 AND 512),
    scopes                  text[] NOT NULL DEFAULT '{}' CHECK (cardinality(scopes) <= 64),
    assertion_ciphertext    bytea,
    assertion_expires_at    timestamptz NOT NULL,
    key_label               text NOT NULL CHECK (length(key_label) BETWEEN 1 AND 128),
    state                   text NOT NULL DEFAULT 'active'
                            CHECK (state IN ('active', 'revoked')),
    revocation_epoch        bigint NOT NULL DEFAULT 0 CHECK (revocation_epoch >= 0),
    created_at              timestamptz NOT NULL DEFAULT clock_timestamp(),
    updated_at              timestamptz NOT NULL DEFAULT clock_timestamp(),
    CONSTRAINT signin_assertions_active_is_sealed
        CHECK ((state = 'active') = (assertion_ciphertext IS NOT NULL))
);

CREATE INDEX IF NOT EXISTS signin_assertions_session
    ON elitea_identity.signin_assertions (browser_session_digest)
    WHERE state = 'active';

CREATE TABLE IF NOT EXISTS elitea_identity.credential_authorization_requirements (
    id                    uuid PRIMARY KEY,
    -- The opaque authorization_reference the API returns. Random; it names
    -- the requirement and grants nothing.
    reference             text NOT NULL UNIQUE CHECK (length(reference) BETWEEN 16 AND 128),
    tenant_project_id     bigint NOT NULL CHECK (tenant_project_id > 0),
    actor_user_id         bigint NOT NULL CHECK (actor_user_id > 0),
    conversation_uuid     uuid NOT NULL,
    response_message_id   uuid NOT NULL,
    execution_id          text NOT NULL CHECK (length(execution_id) BETWEEN 1 AND 128),
    execution_generation  text NOT NULL CHECK (length(execution_generation) BETWEEN 1 AND 128),
    delegation_id         uuid NOT NULL,
    scopes                text[] NOT NULL CHECK (cardinality(scopes) BETWEEN 1 AND 64),
    reason                text NOT NULL CHECK (reason IN ('interaction_required', 'claims_challenge')),
    -- The CAE claims challenge JSON. A challenge names required claims; it
    -- is not a credential.
    claims_challenge      text NOT NULL DEFAULT '' CHECK (length(claims_challenge) <= 8192),
    state                 text NOT NULL DEFAULT 'pending'
                          CHECK (state IN ('pending', 'resolved')),
    resolved_delegation_id uuid,
    created_at            timestamptz NOT NULL DEFAULT clock_timestamp(),
    resolved_at           timestamptz,
    CONSTRAINT credential_authorization_requirements_resolution
        CHECK ((state = 'resolved') = (resolved_at IS NOT NULL AND resolved_delegation_id IS NOT NULL))
);

CREATE INDEX IF NOT EXISTS credential_authorization_requirements_execution
    ON elitea_identity.credential_authorization_requirements (execution_id);
