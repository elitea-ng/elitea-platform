-- 0145_form_auth_state.sql — the Form graph's sign-in state, moved off Redis.
--
-- WHAT MOVES. The Form graph (internal/authcomposition) kept three stores in
-- a dedicated "auth" Redis user until now:
--
--   1. the Form browser SESSION (internal/infra/authsession). It is created
--      unauthenticated at login begin and replaced by the authenticated state
--      at login complete, under a fresh identifier (rotation);
--   2. the one-time login TRANSACTION (internal/infra/authflow). It binds a
--      login begin to its callback and holds the PKCE verifier;
--   3. the ATTEMPT LIMITER (internal/infra/authattempt). It is a fixed window
--      per HMAC'd key that starts at the first attempt.
--
-- WHY POSTGRES. The Redis stores were multi-key Lua compare-and-swaps. One
-- PostgreSQL transaction reproduces each script exactly, which a key-value
-- store with per-key revisions cannot do for the two-key rotation or for the
-- multi-key limiter. Every path that reads these rows already needs this
-- database: Authorize re-validates the principal here on every edge request,
-- and provisioning writes here. So this adds no failure mode, and it removes
-- the auth Redis user and the Redis readiness check.
--
-- CORRECTION TO 0117's HEADER. 0117 calls `internal/infra/authsession` "the
-- Form graph's login-transaction store". It is the SESSION store; the
-- transaction store is `internal/infra/authflow`. 0117 is checksum-immutable,
-- so the correction is recorded here.
--
-- RECORDS ARE bytea, NOT jsonb. Rotation compares the stored record with the
-- one the caller read, byte for byte, and the transaction decoder requires
-- canonical JSON byte equality. jsonb re-serializes its input and would break
-- both checks.
--
-- EXPIRY IS A COLUMN, READ ON EVERY ACCESS. A row past `expires_at` (or
-- `window_ends_at`) is ABSENT to every read, exactly as an expired Redis key
-- was. Removal is the scheduler's job (internal/authstateretention); no
-- correctness depends on it.
--
-- NO DATA MIGRATION. Sessions last at most cookie.lifetime_seconds, login
-- transactions five minutes and attempt windows one minute. A deployment of
-- this file signs Form-session users out once.
--
-- IDEMPOTENT throughout. No BEGIN/COMMIT: the ledgered runner executes each
-- file inside one transaction with its ledger row (migrate/runner.go apply).

CREATE SCHEMA IF NOT EXISTS elitea_auth;

-- The Form browser session. The cookie carries `id`; the row is the session.
CREATE TABLE IF NOT EXISTS elitea_auth.form_sessions (
    -- 32 CSPRNG bytes, unpadded base64url (authsession.randomSessionID).
    id         text        PRIMARY KEY,
    -- The JSON-encoded sessionstate.State, at most 64 KiB.
    record     bytea       NOT NULL,
    expires_at timestamptz NOT NULL,
    CONSTRAINT form_sessions_id_shape
        CHECK (length(id) = 43),
    CONSTRAINT form_sessions_record_size
        CHECK (octet_length(record) BETWEEN 1 AND 65536)
);

CREATE INDEX IF NOT EXISTS form_sessions_expires_idx
    ON elitea_auth.form_sessions (expires_at);

-- The one-time login transaction. `provider` and `originating_session_id` are
-- the binding a callback must present before the row is consumed; a mismatch
-- leaves the row in place.
CREATE TABLE IF NOT EXISTS elitea_auth.form_login_transactions (
    -- 32 CSPRNG bytes, unpadded base64url (browserflow.NewTransactionID).
    id                     text        PRIMARY KEY,
    provider               text        NOT NULL,
    originating_session_id text        NOT NULL,
    -- The canonical JSON browserflow.Transaction, at most 64 KiB.
    record                 bytea       NOT NULL,
    expires_at             timestamptz NOT NULL,
    CONSTRAINT form_login_transactions_id_shape
        CHECK (length(id) = 43),
    CONSTRAINT form_login_transactions_provider_size
        CHECK (length(provider) BETWEEN 1 AND 64),
    CONSTRAINT form_login_transactions_session_size
        CHECK (length(originating_session_id) BETWEEN 1 AND 512),
    CONSTRAINT form_login_transactions_record_size
        CHECK (octet_length(record) BETWEEN 1 AND 65536)
);

CREATE INDEX IF NOT EXISTS form_login_transactions_expires_idx
    ON elitea_auth.form_login_transactions (expires_at);

-- One fixed attempt window. `key` is HMAC-SHA256 under the deployment's
-- attempt key over (stage, dimension, value), so a dump of this table does not
-- give client addresses or logins back by enumeration. A window whose
-- `window_ends_at` has passed counts as zero attempts.
CREATE TABLE IF NOT EXISTS elitea_auth.browser_attempt_windows (
    key            bytea       PRIMARY KEY,
    attempts       bigint      NOT NULL,
    window_ends_at timestamptz NOT NULL,
    CONSTRAINT browser_attempt_windows_key_size
        CHECK (octet_length(key) = 32),
    CONSTRAINT browser_attempt_windows_attempts_not_negative
        CHECK (attempts >= 0)
);

CREATE INDEX IF NOT EXISTS browser_attempt_windows_end_idx
    ON elitea_auth.browser_attempt_windows (window_ends_at);
