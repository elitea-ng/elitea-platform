-- 0190_provider_oauth_grants.sql — provider delegated OAuth custody, the
-- consent flow and single-flight refresh across replicas (ADR-0039 decision
-- 3, issue #1121 items 3 and 4).
--
--   elitea_identity.provider_oauth_grants
--                             one grant per (tenant project, provider, target
--                             binding, subject user, agent binding): the
--                             user consented to the provider directly
--                             (Atlassian 3LO, Aha! OAuth2). The refresh token
--                             and the current access token are AES-256-GCM
--                             ciphertexts. generation increases by one on
--                             every token write, so a write that read an older
--                             generation is refused.
--   elitea_identity.provider_oauth_consent_states
--                             the server side of a consent start: the SHA-256
--                             digest of the state value, what the state is
--                             bound to (user, tenant project, agent binding,
--                             target binding, provider), the exact redirect
--                             URI and the sealed PKCE verifier. Short TTL,
--                             single use: the callback consumes the row.
--   elitea_runtime.credential_refresh_leases
--                             the cross-replica refresh lease. Acquire is an
--                             INSERT ... ON CONFLICT DO UPDATE ... WHERE
--                             expires_at < now() that returns a new fence;
--                             the token write-back is accepted only for the
--                             current fence.
--
-- The ciphertext AAD binds tenant | grant id | provider | target | subject |
-- agent binding (see internal/provideroauth). No column holds a token or a
-- PKCE verifier in plaintext, and a state value is stored only as its digest.
--
-- Written only by a process with credential governance on, so a flag-off
-- deployment writes and reads nothing here. Additive only. Idempotent
-- throughout. No BEGIN/COMMIT: the ledgered runner wraps the file.

CREATE TABLE IF NOT EXISTS elitea_identity.provider_oauth_grants (
    id                        uuid PRIMARY KEY,
    tenant_project_id         bigint NOT NULL CHECK (tenant_project_id > 0),
    provider_type             text NOT NULL CHECK (provider_type ~ '^[a-z0-9][a-z0-9_-]{0,63}$'),
    target_binding_id         uuid NOT NULL,
    subject_user_id           bigint NOT NULL CHECK (subject_user_id > 0),
    agent_binding_id          uuid NOT NULL,
    state                     text NOT NULL DEFAULT 'active'
                              CHECK (state IN ('active', 'invalid', 'revoked')),
    generation                bigint NOT NULL DEFAULT 1 CHECK (generation > 0),
    -- Bumped when the grant stops being usable (invalid or revoked), for the
    -- revocation epoch checks of #1121.
    revocation_epoch          bigint NOT NULL DEFAULT 0 CHECK (revocation_epoch >= 0),
    refresh_token_ciphertext  bytea,
    access_token_ciphertext   bytea,
    -- NULL when the provider stated no lifetime.
    access_token_expires_at   timestamptz,
    granted_scopes            text[] NOT NULL DEFAULT '{}' CHECK (cardinality(granted_scopes) <= 64),
    -- Consent metadata: when and through which exact redirect URI the user
    -- consented. Never a token.
    consented_at              timestamptz NOT NULL,
    consent_redirect_uri      text NOT NULL CHECK (length(consent_redirect_uri) BETWEEN 1 AND 2048),
    key_label                 text NOT NULL CHECK (length(key_label) BETWEEN 1 AND 128),
    refreshed_at              timestamptz,
    created_at                timestamptz NOT NULL DEFAULT clock_timestamp(),
    updated_at                timestamptz NOT NULL DEFAULT clock_timestamp(),
    CONSTRAINT provider_oauth_grants_key
        UNIQUE (tenant_project_id, provider_type, target_binding_id, subject_user_id, agent_binding_id),
    CONSTRAINT provider_oauth_grants_active_is_sealed
        CHECK (state <> 'active' OR access_token_ciphertext IS NOT NULL),
    CONSTRAINT provider_oauth_grants_revoked_is_wiped
        CHECK (state <> 'revoked' OR (access_token_ciphertext IS NULL AND refresh_token_ciphertext IS NULL))
);

CREATE TABLE IF NOT EXISTS elitea_identity.provider_oauth_consent_states (
    state_digest         bytea PRIMARY KEY CHECK (octet_length(state_digest) = 32),
    provider_type        text NOT NULL CHECK (provider_type ~ '^[a-z0-9][a-z0-9_-]{0,63}$'),
    tenant_project_id    bigint NOT NULL CHECK (tenant_project_id > 0),
    user_id              bigint NOT NULL CHECK (user_id > 0),
    agent_binding_id     uuid NOT NULL,
    target_binding_id    uuid NOT NULL,
    redirect_uri         text NOT NULL CHECK (length(redirect_uri) BETWEEN 1 AND 2048),
    verifier_ciphertext  bytea NOT NULL,
    key_label            text NOT NULL CHECK (length(key_label) BETWEEN 1 AND 128),
    expires_at           timestamptz NOT NULL,
    consumed_at          timestamptz,
    created_at           timestamptz NOT NULL DEFAULT clock_timestamp()
);

CREATE INDEX IF NOT EXISTS provider_oauth_consent_states_expiry
    ON elitea_identity.provider_oauth_consent_states (expires_at);

CREATE TABLE IF NOT EXISTS elitea_runtime.credential_refresh_leases (
    partition_hash  bytea PRIMARY KEY CHECK (octet_length(partition_hash) = 32),
    holder          text NOT NULL CHECK (length(holder) <= 256),
    fence           bigint NOT NULL CHECK (fence > 0),
    expires_at      timestamptz NOT NULL,
    generation      bigint NOT NULL CHECK (generation >= 0)
);
