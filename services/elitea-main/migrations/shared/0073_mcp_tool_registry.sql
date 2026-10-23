-- 0073_mcp_tool_registry.sql — the durable store of MCP servers registered into
-- a project, and of the tools each server publishes (issue 335).
--
-- WHAT THIS ADDS. `GET /api/v2/elitea_core/tools_list/{projectID}` reports the
-- MCP servers that a project can drive, and the tools on each one. The Python
-- execution worker reads that list to build an agent's MCP tools. Until now the
-- only store was a dictionary inside a running legacy runtime process
-- (`elitea_core/utils/mcp_servers_storage.py`), so the hybrid edge sent the call
-- to the legacy runtime and an MCP agent could not run without it. These two
-- tables are the store, so the Go service can answer from rows.
--
-- WHY A NEW SCHEMA. The legacy runtime owns no MCP table. A search of every
-- `__tablename__` in the plugin tree returns no MCP relation, and no legacy
-- runtime migration creates one. So this corpus claims nothing that the legacy
-- runtime already owns, and the seeds cannot fail with 42P07.
--
-- WHY SHARED AND NOT TENANT. The read unions two projects: the caller's
-- personal project and the current one (the legacy runtime's
-- `methods/mcp_sse.py:get_registered_servers_private_and_current`). A per-tenant
-- table would make that one read into two reads against two schemas that are
-- migrated independently. One shared table with a `project_id` column answers it
-- in a single statement.
--
-- project_id CARRIES NO FOREIGN KEY. `centry.project` is a legacy-owned table,
-- and 0071 records the same decision for the same reason. Project membership is
-- checked by the route's authorization middleware before any row is read.
--
-- NO to_regclass GUARD IS NEEDED HERE. The guards in 0060, 0061, 0062 and 0071
-- protect a REFERENCES clause that points at a legacy runtime table which
-- `001_initial.sql` may not have created yet. This file references only its own
-- relations, so it is safe on an empty database.
--
-- SECRETS STAY OUT. The table holds a server name, a URL, timeouts and tool
-- descriptors. It does not hold the HTTP headers that authenticate a call to
-- that server. Bearer credentials do not enter this corpus (AGENTS.md).
--
-- IDEMPOTENT throughout: IF NOT EXISTS on the schema, both tables and every
-- index.
--
-- No BEGIN/COMMIT: the ledgered runner executes each file inside one
-- transaction with its ledger row (migrate/runner.go apply).

CREATE SCHEMA IF NOT EXISTS elitea_mcp;

CREATE TABLE IF NOT EXISTS elitea_mcp.registered_servers (
    id                 bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    project_id         bigint      NOT NULL,
    -- name is what the worker matches a toolkit against. The legacy runtime
    -- sanitises the name at registration; the Go writer applies the same rule.
    name               text        NOT NULL,
    -- server_url records where the tools came from. It is empty for a server
    -- that published its tools without an address.
    server_url         text        NOT NULL DEFAULT '',
    -- server_group is the legacy runtime's `group` field. It defaults to
    -- 'Other', which is the default on the pydantic model the worker reads.
    server_group       text        NOT NULL DEFAULT 'Other',
    timeout_tools_list integer     NOT NULL DEFAULT 90,
    timeout_tools_call integer     NOT NULL DEFAULT 90,
    created_at         timestamptz NOT NULL DEFAULT now(),
    updated_at         timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT registered_servers_name_present CHECK (length(btrim(name)) > 0)
);

-- One server name per project. The legacy runtime's dictionary has the same
-- key, so a second registration of the same name updates the entry instead of
-- adding a duplicate.
CREATE UNIQUE INDEX IF NOT EXISTS registered_servers_project_name_uniq
    ON elitea_mcp.registered_servers (project_id, name);

CREATE TABLE IF NOT EXISTS elitea_mcp.registered_tools (
    id           bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    server_id    bigint  NOT NULL
                 REFERENCES elitea_mcp.registered_servers (id) ON DELETE CASCADE,
    name         text    NOT NULL,
    description  text    NOT NULL DEFAULT '',
    -- input_schema is the MCP `inputSchema` object, stored as the server sent
    -- it. The worker gives it to the tool's argument model without changes.
    input_schema jsonb   NOT NULL DEFAULT '{"type": "object", "properties": {}, "required": []}'::jsonb,
    -- ordinal keeps the order the server published, so two reads of an
    -- unchanged server give the same list.
    ordinal      integer NOT NULL DEFAULT 0,
    CONSTRAINT registered_tools_name_present CHECK (length(btrim(name)) > 0)
);

CREATE UNIQUE INDEX IF NOT EXISTS registered_tools_server_name_uniq
    ON elitea_mcp.registered_tools (server_id, name);

-- The read asks "which tools does this server publish, in order".
CREATE INDEX IF NOT EXISTS registered_tools_server_ordinal
    ON elitea_mcp.registered_tools (server_id, ordinal);
