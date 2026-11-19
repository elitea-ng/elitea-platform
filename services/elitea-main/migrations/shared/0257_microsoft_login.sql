-- 0257_microsoft_login.sql — the server side of a Microsoft sign-in
-- (ADR-0057 decision 6; epic 2254 story S1c, issue 2268).
--
-- One https callback on Main serves every client (web popup, desktop, mobile) and
-- both app modes (Custom app, Default app). Two short-lived tables carry the flow:
--
--   elitea_identity.microsoft_login_states    a started sign-in: what it is bound
--                                             to (user, project, credential, app,
--                                             client kind), the exact redirect
--                                             URI and the sealed PKCE verifier.
--                                             10 minutes, single use: the callback
--                                             consumes the row. The state value
--                                             itself is stored only as a digest.
--   elitea_identity.microsoft_login_handoffs  a finished sign-in waiting for the
--                                             person who STARTED it. The callback
--                                             attaches nothing to anyone; it parks
--                                             the sealed access token here, for 60
--                                             seconds, and the starter redeems it
--                                             over the authenticated API (session
--                                             on web, the verifier of
--                                             client_challenge on desktop and
--                                             mobile). That is what stops a login
--                                             CSRF: a victim who finishes a link
--                                             an attacker started cannot redeem it
--                                             as the attacker, and it expires.
--
-- NO TOKEN OR SECRET IN PLAINTEXT. The verifier and the access token are
-- AES-256-GCM under a key derived with HKDF-SHA256 from the deployment master key
-- under the label `elitea/microsoft-logins/v1`; the AAD binds a ciphertext to its
-- row and to the user, project, credential, app, client and scope (internal/
-- microsoftlogin). The Custom app's client secret is not stored here at all: the
-- callback reads it again from the credential. A refresh token is not stored: the
-- stored login (story S1h) adds its own table and column.
--
-- Rows are deleted by the redemption (handoff) and by the next start (expired
-- states and handoffs). Additive and idempotent. No BEGIN/COMMIT: the ledgered
-- runner wraps the file.

CREATE SCHEMA IF NOT EXISTS elitea_identity;

CREATE TABLE IF NOT EXISTS elitea_identity.microsoft_login_states (
    state_digest          bytea       PRIMARY KEY CHECK (octet_length(state_digest) = 32),
    user_id               bigint      NOT NULL CHECK (user_id > 0),
    tenant_project_id     bigint      NOT NULL CHECK (tenant_project_id > 0),
    credential_id         uuid        NOT NULL,
    credential_type       text        NOT NULL
                                      CHECK (credential_type IN ('outlook', 'teams', 'ms_graph', 'dataverse', 'sharepoint')),
    app_ref               text        NOT NULL
                                      CHECK (app_ref = 'custom'
                                             OR app_ref ~ '^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$'),
    client_kind           text        NOT NULL CHECK (client_kind IN ('web', 'desktop', 'mobile')),
    client_challenge      text        CHECK (client_challenge IS NULL OR client_challenge ~ '^[A-Za-z0-9_-]{43}$'),
    return_port           integer     CHECK (return_port IS NULL OR return_port BETWEEN 1024 AND 65535),
    return_scheme         text        CHECK (return_scheme IS NULL OR return_scheme ~ '^[a-z][a-z0-9+.-]{1,31}$'),
    client_id_fingerprint text        NOT NULL CHECK (client_id_fingerprint ~ '^[0-9a-f]{64}$'),
    redirect_uri          text        NOT NULL CHECK (length(redirect_uri) BETWEEN 1 AND 2048),
    authority             text        NOT NULL CHECK (length(authority) BETWEEN 1 AND 512),
    scope                 text        NOT NULL CHECK (length(scope) <= 4096),
    verifier_ciphertext   bytea       NOT NULL,
    key_label             text        NOT NULL CHECK (length(key_label) BETWEEN 1 AND 128),
    expires_at            timestamptz NOT NULL,
    consumed_at           timestamptz,
    created_at            timestamptz NOT NULL DEFAULT clock_timestamp(),
    -- A web popup redeems with its session; desktop and mobile prove they are the
    -- starter with the verifier of a challenge, and say where to return.
    CONSTRAINT microsoft_login_states_client_shape CHECK (
        (client_kind = 'web'     AND client_challenge IS NULL     AND return_port IS NULL     AND return_scheme IS NULL)
     OR (client_kind = 'desktop' AND client_challenge IS NOT NULL AND return_port IS NOT NULL AND return_scheme IS NULL)
     OR (client_kind = 'mobile'  AND client_challenge IS NOT NULL AND return_port IS NULL     AND return_scheme IS NOT NULL))
);

CREATE INDEX IF NOT EXISTS microsoft_login_states_expiry
    ON elitea_identity.microsoft_login_states (expires_at);
CREATE INDEX IF NOT EXISTS microsoft_login_states_user
    ON elitea_identity.microsoft_login_states (user_id, expires_at);

CREATE TABLE IF NOT EXISTS elitea_identity.microsoft_login_handoffs (
    handoff_digest          bytea       PRIMARY KEY CHECK (octet_length(handoff_digest) = 32),
    state_digest            bytea       NOT NULL UNIQUE CHECK (octet_length(state_digest) = 32),
    user_id                 bigint      NOT NULL CHECK (user_id > 0),
    tenant_project_id       bigint      NOT NULL CHECK (tenant_project_id > 0),
    credential_id           uuid        NOT NULL,
    credential_type         text        NOT NULL
                                        CHECK (credential_type IN ('outlook', 'teams', 'ms_graph', 'dataverse', 'sharepoint')),
    app_ref                 text        NOT NULL,
    client_kind             text        NOT NULL CHECK (client_kind IN ('web', 'desktop', 'mobile')),
    client_challenge        text        CHECK (client_challenge IS NULL OR client_challenge ~ '^[A-Za-z0-9_-]{43}$'),
    client_id_fingerprint   text        NOT NULL CHECK (client_id_fingerprint ~ '^[0-9a-f]{64}$'),
    authority               text        NOT NULL,
    scope                   text        NOT NULL,
    entra_tid               text        NOT NULL CHECK (length(entra_tid) BETWEEN 1 AND 64),
    entra_oid               text        NOT NULL CHECK (length(entra_oid) BETWEEN 1 AND 64),
    account_name            text        NOT NULL DEFAULT '' CHECK (length(account_name) <= 256),
    access_token_ciphertext bytea       NOT NULL,
    key_label               text        NOT NULL CHECK (length(key_label) BETWEEN 1 AND 128),
    access_expires_at       timestamptz NOT NULL,
    expires_at              timestamptz NOT NULL,
    created_at              timestamptz NOT NULL DEFAULT clock_timestamp()
);

CREATE INDEX IF NOT EXISTS microsoft_login_handoffs_expiry
    ON elitea_identity.microsoft_login_handoffs (expires_at);

COMMENT ON COLUMN elitea_identity.microsoft_login_handoffs.access_token_ciphertext IS
    'AES-256-GCM, 60 seconds, deleted at redemption: no API response, log or export carries the token (ADR-0057 decision 6).';
