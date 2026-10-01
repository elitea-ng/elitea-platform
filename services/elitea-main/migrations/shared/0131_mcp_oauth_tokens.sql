-- Main owns delegated OAuth grants separately from registered client secrets.
-- The deployment master key encrypts each immutable, actor-bound grant.
CREATE SCHEMA IF NOT EXISTS elitea_auth;
CREATE TABLE IF NOT EXISTS elitea_auth.mcp_oauth_tokens (
    id text PRIMARY KEY CHECK (length(id) = 43),
    project_id integer NOT NULL CHECK (project_id > 0),
    actor_id integer NOT NULL CHECK (actor_id > 0),
    toolkit_id bigint NOT NULL CHECK (toolkit_id > 0),
    resource text NOT NULL CHECK (length(resource) BETWEEN 1 AND 4096),
    revision bigint NOT NULL CHECK (revision = 1),
    encrypted_token bytea NOT NULL CHECK (octet_length(encrypted_token) BETWEEN 29 AND 32768),
    expires_at timestamptz NOT NULL,
    revoked_at timestamptz
);
CREATE INDEX IF NOT EXISTS mcp_oauth_tokens_expiry ON elitea_auth.mcp_oauth_tokens (expires_at, id);
