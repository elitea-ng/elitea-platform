-- 0210_remote_agent_assertions.sql — one-time use of inbound delegation
-- assertions (ADR-0044 decision 4, issue #1501).
--
--   elitea_identity.remote_agent_assertion_replays
--       One row per accepted inbound delegation assertion: its issuer, its
--       jti and a digest of the credential that first presented it, kept
--       until the assertion expires (plus clock skew). The MCP endpoint
--       inserts the row before it serves the call. The same presenter may
--       present the assertion again (one MCP session sends one cached
--       assertion on each request); another presenter is a replay and is
--       refused (agent_assertion_replayed). An assertion lives at most 5 minutes, so
--       the table stays small: the verifier deletes expired rows in small
--       batches as it inserts new ones (index on expires_at).
--
-- PostgreSQL rather than NATS KV: the MCP endpoint is on the HTTP plane,
-- which always has the database and does not always have the runtime NATS
-- connection, and every Main replica must see the same record.
--
-- No permission: the endpoint and the admin section reuse existing grants
-- (configuration.governance). No data.
--
-- Additive and idempotent. No BEGIN/COMMIT: the ledgered runner wraps the file.

CREATE SCHEMA IF NOT EXISTS elitea_identity;

CREATE TABLE IF NOT EXISTS elitea_identity.remote_agent_assertion_replays (
    issuer      text NOT NULL CHECK (length(issuer) BETWEEN 1 AND 1024),
    jti         text NOT NULL CHECK (length(jti) BETWEEN 1 AND 128),
    presenter   text NOT NULL CHECK (presenter ~ '^[0-9a-f]{64}$'),
    expires_at  timestamptz NOT NULL,
    seen_at     timestamptz NOT NULL DEFAULT clock_timestamp(),
    PRIMARY KEY (issuer, jti)
);

CREATE INDEX IF NOT EXISTS remote_agent_assertion_replays_expiry
    ON elitea_identity.remote_agent_assertion_replays (expires_at);
