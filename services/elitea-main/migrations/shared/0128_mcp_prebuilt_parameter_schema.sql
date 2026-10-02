-- Add project-supplied fields for operator-owned pre-built MCP templates.
-- Secret values remain in each project's hidden-secret vault.

ALTER TABLE elitea_mcp.prebuilt_servers
    ADD COLUMN IF NOT EXISTS config_schema jsonb NOT NULL
        DEFAULT '{"properties": {}}'::jsonb;

ALTER TABLE elitea_mcp.prebuilt_servers
    DROP CONSTRAINT IF EXISTS prebuilt_servers_config_schema_object;

ALTER TABLE elitea_mcp.prebuilt_servers
    ADD CONSTRAINT prebuilt_servers_config_schema_object
        CHECK (
            jsonb_typeof(config_schema) = 'object'
            AND (
                NOT (config_schema ? 'properties')
                OR jsonb_typeof(config_schema -> 'properties') = 'object'
            )
        );
