-- SQLC projection of shared migration 0124. This file is not a runtime migration.
CREATE SCHEMA IF NOT EXISTS elitea_auth;
CREATE TABLE elitea_auth.mcp_oauth_clients (
    id text PRIMARY KEY,
    project_id integer NOT NULL,
    actor_id integer NOT NULL,
    client_id text NOT NULL,
    token_endpoint text NOT NULL,
    resource text NOT NULL DEFAULT '',
    encrypted_credentials bytea NOT NULL,
    secret_expires_at timestamptz,
    idle_expires_at timestamptz NOT NULL DEFAULT (now() + interval '30 days')
);
