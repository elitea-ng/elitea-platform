-- name: InsertMCPOAuthClient :exec
INSERT INTO elitea_auth.mcp_oauth_clients
    (id, project_id, actor_id, client_id, token_endpoint, resource, encrypted_credentials, secret_expires_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8);

-- name: LoadMCPOAuthClient :one
UPDATE elitea_auth.mcp_oauth_clients
SET idle_expires_at = clock_timestamp() + interval '30 days'
WHERE id = $1 AND project_id = $2 AND actor_id = $3
  AND client_id = $4 AND token_endpoint = $5 AND resource = $6
  AND idle_expires_at > clock_timestamp()
  AND (secret_expires_at IS NULL OR secret_expires_at > clock_timestamp())
RETURNING encrypted_credentials;

-- name: PruneMCPOAuthClients :exec
DELETE FROM elitea_auth.mcp_oauth_clients
WHERE id IN (
    SELECT id FROM elitea_auth.mcp_oauth_clients
    WHERE idle_expires_at <= clock_timestamp()
    ORDER BY idle_expires_at, id
    LIMIT 128
);
