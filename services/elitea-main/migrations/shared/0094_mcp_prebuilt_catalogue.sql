-- 0094_mcp_prebuilt_catalogue.sql — the PRE-BUILT MCP server catalogue: the
-- platform-wide list of MCP servers an operator offers as ready-made toolkits.
--
-- WHAT THIS REPLACES. In the legacy runtime the catalogue is a block of the
-- indexer_worker plugin's YAML descriptor:
--
--     mcp_servers:
--       Epam Presales:
--         client_id: "…"
--         client_secret: "…"
--         timeout: 30
--         base_url: "https://api.example.com"
--       GitHub Copilot:
--         url: "https://api.githubcopilot.com/mcp/"
--
-- `indexer_worker/methods/indexer_mcp_prebuilt_config.py` reads that block,
-- normalises each key and emits `application_mcp_prebuilt_config_collected` on
-- the Arbiter event bus; `elitea_core` caches the payload in module state
-- (`self.mcp_prebuilt_configs`) and serves it to five call sites through
-- `get_mcp_prebuilt_config()` and `resolve_mcp_prebuilt_settings()`.
--
-- None of that machinery exists here: this service loads no plugins, has no
-- descriptor to patch and speaks no Arbiter. So the admin Configuration page's
-- "MCP Servers" section had nothing behind it, and
-- `internal/api/v2/eliteacore/handler.go` refused a URL-less sync with the
-- sentence "this service does not hold the pre-built MCP catalogue". This table
-- is that catalogue.
--
-- WHY A TABLE AND NOT A `centry.platform_config` ROW. The catalogue holds
-- credentials — the legacy runtime's own example block carries `client_secret`
-- in clear text. `internal/api/v2/admin/config_values.go:rejectCredentialField`
-- refuses any credential into a plaintext platform-configuration row, and that
-- rule is right: those rows are readable by every holder of `runtime.plugins`.
-- So the secret does NOT live here either. `client_secret_ref` names an entry
-- in the GLOBAL secret vault (`centry.secrets_key`/`secrets_data`, vault id
-- `admin`), and the value is sealed there by the admin write path. This table
-- holds the reference, never the secret.
--
-- HTTP ONLY, AND THAT IS NOT AN OMISSION. The reference page's field help says
-- the section "supports stdio (local subprocess) and http (remote) server
-- types". A stdio server is a child process on the host that runs the MCP
-- client; this service starts no subprocesses and has no host to start one on,
-- and `internal/mcpregistry.Discoverer` speaks streamable HTTP and nothing
-- else. Storing a stdio row would be storing a definition that no code path in
-- this stack can honour, so the catalogue is HTTP-only and the admin write
-- refuses a stdio definition with that reason rather than accepting it into a
-- column nobody reads.
--
-- WHY SHARED. The catalogue is platform-wide by construction: the legacy
-- runtime's copy is one dictionary per deployment, offered to every project. A
-- per-tenant table would make one platform fact into N copies that drift.
--
-- catalogue_key IS THE NORMALISED NAME, and it is what a toolkit type is
-- matched against. The legacy runtime normalises on both sides — the producer
-- lowercases the YAML key and turns spaces into underscores, and
-- `normalize_mcp_toolkit_name` additionally strips a leading `mcp_` from the
-- toolkit type before the lookup. Storing the normalised form is what makes the
-- lookup a single indexed read instead of a scan with a transform.
--
-- NO FOREIGN KEYS. The table references nothing. 0073 records the same for
-- `project_id`, and this table has no project at all.
--
-- IDEMPOTENT throughout. No BEGIN/COMMIT: the ledgered runner executes each
-- file inside one transaction with its ledger row (migrate/runner.go apply).

CREATE SCHEMA IF NOT EXISTS elitea_mcp;

CREATE TABLE IF NOT EXISTS elitea_mcp.prebuilt_servers (
    id                bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    -- catalogue_key is the normalised lookup key: lowercase, spaces replaced by
    -- underscores, trimmed, with any leading `mcp_` removed.
    catalogue_key     text        NOT NULL,
    -- display_name is the legacy runtime's `original_name` — the key exactly as
    -- the operator wrote it, kept so the admin page can show "Epam Presales"
    -- rather than "epam_presales".
    display_name      text        NOT NULL,
    -- server_url is the legacy runtime's `url`: the MCP endpoint tools are
    -- discovered from.
    server_url        text        NOT NULL DEFAULT '',
    -- base_url is the legacy runtime's `base_url`, a separate field in its
    -- example block and carried for the toolkits whose settings expect it.
    base_url          text        NOT NULL DEFAULT '',
    client_id         text        NOT NULL DEFAULT '',
    -- client_secret_ref names the entry in the GLOBAL vault. Empty means the
    -- catalogue entry has no client secret, which is distinct from "the secret
    -- is empty".
    client_secret_ref text        NOT NULL DEFAULT '',
    -- timeout_seconds is the legacy runtime's `timeout`. Zero means "not
    -- configured", so the caller's own default applies rather than a
    -- zero-second timeout.
    timeout_seconds   integer     NOT NULL DEFAULT 0,
    -- headers are the non-secret HTTP headers merged into a discovery request.
    headers           jsonb       NOT NULL DEFAULT '{}'::jsonb,
    -- enabled lets an operator withdraw an entry without deleting it and losing
    -- its sealed secret reference.
    enabled           boolean     NOT NULL DEFAULT true,
    created_at        timestamptz NOT NULL DEFAULT now(),
    updated_at        timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT prebuilt_servers_key_present
        CHECK (length(btrim(catalogue_key)) > 0),
    CONSTRAINT prebuilt_servers_display_name_present
        CHECK (length(btrim(display_name)) > 0),
    CONSTRAINT prebuilt_servers_timeout_not_negative
        CHECK (timeout_seconds >= 0),
    -- headers is an object, not an array or a scalar. A malformed value would
    -- reach the resolver as something it cannot merge.
    CONSTRAINT prebuilt_servers_headers_object
        CHECK (jsonb_typeof(headers) = 'object')
);

-- One entry per normalised key. That is the legacy runtime's dictionary key, so
-- a second definition of the same name replaces the first rather than adding a
-- duplicate that the lookup would resolve arbitrarily.
CREATE UNIQUE INDEX IF NOT EXISTS prebuilt_servers_key_uniq
    ON elitea_mcp.prebuilt_servers (catalogue_key);
