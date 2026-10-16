-- 0165_mcp_prebuilt_token_url.sql — an operator-pinned OAuth token endpoint for a
-- pre-built MCP catalogue entry.
--
-- WHY. Main releases a catalogue entry's vault-held client secret only to a
-- token endpoint an ADMIN controls. For an entry whose server_url is an exact
-- URL (no placeholders) that endpoint is discovered from the server itself;
-- for a URL template the user's own values select the host, so discovery from
-- it would let an editor choose where the secret goes. token_url is the
-- operator's pin for that case, and it takes precedence when set. Empty means
-- "not pinned".
ALTER TABLE elitea_mcp.prebuilt_servers
    ADD COLUMN IF NOT EXISTS token_url text NOT NULL DEFAULT '';
