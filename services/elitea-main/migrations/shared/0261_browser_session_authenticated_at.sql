-- 0261_browser_session_authenticated_at.sql — when the person behind a browser
-- session last authenticated (ADR-0059 decision 5 and "Consequences,
-- Migrations"; story R1.10a, issue 2577).
--
-- A browser session row (0117, lifecycle 0200) records when it was created and
-- when it was last used. Neither says when its person last PROVED who they
-- are: a session refresh moves last_seen_at, and created_at is the moment the
-- row was written, which after a silent single sign-on can be long after the
-- person last typed a password at the identity provider. Three rules of
-- ADR-0059 need "this person authenticated within the last 15 minutes"
-- (administration scopes on tokens, starting an elevation, critical actions),
-- so the row now carries that time:
--
--   authenticated_at  the time of the person's last authentication. An OIDC
--                     sign-in writes the id_token's auth_time, or NULL when
--                     the provider sends none; SAML writes the sign-in time.
--                     Nothing moves it after the row is written: a refresh
--                     (Touch) writes only last_seen_at.
--
-- NULL IS UNKNOWN, and an unknown time never satisfies a recent-authentication
-- check. EXISTING ROWS take created_at, the only authentication time this
-- table ever knew. The column stays NULLABLE so a replica of the previous
-- release, which does not name it in its INSERT, keeps signing people in
-- during a rolling upgrade; such a row reads as unknown, which only ever asks
-- for one more sign-in.
--
-- Additive only. Idempotent. No BEGIN/COMMIT: the ledgered runner wraps the
-- file.

ALTER TABLE elitea_auth.browser_sessions
    ADD COLUMN IF NOT EXISTS authenticated_at timestamptz;

UPDATE elitea_auth.browser_sessions
SET authenticated_at = created_at
WHERE authenticated_at IS NULL;

COMMENT ON COLUMN elitea_auth.browser_sessions.authenticated_at IS
    'When the person behind this session last authenticated (ADR-0059 R1.10). Never moved by a refresh. NULL is unknown and never counts as a recent authentication.';
