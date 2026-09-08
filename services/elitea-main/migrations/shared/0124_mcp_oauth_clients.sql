-- Main owns confidential DCR credentials. Browsers retain only opaque references.
-- Applied receipts remain unchanged. The migration runner owns the transaction.
CREATE SCHEMA IF NOT EXISTS elitea_auth;
CREATE TABLE IF NOT EXISTS elitea_auth.mcp_oauth_clients (
    id text PRIMARY KEY CHECK (length(id) = 43),
    project_id integer NOT NULL CHECK (project_id > 0),
    actor_id integer NOT NULL CHECK (actor_id > 0),
    client_id text NOT NULL CHECK (length(client_id) BETWEEN 1 AND 4096),
    token_endpoint text NOT NULL CHECK (length(token_endpoint) BETWEEN 1 AND 4096),
    resource text NOT NULL DEFAULT '' CHECK (length(resource) <= 4096),
    encrypted_credentials bytea NOT NULL CHECK (octet_length(encrypted_credentials) BETWEEN 29 AND 32768),
    secret_expires_at timestamptz,
    idle_expires_at timestamptz NOT NULL DEFAULT (now() + interval '30 days')
);
CREATE INDEX IF NOT EXISTS mcp_oauth_clients_idle_expiry
    ON elitea_auth.mcp_oauth_clients (idle_expires_at, id);
