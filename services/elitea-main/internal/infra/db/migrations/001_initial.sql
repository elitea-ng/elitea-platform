-- 001_initial.sql
-- Drop-in compatible schema for the pylon platform.
-- Shared schema: "centry" (projects, users, groups)
-- Tenant schema: "p_{project_id}" (per-project data)

BEGIN;

-- =============================================================================
-- SHARED SCHEMA (centry)
-- =============================================================================
CREATE SCHEMA IF NOT EXISTS centry;

-- Projects
CREATE TABLE IF NOT EXISTS centry.project (
    id SERIAL PRIMARY KEY,
    name VARCHAR(256) NOT NULL,
    owner_id INTEGER NOT NULL,
    secrets_json JSONB DEFAULT '{}'::jsonb,
    plugins TEXT[] DEFAULT '{}',
    keycloak_groups JSONB NOT NULL DEFAULT '{}'::jsonb,
    create_success BOOLEAN NOT NULL DEFAULT false,
    suspended BOOLEAN NOT NULL DEFAULT false
);

-- Project groups
CREATE TABLE IF NOT EXISTS centry.project_group (
    id SERIAL PRIMARY KEY,
    name VARCHAR(256) NOT NULL UNIQUE
);

-- Project <-> Group association
CREATE TABLE IF NOT EXISTS centry.project_group_association (
    project_id INTEGER REFERENCES centry.project(id) ON DELETE CASCADE,
    group_id INTEGER REFERENCES centry.project_group(id) ON DELETE CASCADE,
    PRIMARY KEY (project_id, group_id)
);

-- Per-project resource ceilings and usage counters (issue #246).
-- Column-for-column legacy/plugins/projects/models/{quota,statistics}.py, so a
-- fresh database and a legacy dump present the same table to
-- /api/v2/projects/{quota,statistics}. The same DDL lands on existing
-- databases through migrations/shared/0062_budgets_quota_statistics.sql — see
-- that file for why -1 is the "unlimited" sentinel and why the project_id
-- index is deliberately NOT unique.
CREATE TABLE IF NOT EXISTS centry.project_quota (
    id                        SERIAL PRIMARY KEY,
    project_id                INTEGER NOT NULL,
    data_retention_limit      INTEGER,
    test_duration_limit       INTEGER DEFAULT -1,
    cpu_limit                 INTEGER DEFAULT -1,
    memory_limit              INTEGER DEFAULT -1,
    last_update_time          TIMESTAMP DEFAULT (now() AT TIME ZONE 'utc'),
    dast_scans                INTEGER DEFAULT -1,
    sast_scans                INTEGER DEFAULT -1,
    vcu_hard_limit            INTEGER,
    vcu_soft_limit            INTEGER,
    vcu_limit_total_block     BOOLEAN NOT NULL DEFAULT false,
    storage_hard_limit        INTEGER,
    storage_soft_limit        INTEGER,
    storage_limit_total_block BOOLEAN NOT NULL DEFAULT false
);
CREATE INDEX IF NOT EXISTS project_quota_project_idx
    ON centry.project_quota (project_id);

CREATE TABLE IF NOT EXISTS centry.statistic (
    id                       SERIAL PRIMARY KEY,
    project_id               INTEGER NOT NULL,
    start_time               TIMESTAMP DEFAULT (now() AT TIME ZONE 'utc'),
    vuh_used                 INTEGER DEFAULT 0,
    performance_test_runs    INTEGER DEFAULT 0,
    sast_scans               INTEGER DEFAULT 0,
    dast_scans               INTEGER DEFAULT 0,
    public_pool_workers      INTEGER DEFAULT 0,
    ui_performance_test_runs INTEGER DEFAULT 0,
    tasks_executions         INTEGER DEFAULT 0
);
CREATE INDEX IF NOT EXISTS statistic_project_idx
    ON centry.statistic (project_id);

-- Social users (author profiles)
CREATE TABLE IF NOT EXISTS centry.social_users (
    id SERIAL PRIMARY KEY,
    user_id INTEGER NOT NULL UNIQUE,
    avatar VARCHAR,
    title VARCHAR,
    description VARCHAR,
    personalization JSONB,
    default_context_management JSONB,
    default_summarization JSONB
);

-- Project secret vault (centry.secrets_key holds the per-project Fernet key,
-- itself encrypted with SECRETS_MASTER_KEY; centry.secrets_data holds the
-- Fernet-encrypted {secrets, hidden_secrets} JSON blob). Read and written by
-- internal/api/v2/secrets/handler.go and internal/infra/centrysecrets.
-- Column types are copied verbatim from the running legacy centry database —
-- both tables are `id text PRIMARY KEY, data bytea`. Without them every
-- secrets write answered 500 "relation centry.secrets_key does not exist".
CREATE TABLE IF NOT EXISTS centry.secrets_key (
    id TEXT PRIMARY KEY,
    data BYTEA
);

CREATE TABLE IF NOT EXISTS centry.secrets_data (
    id TEXT PRIMARY KEY,
    data BYTEA
);

-- User notifications. Same omission as centry.secrets_key above, found the same
-- way (#152): THREE Go call sites already query this table and none of them
-- could ever have worked against a database this file bootstrapped —
--   internal/api/v2/eliteacore/handler.go:393        (the list endpoint)
--   internal/db/sqlcgen/notification_events.sql.go  (HighWater + ListAfter,
--                                                    behind the notification SSE stream)
-- The list endpoint hid it: it runs the query under `if err == nil` and returns
-- 200 with an empty array on ANY failure, so a missing table is indistinguishable
-- from "no notifications". The SSE stream does not swallow, and answered 503 the
-- moment it was first mounted — which is what exposed this.
--
-- Column set is dictated by those queries (id, uuid, is_seen, project_id,
-- user_id, meta, event_type, created_at, updated_at); event_type values are the
-- legacy NotificationEventTypes enum
-- (legacy/plugins/elitea_core/models/enums/all.py), kept as TEXT rather than a
-- PG enum so a new legacy event type does not require a migration here.
CREATE TABLE IF NOT EXISTS centry.notifications (
    id SERIAL PRIMARY KEY,
    uuid UUID NOT NULL DEFAULT gen_random_uuid(),
    is_seen BOOLEAN NOT NULL DEFAULT FALSE,
    project_id INTEGER NOT NULL,
    user_id INTEGER NOT NULL,
    meta JSONB,
    event_type TEXT NOT NULL,
    created_at TIMESTAMP NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMP NOT NULL DEFAULT NOW()
);

-- Both readers are user-scoped and ordered by id: the list endpoint filters
-- (project_id, user_id), the SSE stream filters user_id and pages on id > cursor.
CREATE INDEX IF NOT EXISTS notifications_user_id_id_idx
    ON centry.notifications (user_id, id);

-- Audit trail (unit A14). READ-ONLY from this service: the write path belongs
-- to the legacy tracing plugin, whose SQLAlchemy model
-- (legacy/plugins/tracing/models/audit_event.py) this mirrors column for column
-- and index for index. A legacy-backed deployment already has the table and
-- these IF NOT EXISTS statements are no-ops; a Go-only deployment gets it here,
-- so `GET /elitea_core/audit*` reads an empty table rather than failing with
-- "relation does not exist".
CREATE TABLE IF NOT EXISTS centry.audit_events (
    id SERIAL PRIMARY KEY,
    timestamp TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    user_id INTEGER,
    user_email VARCHAR(256),
    project_id INTEGER,
    event_type VARCHAR(32) NOT NULL,
    action VARCHAR(512) NOT NULL,
    http_method VARCHAR(10),
    http_route VARCHAR(512),
    status_code SMALLINT,
    duration_ms DOUBLE PRECISION,
    is_error BOOLEAN NOT NULL DEFAULT FALSE,
    entity_type VARCHAR(32),
    entity_id INTEGER,
    entity_name VARCHAR(256),
    tool_name VARCHAR(256),
    model_name VARCHAR(256),
    input_tokens INTEGER,
    output_tokens INTEGER,
    llm_cost NUMERIC(18, 8),
    token_source VARCHAR(16),
    cost_source VARCHAR(64),
    trace_id VARCHAR(32),
    span_id VARCHAR(16),
    parent_span_id VARCHAR(16)
);

CREATE INDEX IF NOT EXISTS ix_audit_events_timestamp ON centry.audit_events (timestamp);
CREATE INDEX IF NOT EXISTS ix_audit_events_user_id ON centry.audit_events (user_id);
CREATE INDEX IF NOT EXISTS ix_audit_events_project_id ON centry.audit_events (project_id);
CREATE INDEX IF NOT EXISTS ix_audit_events_trace_id ON centry.audit_events (trace_id);
CREATE INDEX IF NOT EXISTS ix_audit_events_entity ON centry.audit_events (entity_type, entity_id);
CREATE INDEX IF NOT EXISTS ix_audit_events_model_name ON centry.audit_events (model_name);

-- Cron schedules (unit A14). Mirrors the legacy SQLAlchemy model column for
-- column (legacy/plugins/scheduling/models/schedule.py) — same table name, same
-- types, same nullability, same `active` default.
--
-- This is NOT a table this service invented. `services/elitea-scheduler`
-- ALREADY reads and updates it every minute
-- (internal/scheduler/scheduler.go: `SELECT ... FROM centry.schedule`,
-- `UPDATE centry.schedule SET last_run`), and until now no migration in this
-- repository created it: a Go-only deployment ran a scheduler whose every tick
-- failed with "relation does not exist". A legacy-backed deployment already has
-- the table from the plugin's own init_db and this IF NOT EXISTS is a no-op.
--
-- `rpc_func` names an INTERNAL platform RPC that the scheduler dispatches with
-- no principal and full privilege (see internal/api/v2/scheduling/schedules.go
-- for why the admin write path refuses to set it).
CREATE TABLE IF NOT EXISTS centry.schedule (
    id SERIAL PRIMARY KEY,
    name VARCHAR(64) NOT NULL,
    project_id INTEGER DEFAULT NULL,
    cron VARCHAR(64) NOT NULL,
    active BOOLEAN DEFAULT TRUE,
    rpc_func VARCHAR(64) NOT NULL,
    rpc_kwargs JSONB NOT NULL DEFAULT '{}'::jsonb,
    last_run TIMESTAMP
);

-- The scheduler's tick reads `WHERE active = true`; the admin listing reads the
-- whole table ordered by name.
CREATE INDEX IF NOT EXISTS ix_schedule_active ON centry.schedule (active);

-- App requests / moderation state (unit A14). Mirrors the legacy SQLAlchemy
-- model column for column (legacy/plugins/admin/models/moderation.py) — same
-- table name, same types, same nullability, same `pending` server default, same
-- four indexes.
--
-- The schema is `centry` and NOT a tenant schema, which looks wrong for a table
-- carrying `project_id` and is not: the legacy model declares
-- `{'schema': c.POSTGRES_SCHEMA}` (the shared schema, default `centry`) rather
-- than `POSTGRES_TENANT_SCHEMA`, and all four legacy endpoints open the session
-- with `db.with_project_schema_session(None)`. One shared table, filtered by
-- `project_id`. Reproduced rather than corrected because the admin listing reads
-- ACROSS projects by design, and moving the rows per tenant would make that
-- listing a fan-out over every `p_*` schema.
--
-- Unlike `centry.schedule` above, no deployment of this service has ever had
-- this table: nothing in Go creates it and nothing in Go read it — the two
-- endpoints that serve it answered from constants. A legacy-backed deployment
-- already has it from the admin plugin's own `create_tables`, and this
-- IF NOT EXISTS is a no-op there.
--
-- `status` is TEXT, not a PG enum, for the same reason `notifications.event_type`
-- is: the legacy column is `String(64)` with the vocabulary enforced in pydantic.
-- internal/api/v2/moderation/requests.go is where the three legal values live.
CREATE TABLE IF NOT EXISTS centry.moderation_state (
    id SERIAL PRIMARY KEY,
    user_id INTEGER NOT NULL,
    project_id INTEGER NOT NULL,
    issue_type VARCHAR(256) NOT NULL,
    entity_id VARCHAR,
    description TEXT NOT NULL,
    status VARCHAR(64) NOT NULL DEFAULT 'pending',
    rejection_comment TEXT,
    created_at TIMESTAMP NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMP NOT NULL DEFAULT NOW(),
    meta JSONB
);

-- The four `index=True` columns of the legacy model. The per-entity read filters
-- (project_id, entity_id, user_id) and the admin listing sorts on created_at.
CREATE INDEX IF NOT EXISTS ix_moderation_state_user_id ON centry.moderation_state (user_id);
CREATE INDEX IF NOT EXISTS ix_moderation_state_project_id ON centry.moderation_state (project_id);
CREATE INDEX IF NOT EXISTS ix_moderation_state_issue_type ON centry.moderation_state (issue_type);
CREATE INDEX IF NOT EXISTS ix_moderation_state_entity_id ON centry.moderation_state (entity_id);

-- =============================================================================
-- PLATFORM CONFIGURATION (unit A14, admin Configuration page)
-- =============================================================================
--
-- Global, admin-authored platform settings: one row per (section, key) of the
-- schema `internal/api/v2/admin/config_schemas.go` publishes.
--
-- This table has no counterpart in the legacy deployment, and that is the point.
-- pylon has no configuration table either: the admin Configuration page reads an
-- IN-PROCESS dict of pylon heartbeats (`admin.remote_runtimes`, 15s announce /
-- 60s freshness cut-off), and its save re-serialises a plugin's whole YAML and
-- fires `bootstrap_runtime_update` onto the Arbiter bus fire-and-forget, where a
-- bootstrap handler on the target pylon upserts it into that pylon's own
-- `plugin_config` table (legacy/plugins/admin/api/v2/plugin_config_values.py:275-305,
-- legacy/pylon/pylon/core/providers/internal/db_config.py:65-88).
--
-- That mechanism is Pylon plugin loading plus Arbiter transport, which AGENTS.md
-- names as things the target architecture does NOT preserve — and "Go owns
-- product data and migrations". So the settings this platform actually consumes
-- are owned here, in a table, rather than announced over a bus.
--
-- `value` is JSONB rather than TEXT because the schema's field types include
-- arrays and objects (a resource card's `links` is an array of {title,url}), and
-- because a typed read is what lets the write validate against the declared type
-- instead of round-tripping strings.
CREATE TABLE IF NOT EXISTS centry.platform_config (
    section VARCHAR(64) NOT NULL,
    key VARCHAR(128) NOT NULL,
    value JSONB NOT NULL,
    updated_at TIMESTAMP NOT NULL DEFAULT now(),
    updated_by VARCHAR(255),
    PRIMARY KEY (section, key)
);

-- =============================================================================
-- TENANT SCHEMA FUNCTION
-- Creates all per-project tables in a given schema (e.g. "p_1")
-- =============================================================================
CREATE OR REPLACE FUNCTION create_tenant_schema(schema_name TEXT) RETURNS void AS $$
BEGIN
    EXECUTE format('CREATE SCHEMA IF NOT EXISTS %I', schema_name);

    -- Tags
    EXECUTE format('
        CREATE TABLE IF NOT EXISTS %I.tags (
            id SERIAL PRIMARY KEY,
            name VARCHAR NOT NULL UNIQUE,
            data JSONB
        )', schema_name);

    -- Applications
    EXECUTE format('
        CREATE TABLE IF NOT EXISTS %I.applications (
            id SERIAL PRIMARY KEY,
            name VARCHAR(128) NOT NULL,
            description VARCHAR(2304),
            icon VARCHAR,
            owner_id INTEGER NOT NULL,
            created_at TIMESTAMP NOT NULL DEFAULT now(),
            -- WHEN THE AGENT LAST CHANGED. Declared here and, for a schema
            -- this function already built, by migrations/tenant/0134 — the
            -- pair every table in this file needs (the canvas tables are the
            -- same shape). The API has always carried an `updated_at` on
            -- every agent it lists; there was no column to fill it from, so
            -- the wire said "0001-01-01T00:00:00Z" for every agent ever
            -- created. The writers set it (repos/applications.go).
            updated_at TIMESTAMP NOT NULL DEFAULT now(),
            shared_owner_id INTEGER,
            shared_id INTEGER,
            uuid UUID UNIQUE DEFAULT gen_random_uuid(),
            webhook_secret VARCHAR,
            meta JSONB DEFAULT ''{}''::jsonb,
            CONSTRAINT application_shared_origin UNIQUE (shared_owner_id, shared_id)
        )', schema_name);

    -- Application versions
    EXECUTE format('
        CREATE TABLE IF NOT EXISTS %I.application_versions (
            id SERIAL PRIMARY KEY,
            application_id INTEGER NOT NULL REFERENCES %I.applications(id) ON DELETE CASCADE,
            name VARCHAR(128) NOT NULL,
            status VARCHAR NOT NULL DEFAULT ''draft'',
            author_id INTEGER NOT NULL,
            uuid UUID UNIQUE DEFAULT gen_random_uuid(),
            created_at TIMESTAMP NOT NULL DEFAULT now(),
            shared_owner_id INTEGER,
            shared_id INTEGER,
            llm_settings JSONB DEFAULT ''{}''::jsonb,
            instructions TEXT,
            conversation_starters JSONB DEFAULT ''[]''::jsonb,
            welcome_message TEXT DEFAULT '''',
            agent_type VARCHAR NOT NULL DEFAULT ''openai'',
            meta JSONB DEFAULT ''{}''::jsonb,
            pipeline_settings JSONB DEFAULT ''{}''::jsonb,
            CONSTRAINT application_version_shared_origin UNIQUE (shared_owner_id, shared_id),
            CONSTRAINT _application_version_name_uc UNIQUE (application_id, name)
        )', schema_name, schema_name);

    -- Application variables
    EXECUTE format('
        CREATE TABLE IF NOT EXISTS %I.application_variables (
            id SERIAL PRIMARY KEY,
            application_version_id INTEGER NOT NULL REFERENCES %I.application_versions(id) ON DELETE CASCADE,
            name VARCHAR NOT NULL,
            value VARCHAR,
            created_at TIMESTAMP NOT NULL DEFAULT now(),
            updated_at TIMESTAMP,
            CONSTRAINT _application_version_variable_name_uc UNIQUE (application_version_id, name)
        )', schema_name, schema_name);

    -- Application version <-> Tag association
    EXECUTE format('
        CREATE TABLE IF NOT EXISTS %I.application_version_tag_association (
            version_id INTEGER REFERENCES %I.application_versions(id) ON DELETE CASCADE,
            tag_id INTEGER REFERENCES %I.tags(id) ON DELETE CASCADE,
            PRIMARY KEY (version_id, tag_id)
        )', schema_name, schema_name, schema_name);

    -- Skills
    EXECUTE format('
        CREATE TABLE IF NOT EXISTS %I.skills (
            id SERIAL PRIMARY KEY,
            name VARCHAR(128) NOT NULL,
            description VARCHAR(2304) NOT NULL,
            owner_id INTEGER NOT NULL,
            author_id INTEGER NOT NULL,
            created_at TIMESTAMP NOT NULL DEFAULT now(),
            uuid UUID UNIQUE DEFAULT gen_random_uuid(),
            meta JSONB DEFAULT ''{}''::jsonb,
            shared_owner_id INTEGER,
            shared_id INTEGER
        )', schema_name);

    -- A published skill has exactly one twin in the public project, keyed by
    -- (source project, source skill). The partial unique index is what makes a
    -- concurrent first-publish race lose loudly instead of creating a second
    -- catalog entry for the same source skill (mirrors pylon's
    -- uq_skills_shared_owner, legacy/plugins/elitea_core/models/skill.py:39-42).
    EXECUTE format('
        CREATE UNIQUE INDEX IF NOT EXISTS uq_skills_shared_owner
            ON %I.skills (shared_owner_id, shared_id)
            WHERE shared_owner_id IS NOT NULL', schema_name);

    -- Skill versions
    EXECUTE format('
        CREATE TABLE IF NOT EXISTS %I.skill_versions (
            id SERIAL PRIMARY KEY,
            skill_id INTEGER NOT NULL REFERENCES %I.skills(id) ON DELETE CASCADE,
            name VARCHAR(128) NOT NULL DEFAULT ''base'',
            instructions TEXT NOT NULL,
            author_id INTEGER NOT NULL,
            created_at TIMESTAMP NOT NULL DEFAULT now(),
            uuid UUID UNIQUE DEFAULT gen_random_uuid(),
            meta JSONB DEFAULT ''{}''::jsonb,
            status VARCHAR NOT NULL DEFAULT ''draft'',
            CONSTRAINT _skill_version_name_uc UNIQUE (skill_id, name)
        )', schema_name, schema_name);

    EXECUTE format('
        CREATE INDEX IF NOT EXISTS ix_skill_versions_status
            ON %I.skill_versions (status)', schema_name);

    -- Skill version <-> Tag association
    EXECUTE format('
        CREATE TABLE IF NOT EXISTS %I.skill_version_tag_association (
            version_id INTEGER REFERENCES %I.skill_versions(id) ON DELETE CASCADE,
            tag_id INTEGER REFERENCES %I.tags(id) ON DELETE CASCADE,
            PRIMARY KEY (version_id, tag_id)
        )', schema_name, schema_name, schema_name);

    -- Entity <-> Skill mapping
    EXECUTE format('
        CREATE TABLE IF NOT EXISTS %I.entity_skill_mapping (
            id SERIAL PRIMARY KEY,
            entity_version_id INTEGER NOT NULL,
            entity_type VARCHAR(50) NOT NULL,
            skill_id INTEGER NOT NULL REFERENCES %I.skills(id) ON DELETE CASCADE,
            skill_version_id INTEGER REFERENCES %I.skill_versions(id),
            created_at TIMESTAMP NOT NULL DEFAULT now(),
            updated_at TIMESTAMP,
            CONSTRAINT _entity_skill_unique UNIQUE (entity_version_id, skill_id, entity_type)
        )', schema_name, schema_name, schema_name);

    -- EliteA Tools (toolkits)
    EXECUTE format('
        CREATE TABLE IF NOT EXISTS %I.elitea_tools (
            id SERIAL PRIMARY KEY,
            name VARCHAR(128) NOT NULL,
            type VARCHAR(64) NOT NULL,
            description TEXT,
            owner_id INTEGER NOT NULL,
            author_id INTEGER NOT NULL,
            created_at TIMESTAMP NOT NULL DEFAULT now(),
            uuid UUID UNIQUE DEFAULT gen_random_uuid(),
            meta JSONB DEFAULT ''{}''::jsonb,
            settings JSONB DEFAULT ''{}''::jsonb,
            env_vars JSONB DEFAULT ''{}''::jsonb
        )', schema_name);

    -- Entity <-> Tool mapping
    EXECUTE format('
        CREATE TABLE IF NOT EXISTS %I.entity_tool_mapping (
            id SERIAL PRIMARY KEY,
            entity_version_id INTEGER NOT NULL,
            entity_type VARCHAR(50) NOT NULL,
            tool_id INTEGER NOT NULL REFERENCES %I.elitea_tools(id) ON DELETE CASCADE,
            selected_tools JSONB DEFAULT ''[]''::jsonb,
            created_at TIMESTAMP NOT NULL DEFAULT now(),
            updated_at TIMESTAMP,
            CONSTRAINT _entity_tool_unique UNIQUE (entity_version_id, tool_id, entity_type)
        )', schema_name, schema_name);

    -- Chat conversation folders
    EXECUTE format('
        CREATE TABLE IF NOT EXISTS %I.chat_conversation_folders (
            id SERIAL PRIMARY KEY,
            uuid UUID UNIQUE DEFAULT gen_random_uuid(),
            name VARCHAR NOT NULL,
            owner_id INTEGER NOT NULL,
            position INTEGER NOT NULL DEFAULT 0,
            meta JSONB NOT NULL DEFAULT ''{}''::jsonb,
            created_at TIMESTAMP NOT NULL DEFAULT now(),
            updated_at TIMESTAMP
        )', schema_name);

    -- Chat conversations
    EXECUTE format('
        CREATE TABLE IF NOT EXISTS %I.chat_conversations (
            id SERIAL PRIMARY KEY,
            uuid UUID UNIQUE DEFAULT gen_random_uuid(),
            name VARCHAR NOT NULL,
            is_private BOOLEAN NOT NULL DEFAULT true,
            author_id INTEGER NOT NULL,
            meta JSONB NOT NULL DEFAULT ''{}''::jsonb,
            source VARCHAR(64) NOT NULL DEFAULT ''elitea'',
            instructions TEXT,
            attachment_participant_id INTEGER,
            folder_id INTEGER REFERENCES %I.chat_conversation_folders(id),
            created_at TIMESTAMP NOT NULL DEFAULT now(),
            updated_at TIMESTAMP
        )', schema_name, schema_name);

    -- Chat participants
    EXECUTE format('
        CREATE TABLE IF NOT EXISTS %I.chat_participants (
            id SERIAL PRIMARY KEY,
            uuid UUID UNIQUE DEFAULT gen_random_uuid(),
            entity_name VARCHAR NOT NULL,
            entity_meta JSONB NOT NULL DEFAULT ''{}''::jsonb,
            meta JSONB NOT NULL DEFAULT ''{}''::jsonb
        )', schema_name);

    -- Chat participant <-> conversation mapping
    EXECUTE format('
        CREATE TABLE IF NOT EXISTS %I.chat_participant_mapping (
            id SERIAL PRIMARY KEY,
            conversation_id INTEGER REFERENCES %I.chat_conversations(id) ON DELETE CASCADE,
            participant_id INTEGER REFERENCES %I.chat_participants(id) ON DELETE CASCADE,
            entity_settings JSONB NOT NULL DEFAULT ''{}''::jsonb,
            created_at TIMESTAMP NOT NULL DEFAULT now(),
            updated_at TIMESTAMP,
            CONSTRAINT _participant_conversation_uc UNIQUE (participant_id, conversation_id)
        )', schema_name, schema_name, schema_name);

    -- Chat message groups
    EXECUTE format('
        CREATE TABLE IF NOT EXISTS %I.chat_message_group (
            id SERIAL PRIMARY KEY,
            uuid UUID UNIQUE DEFAULT gen_random_uuid(),
            author_participant_id INTEGER REFERENCES %I.chat_participants(id),
            conversation_id INTEGER REFERENCES %I.chat_conversations(id) ON DELETE CASCADE,
            sent_to_id INTEGER REFERENCES %I.chat_participants(id),
            reply_to_id INTEGER REFERENCES %I.chat_message_group(id) ON DELETE SET NULL,
            meta JSONB NOT NULL DEFAULT ''{}''::jsonb,
            is_streaming BOOLEAN NOT NULL DEFAULT false,
            task_id VARCHAR(64),
            created_at TIMESTAMP NOT NULL DEFAULT now(),
            updated_at TIMESTAMP
        )', schema_name, schema_name, schema_name, schema_name, schema_name);

    -- Chat message items
    EXECUTE format('
        CREATE TABLE IF NOT EXISTS %I.chat_message_items (
            id SERIAL PRIMARY KEY,
            uuid UUID UNIQUE DEFAULT gen_random_uuid(),
            item_type VARCHAR(50) NOT NULL,
            order_index INTEGER NOT NULL,
            meta JSONB NOT NULL DEFAULT ''{}''::jsonb,
            created_at TIMESTAMP NOT NULL DEFAULT now(),
            updated_at TIMESTAMP,
            message_group_id INTEGER REFERENCES %I.chat_message_group(id) ON DELETE CASCADE
        )', schema_name, schema_name);

    -- The 1:1 payload tables a message item's `item_type` discriminator selects,
    -- and the execution trace hanging off a group.
    --
    -- WHY THEY ARE HERE AS WELL AS IN THE TENANT HISTORY. This file builds the
    -- ONLY tenant schema a first install has. `migrations/tenant/0123` and
    -- `0127` own the same four tables, but the tenant history does not
    -- necessarily run: the journeys E2E stack applies this file with psql and
    -- then invokes `/elitea-migrate` with NO FLAGS
    -- (apps/elitea-web/scripts/e2e-stack.sh), which is Bootstrap plus
    -- ApplyShared and nothing else, and deploy/docker-compose.e2e-standalone.yml
    -- has no migrate service to make up the difference. So p_1 there was exactly
    -- what the block above builds — a message graph whose leaves did not exist.
    --
    -- The consequence was not a missing feature. GET
    -- /elitea_core/messages/prompt_lib/{project}/{conversation} joins
    -- chat_messages_text for every group's content and then joins
    -- chat_messages_attachment for its files, and BOTH propagate rather than
    -- answering an empty transcript (#599), so the route answered
    -- `relation "p_1.chat_messages_text" does not exist` (42P01) -> 500 for
    -- EVERY conversation on that stack. The writer failed identically:
    -- predict.go's `INSERT INTO chat_messages_text` had nowhere to land.
    --
    -- SAFE IN BOTH DIRECTIONS, because every statement is IF NOT EXISTS and the
    -- shapes are identical to 0123/0127. Where the tenant history runs first
    -- these are no-ops; where it runs after this file they are, and it still
    -- records itself in the ledger. Neither ordering produces 42P07, and neither
    -- produces two deployments carrying different columns for one table — a
    -- Postgres integration test compares the two definitions column for column
    -- rather than trusting that claim
    -- (repos/conversation_messages_fresh_install_postgres_integration_test.go).
    --
    -- The shapes come from those migrations, which take them in turn from the
    -- pg_catalog dump of the live legacy database (schema p_1) rather than from
    -- pylon's SQLAlchemy models. Two details that read as oversights are
    -- deliberate and are copied rather than improved: chat_messages_attachment's
    -- `content` is `json` and NOT `jsonb`, because a deployment running both
    -- pylon and this service must read back the bytes that were written, and
    -- `jsonb` normalises whitespace, reorders keys and drops duplicates; and
    -- chat_messages_context lists `id` LAST, which is the deployed column order.

    -- One row per text message item. Sharing chat_message_items' primary key
    -- rather than carrying its own is what makes the item's type discriminator
    -- and its payload impossible to disagree.
    EXECUTE format('
        CREATE TABLE IF NOT EXISTS %I.chat_messages_text (
            id INTEGER PRIMARY KEY REFERENCES %I.chat_message_items(id) ON DELETE CASCADE,
            content TEXT NOT NULL
        )', schema_name, schema_name);

    -- Same 1:1 shape for context items (support-assistant context and friends).
    -- context_type is nullable: the adhoc resolver treats a NULL type as "not a
    -- support_assistant_context" and refuses the turn on it, so the column has
    -- to be able to hold that state rather than defaulting to something
    -- plausible.
    EXECUTE format('
        CREATE TABLE IF NOT EXISTS %I.chat_messages_context (
            context_data JSONB NOT NULL,
            context_type TEXT,
            id INTEGER PRIMARY KEY REFERENCES %I.chat_message_items(id) ON DELETE CASCADE
        )', schema_name, schema_name);

    -- One row per uploaded chat attachment (#606). The discriminator that
    -- selects it is the literal `attachment_message`, not `attachment`.
    EXECUTE format('
        CREATE TABLE IF NOT EXISTS %I.chat_messages_attachment (
            id INTEGER PRIMARY KEY REFERENCES %I.chat_message_items(id) ON DELETE CASCADE,
            name VARCHAR(256) NOT NULL,
            bucket VARCHAR(256) NOT NULL,
            attachment_type VARCHAR(256) NOT NULL,
            content JSON
        )', schema_name, schema_name);

    -- Execution-step drill-down (#277). Written by repos/agent_trace.go as the
    -- worker streams node events, read by the message-traces API, and deleted
    -- wholesale per message group on regeneration. BIGSERIAL rather than the
    -- bare BIGINT the sqlc projection records: agent_trace.go inserts a step
    -- WITHOUT supplying an id, so the column has to carry a default.
    EXECUTE format('
        CREATE TABLE IF NOT EXISTS %I.chat_message_trace_step (
            id BIGSERIAL PRIMARY KEY,
            message_group_id INTEGER NOT NULL REFERENCES %I.chat_message_group(id) ON DELETE CASCADE,
            kind TEXT NOT NULL,
            run_id TEXT,
            parent_agent_name TEXT,
            parent_agent_call_id TEXT,
            started_at TIMESTAMPTZ,
            finished_at TIMESTAMPTZ,
            is_error BOOLEAN NOT NULL DEFAULT false,
            has_visible_content BOOLEAN NOT NULL DEFAULT true,
            tool_name TEXT,
            tool_inputs JSONB,
            tool_output TEXT,
            finish_reason TEXT,
            step_type TEXT,
            text TEXT,
            thinking TEXT,
            model_name TEXT,
            attrs JSONB
        )', schema_name, schema_name);

    -- The canvas branch of the same graph, and the same bargain again — see
    -- migrations/tenant/0129, which owns these three and carries the full
    -- reasoning. CreateCanvas INSERTs into chat_messages_canvas and
    -- chat_canvas_versions, and it is reached by a REGISTERED production route
    -- (POST /elitea_core/canvases/prompt_lib/{projectID}, behind
    -- `models.chat.canvas.create`, which shared/0068 seeds), so on this stack it
    -- answered 42P01 for every caller that cleared the permission gate.
    -- GetMessageByUUID LEFT JOINs chat_messages_canvas for every item of every
    -- message, canvas or not; that read is latent rather than live only because
    -- its handler is not currently registered by any router, and owning the
    -- table is what keeps wiring it later from being a regression.
    --
    -- chat_messages_canvas lists `id` LAST for the same reason
    -- chat_messages_context does: it is the deployed column order.
    -- `canvas_type` carries no length limit because the deployed column carries
    -- none, and a shorter column here would reject a value pylon accepts in a
    -- database they share. chat_canvas_version_authors has no Go reader or
    -- writer yet — it is the source of the `editors` list CreateCanvas already
    -- returns empty — and is created anyway, because 0123's header records what
    -- happened the last time this graph was owned only as far as the error
    -- message named.
    EXECUTE format('
        CREATE TABLE IF NOT EXISTS %I.chat_messages_canvas (
            name TEXT NOT NULL,
            canvas_type VARCHAR NOT NULL,
            id INTEGER PRIMARY KEY REFERENCES %I.chat_message_items(id) ON DELETE CASCADE
        )', schema_name, schema_name);

    EXECUTE format('
        CREATE TABLE IF NOT EXISTS %I.chat_canvas_versions (
            id SERIAL PRIMARY KEY,
            code_language VARCHAR(32),
            canvas_content TEXT NOT NULL,
            canvas_item_id INTEGER NOT NULL REFERENCES %I.chat_messages_canvas(id) ON DELETE CASCADE,
            created_at TIMESTAMP NOT NULL DEFAULT now()
        )', schema_name, schema_name);

    EXECUTE format('
        CREATE TABLE IF NOT EXISTS %I.chat_canvas_version_authors (
            id SERIAL PRIMARY KEY,
            participant_id INTEGER NOT NULL REFERENCES %I.chat_participants(id) ON DELETE CASCADE,
            canvas_version_id INTEGER NOT NULL REFERENCES %I.chat_canvas_versions(id) ON DELETE CASCADE,
            CONSTRAINT _participant_id_canvas_version_id_uc UNIQUE (participant_id, canvas_version_id)
        )', schema_name, schema_name, schema_name);

    -- PostgreSQL does not index a referencing column for you, and every access
    -- path in the chat query set is by parent: the trace read, its FOR UPDATE
    -- lock and its regeneration DELETE go by message group;
    -- InsertCurrentAgentTextItem runs a count(*) over chat_message_items'
    -- message_group_id to pick the next order_index on every streamed text item;
    -- and groups are always read by conversation. Without these each degrades to
    -- a sequential scan over the whole project history. Same names as 0123,
    -- so whichever definition runs second finds them present.
    EXECUTE format('
        CREATE INDEX IF NOT EXISTS chat_message_trace_step_message_group_id_idx
            ON %I.chat_message_trace_step (message_group_id, id)', schema_name);
    EXECUTE format('
        CREATE INDEX IF NOT EXISTS chat_message_items_message_group_id_idx
            ON %I.chat_message_items (message_group_id, order_index)', schema_name);
    EXECUTE format('
        CREATE INDEX IF NOT EXISTS chat_message_group_conversation_id_idx
            ON %I.chat_message_group (conversation_id, id)', schema_name);
    -- Keeps the deployed name, `ix_tenant_...`, rather than one of our own: an
    -- index we named differently would be a SECOND index on the same column in
    -- every deployment where pylon already built the first.
    EXECUTE format('
        CREATE INDEX IF NOT EXISTS ix_tenant_chat_canvas_versions_created_at
            ON %I.chat_canvas_versions (created_at)', schema_name);

    -- Selected conversations (per-user pinned)
    EXECUTE format('
        CREATE TABLE IF NOT EXISTS %I.chat_selected_conversations (
            id SERIAL PRIMARY KEY,
            user_id INTEGER NOT NULL,
            conversation_id INTEGER REFERENCES %I.chat_conversations(id) ON DELETE CASCADE
        )', schema_name, schema_name);

    -- Configuration (integrations/credentials)
    EXECUTE format('
        CREATE TABLE IF NOT EXISTS %I.configuration (
            id SERIAL PRIMARY KEY,
            uuid UUID UNIQUE DEFAULT gen_random_uuid(),
            project_id INTEGER NOT NULL,
            label VARCHAR,
            elitea_title VARCHAR NOT NULL UNIQUE,
            type VARCHAR NOT NULL,
            section VARCHAR NOT NULL,
            data JSONB NOT NULL DEFAULT ''{}''::jsonb,
            meta JSONB NOT NULL DEFAULT ''{}''::jsonb,
            shared BOOLEAN NOT NULL DEFAULT false,
            status_ok BOOLEAN NOT NULL DEFAULT false,
            status_logs TEXT,
            source VARCHAR NOT NULL DEFAULT ''user'',
            author_id INTEGER,
            created_at TIMESTAMP NOT NULL DEFAULT now(),
            updated_at TIMESTAMP
        )', schema_name);

    -- Likes
    EXECUTE format('
        CREATE TABLE IF NOT EXISTS %I.social_likes (
            id SERIAL PRIMARY KEY,
            entity_name VARCHAR NOT NULL,
            entity_id INTEGER NOT NULL,
            user_id INTEGER NOT NULL,
            created_at TIMESTAMP NOT NULL DEFAULT now(),
            CONSTRAINT _like_unique UNIQUE (entity_name, entity_id, user_id)
        )', schema_name);

    -- Pins
    EXECUTE format('
        CREATE TABLE IF NOT EXISTS %I.social_pins (
            id SERIAL PRIMARY KEY,
            entity_name VARCHAR NOT NULL,
            entity_id INTEGER NOT NULL,
            user_id INTEGER NOT NULL,
            created_at TIMESTAMP NOT NULL DEFAULT now(),
            CONSTRAINT _pin_unique UNIQUE (entity_name, entity_id, user_id)
        )', schema_name);

    -- Feedbacks
    EXECUTE format('
        CREATE TABLE IF NOT EXISTS %I.social_feedbacks (
            id SERIAL PRIMARY KEY,
            entity_name VARCHAR NOT NULL,
            entity_id INTEGER NOT NULL,
            user_id INTEGER NOT NULL,
            rating INTEGER,
            comment TEXT,
            meta JSONB DEFAULT ''{}''::jsonb,
            created_at TIMESTAMP NOT NULL DEFAULT now()
        )', schema_name);

    -- Per-message like/dislike feedback with an optional comment (#880).
    -- Not `social_feedbacks` above: that table has no unique constraint on
    -- (entity_name, entity_id, user_id), a 1-5 `rating` shape rather than a
    -- like/dislike one, and an `entity_id INTEGER` that cannot hold a
    -- message's real identifier (chat_message_group.uuid). See
    -- tenant/0135_chat_message_feedback.sql for the full account — this
    -- block is the SAME shape, kept here because the journeys E2E stack
    -- applies this file with psql and then runs `/elitea-migrate` with no
    -- flags (Bootstrap plus ApplyShared, never the tenant history), the same
    -- reason 0129''s chat_canvas_* tables are declared twice.
    EXECUTE format('
        CREATE TABLE IF NOT EXISTS %I.chat_message_feedback (
            id BIGSERIAL PRIMARY KEY,
            message_group_uuid UUID NOT NULL REFERENCES %I.chat_message_group(uuid) ON DELETE CASCADE,
            user_id INTEGER NOT NULL,
            rating SMALLINT NOT NULL,
            comment TEXT,
            created_at TIMESTAMP NOT NULL DEFAULT now(),
            updated_at TIMESTAMP NOT NULL DEFAULT now(),
            CONSTRAINT chat_message_feedback_message_user_uc UNIQUE (message_group_uuid, user_id),
            CONSTRAINT chat_message_feedback_rating_check CHECK (rating IN (-1, 1))
        )', schema_name, schema_name);
    EXECUTE format('
        CREATE INDEX IF NOT EXISTS ix_tenant_chat_message_feedback_message_group_uuid
            ON %I.chat_message_feedback (message_group_uuid)', schema_name);

    -- Toolkit index metadata (the "Indexes" tab, issue #149).
    -- Columns match exactly what `internal/api/v2/toolkits/handler.go`'s
    -- `IndexMeta`/`IndexMetaGet` SELECT: id, name, status, progress,
    -- created_at, plus the `toolkit_id` those queries filter on.
    -- Missing here until now, which is why `IndexMeta` was silently taking its
    -- error branch on this stack — see that handler's own comment.
    EXECUTE format('
        CREATE TABLE IF NOT EXISTS %I.index_meta (
            id SERIAL PRIMARY KEY,
            toolkit_id INTEGER NOT NULL REFERENCES %I.elitea_tools(id) ON DELETE CASCADE,
            name VARCHAR(128) NOT NULL,
            status VARCHAR(64) NOT NULL DEFAULT ''created'',
            progress DOUBLE PRECISION NOT NULL DEFAULT 0,
            meta JSONB DEFAULT ''{}''::jsonb,
            created_at TIMESTAMP NOT NULL DEFAULT now()
        )', schema_name, schema_name);

END;
$$ LANGUAGE plpgsql;

-- =============================================================================
-- AUTH TABLES (pylon_auth owns these; we create them for standalone dev mode)
-- In production, pylon_auth creates and manages these tables.
-- =============================================================================
CREATE TABLE IF NOT EXISTS auth_core__user (
    id SERIAL PRIMARY KEY,
    email TEXT UNIQUE,
    name TEXT,
    last_login TIMESTAMP,
    suspended BOOLEAN NOT NULL DEFAULT false
);
CREATE INDEX IF NOT EXISTS idx_auth_core_user_email ON auth_core__user(email);

CREATE TABLE IF NOT EXISTS auth_core__user_provider (
    user_id INTEGER REFERENCES auth_core__user(id) ON DELETE CASCADE,
    provider_ref TEXT UNIQUE,
    PRIMARY KEY (user_id, provider_ref)
);

CREATE TABLE IF NOT EXISTS auth_core__role (
    id SERIAL PRIMARY KEY,
    name VARCHAR(64) NOT NULL,
    mode VARCHAR(64) NOT NULL,
    UNIQUE (name, mode)
);

CREATE TABLE IF NOT EXISTS auth_core__role_permission (
    id SERIAL PRIMARY KEY,
    role_id INTEGER NOT NULL REFERENCES auth_core__role(id) ON DELETE CASCADE,
    permission VARCHAR(64),
    UNIQUE (role_id, permission)
);

CREATE TABLE IF NOT EXISTS auth_core__user_role (
    id SERIAL PRIMARY KEY,
    user_id INTEGER NOT NULL REFERENCES auth_core__user(id) ON DELETE CASCADE,
    role_id INTEGER NOT NULL REFERENCES auth_core__role(id) ON DELETE CASCADE,
    UNIQUE (user_id, role_id)
);

CREATE TABLE IF NOT EXISTS auth_core__scope (
    id SERIAL PRIMARY KEY,
    parent_id INTEGER REFERENCES auth_core__scope(id) ON DELETE SET NULL,
    name TEXT
);

CREATE TABLE IF NOT EXISTS auth_core__user_permission (
    user_id INTEGER REFERENCES auth_core__user(id) ON DELETE CASCADE,
    scope_id INTEGER REFERENCES auth_core__scope(id) ON DELETE CASCADE,
    permission TEXT,
    PRIMARY KEY (user_id, scope_id, permission)
);

CREATE TABLE IF NOT EXISTS auth_core__token (
    id SERIAL PRIMARY KEY,
    uuid VARCHAR(36) UNIQUE,
    expires TIMESTAMP,
    user_id INTEGER REFERENCES auth_core__user(id) ON DELETE CASCADE,
    name TEXT
);

CREATE TABLE IF NOT EXISTS auth_core__project_role (
    id SERIAL PRIMARY KEY,
    project_id INTEGER NOT NULL,
    name TEXT NOT NULL,
    UNIQUE (project_id, name)
);

CREATE TABLE IF NOT EXISTS auth_core__project_role_permission (
    id SERIAL PRIMARY KEY,
    project_id INTEGER NOT NULL,
    role_id INTEGER REFERENCES auth_core__project_role(id) ON DELETE CASCADE,
    permission TEXT NOT NULL,
    UNIQUE (project_id, role_id, permission)
);

CREATE TABLE IF NOT EXISTS auth_core__project_user_role (
    id SERIAL PRIMARY KEY,
    project_id INTEGER NOT NULL,
    user_id INTEGER NOT NULL REFERENCES auth_core__user(id) ON DELETE CASCADE,
    role_id INTEGER NOT NULL REFERENCES auth_core__project_role(id) ON DELETE CASCADE,
    UNIQUE (project_id, user_id, role_id)
);

-- =============================================================================
-- SEED DATA
-- =============================================================================

-- Default auth scope
INSERT INTO auth_core__scope (id, name, parent_id)
VALUES (1, 'Global', NULL)
ON CONFLICT (id) DO NOTHING;

-- Default roles
INSERT INTO auth_core__role (id, name, mode) VALUES
    (1, 'admin', 'default'),
    (2, 'editor', 'default'),
    (3, 'viewer', 'default')
ON CONFLICT (id) DO NOTHING;

-- Administration-mode roles.
--
-- These used to be absent, and that absence was load-bearing in the wrong
-- direction. `legacyrbac.PostgresResolver` resolves the `administration` and
-- `developer` modes from auth_core__user_role/auth_core__role/…role_permission
-- with the project id ignored, so with no administration-mode row anywhere
-- EVERY central permission resolved to the empty set for EVERY user. Unit A14
-- could therefore only gate the admin panel's WRITES: gating a READ would have
-- turned "the admin panel works" into "403 for everyone" on a fresh database.
-- The reads are gated now (internal/api/router.go), so the roles have to exist.
--
-- pylon's equivalent is auth_core/db/migrations/202202021633_core.py plus
-- 202604161400_add_super_admin_role.py. `system` is omitted: it is not in the
-- Go product's role vocabulary (users.go `adminRolePriority` is
-- super_admin/admin/editor/viewer) and nothing assigns it.
INSERT INTO auth_core__role (id, name, mode) VALUES
    (4, 'super_admin', 'administration'),
    (5, 'admin', 'administration'),
    (6, 'editor', 'administration'),
    (7, 'viewer', 'administration')
ON CONFLICT (id) DO NOTHING;

-- Administration-mode grants, transcribed from the `recommended_roles` each
-- pylon handler declares in legacy/plugins/admin/api/v2/.
--
-- Two things about that transcription are easy to get wrong. First, a BARE list
-- (`check_api(["runtime.plugins"])`) is not "no recommended roles" — it parses
-- into the RecommendedRoles defaults, which are system/super_admin/admin True.
-- Second, a dict that names only `{"admin": True, "viewer": False,
-- "editor": False}` still leaves `super_admin` at its default True. So
-- super_admin holds everything and admin holds everything except the
-- super_admin escalation permission, which auth_users.py checks separately
-- (admin/module.py registers it with admin explicitly False).
--
-- `editor` and `viewer` get nothing: every declaration sets them False.
--
-- Only the `administration` mode is seeded. pylon also registers these in
-- `developer` and `default`, but no Go route resolves an admin-panel permission
-- in either, and granting `admin.auth.users` to the DEFAULT-mode `admin` role
-- would leak it into project-scoped resolution — projectPermissions() falls
-- back to central default-mode grants by role name when a project has no
-- per-project rows.
INSERT INTO auth_core__role_permission (role_id, permission)
SELECT role.id, grant_row.permission
FROM auth_core__role AS role
JOIN (VALUES
    ('super_admin', 'admin.auth.users'),
    ('super_admin', 'admin.auth.users.super_admin'),
    ('super_admin', 'runtime.plugins'),
    ('super_admin', 'projects.projects.projects.view'),
    ('super_admin', 'configuration.roles.permissions.view'),
    ('super_admin', 'admin.moderation'),
    -- Unit A14, Service Descriptors. `elitea_core/api/v2/admin.py` declares a
    -- BARE `check_api(["runtime.airun.serviceproviders"])` — which, per the note
    -- above, is system/super_admin/admin True — and
    -- `elitea_core/api/v2/register_descriptor.py` declares
    -- `provider_hub.descriptor.register` with `{"admin": True, "viewer": False,
    -- "editor": False}` in administration mode, leaving super_admin at its
    -- default True.
    --
    -- The routes they gate answer 501: this platform has no provider hub. The
    -- grants exist anyway because the gate runs BEFORE the refusal, and without
    -- them every administrator would get a bare 403 instead of the sentence
    -- explaining why the page is empty — which is the whole point of the page.
    ('super_admin', 'runtime.airun.serviceproviders'),
    ('super_admin', 'provider_hub.descriptor.register'),
    -- Issue 255, admin & tenancy parity. `modes.py`, `user_invite.py` and
    -- `admin_published_agents.py` all declare BARE lists — system/super_admin/
    -- admin True per the note above — and `user_project_permissions.py`
    -- declares `{"admin": True, "viewer": False, "editor": False}` in
    -- administration mode, which leaves super_admin at its default True. So all
    -- four land on both roles.
    --
    -- These grants are not optional decoration: the routes are gated centrally,
    -- and a permission nobody holds is 403-for-everyone, which is the outcome
    -- this whole block exists to prevent.
    ('super_admin', 'modes.users'),
    ('super_admin', 'configuration.roles.user_project_permissions.view'),
    ('super_admin', 'configuration.roles.user_project_permissions.edit'),
    ('super_admin', 'runtime.admin.published_agents'),
    ('admin', 'admin.auth.users'),
    ('admin', 'runtime.plugins'),
    ('admin', 'projects.projects.projects.view'),
    ('admin', 'configuration.roles.permissions.view'),
    ('admin', 'admin.moderation'),
    ('admin', 'runtime.airun.serviceproviders'),
    ('admin', 'provider_hub.descriptor.register'),
    ('admin', 'modes.users'),
    ('admin', 'configuration.roles.user_project_permissions.view'),
    ('admin', 'configuration.roles.user_project_permissions.edit'),
    ('admin', 'runtime.admin.published_agents'),
    -- Issue 246, budgets. project_budget.py, project_budgets.py,
    -- user_budget.py and user_budgets.py declare `{"admin": True,
    -- "editor": False, "viewer": False}` in administration mode, which leaves
    -- super_admin at its default True per the note above.
    ('super_admin', 'models.admin.project_budgets.view'),
    ('super_admin', 'models.admin.project_budgets.edit'),
    ('admin', 'models.admin.project_budgets.view'),
    ('admin', 'models.admin.project_budgets.edit'),
    -- Issue 333, project provisioning. `project.py`'s `AdminAPI.post` and
    -- `AdminAPI.delete` declare `{"admin": True, "viewer": False,
    -- "editor": False}` in administration mode, which leaves super_admin at
    -- its default True per the note above. The exported matrix agrees: both
    -- names sit on administration/{admin, super_admin, system}, and `system`
    -- is omitted here as everywhere else in this file.
    --
    -- Deliberately NOT granted in the default mode, even though the matrix
    -- lists both names there for super_admin and system. Go seeds neither role
    -- in that mode, and a default-mode grant to `admin` would reach every
    -- project admin through projectPermissions()' central fallback. The routes
    -- resolve in `administration` mode, so this is the only mode that reaches
    -- them. The same grants land on existing databases through
    -- migrations/shared/0069_project_create_permissions.sql.
    ('super_admin', 'projects.projects.project.create'),
    ('super_admin', 'projects.projects.project.delete'),
    ('admin', 'projects.projects.project.create'),
    ('admin', 'projects.projects.project.delete')
) AS grant_row(role_name, permission) ON grant_row.role_name = role.name
WHERE role.mode = 'administration'
ON CONFLICT (role_id, permission) DO NOTHING;

-- Default-mode grants (issues 246 and 253).
--
-- The only ones in this file, and the exception the block above describes
-- rather than a contradiction of it: `models.project_context.view` IS a
-- project-scoped permission, declared `{"admin": True, "editor": True,
-- "viewer": True}` in DEFAULT_MODE by project_budget.py, user_budget.py,
-- user_budgets.py and usage.py. Granting it to the default-mode roles is what
-- lets projectPermissions()' central fallback resolve it for a project with no
-- per-project rows — which is every project on a fresh database. Without it
-- the project-scoped budget and usage reads are 403 for every user.
--
-- It also makes a project member's permission set NON-EMPTY for the first time
-- on a fresh database, which two things outside the budgets surface react to.
-- migrations/shared/0062_budgets_quota_statistics.sql documents both, and
-- internal/infra/legacyrbac/default_mode_grant_postgres_integration_test.go
-- pins the whole blast radius.
INSERT INTO auth_core__role_permission (role_id, permission)
SELECT role.id, 'models.project_context.view'
FROM auth_core__role AS role
WHERE role.mode = 'default' AND role.name IN ('admin', 'editor', 'viewer')
ON CONFLICT (role_id, permission) DO NOTHING;

-- Issue 253: the cost breakdown and the two chat trace reads. Same DEFAULT_MODE
-- `{"admin": True, "editor": True, "viewer": True}` declaration, transcribed
-- from analytics_costs.py, message_traces.py and message_trace.py; the same DDL
-- lands on existing databases through
-- migrations/shared/0063_trace_and_cost_read_permissions.sql, which carries the
-- blast-radius note. None of the three strings is read anywhere else in this
-- service, so the surface they open is exactly those three routes.
INSERT INTO auth_core__role_permission (role_id, permission)
SELECT role.id, grant_row.permission
FROM auth_core__role AS role
CROSS JOIN (VALUES
    ('models.monitoring.tracing.view'),
    ('models.chat.messages.list'),
    ('models.chat.messages.details')
) AS grant_row(permission)
WHERE role.mode = 'default' AND role.name IN ('admin', 'editor', 'viewer')
ON CONFLICT (role_id, permission) DO NOTHING;

-- Default dev user
INSERT INTO auth_core__user (id, email, name)
VALUES (1, 'dev@elitea.ai', 'Dev User')
ON CONFLICT (id) DO NOTHING;

-- Give dev user admin role
INSERT INTO auth_core__user_role (user_id, role_id)
VALUES (1, 1)
ON CONFLICT (user_id, role_id) DO NOTHING;

-- …and the administration-mode `admin` role, so a fresh database has one
-- account that can actually reach the admin panel. Without this the roles above
-- exist and nobody holds them, which is the same 403-for-everyone outcome by a
-- different route.
--
-- `admin`, deliberately, not `super_admin`. Least privilege is the first
-- reason: a bootstrap seed should not hand out the escalation permission
-- unprompted, and `admin` holds every permission this file grants except
-- `admin.auth.users.super_admin`. The second reason is concrete —
-- eliteacore/handler.go's project-member listing UNIONs project members with
-- every holder of a role NAMED `super_admin`, and it does not filter on
-- `mode`, so a global super_admin appears as a member of every project on
-- every page that reads it. Whether that union should be mode-filtered is a
-- separate question; seeding `admin` means this file does not force the answer.
INSERT INTO auth_core__user_role (user_id, role_id)
SELECT 1, role.id
FROM auth_core__role AS role
WHERE role.name = 'admin' AND role.mode = 'administration'
ON CONFLICT (user_id, role_id) DO NOTHING;

-- Default project
INSERT INTO centry.project (id, name, owner_id, create_success)
VALUES (1, 'Default Project', 1, true)
ON CONFLICT (id) DO NOTHING;

-- Default project group
INSERT INTO centry.project_group (id, name)
VALUES (1, 'Default')
ON CONFLICT (id) DO NOTHING;

INSERT INTO centry.project_group_association (project_id, group_id)
VALUES (1, 1)
ON CONFLICT DO NOTHING;

-- Default social user (dev user)
INSERT INTO centry.social_users (id, user_id, title, description)
VALUES (1, 1, 'Dev User', '')
ON CONFLICT (id) DO NOTHING;

-- Create tenant schema for default project
SELECT create_tenant_schema('p_1');

-- Reset sequences
--
-- Every INSERT above supplies an explicit id, which does NOT advance the
-- table's SERIAL sequence — so the next id-less INSERT collides on the primary
-- key. The three centry sequences were reset here from the start; the three
-- auth_core ones were not, and auth_core__user is the one that bit.
--
-- Symptom (issue #154, first CI run of the E2E job on a fresh database): the
-- E2E seed's member-persona `INSERT INTO auth_core__user (email, name)` drew
-- nextval = 1, collided with the dev user seeded at id 1 here, and failed. The
-- admin persona then drew 2 and succeeded, so the stack came up with exactly
-- one of the two personas — `project_user_role rows for the two personas: 1
-- (want 2)`. Running the seed a SECOND time appeared to fix it, because by then
-- the sequence had been dragged past the collision; that is why the failure
-- stayed invisible on a long-lived developer database and only ever showed up
-- on a database created from scratch.
SELECT setval('centry.project_id_seq', (SELECT COALESCE(MAX(id), 0) FROM centry.project) + 1, false);
SELECT setval('centry.project_group_id_seq', (SELECT COALESCE(MAX(id), 0) FROM centry.project_group) + 1, false);
SELECT setval('centry.social_users_id_seq', (SELECT COALESCE(MAX(id), 0) FROM centry.social_users) + 1, false);
SELECT setval('auth_core__user_id_seq', (SELECT COALESCE(MAX(id), 0) FROM auth_core__user) + 1, false);
SELECT setval('auth_core__role_id_seq', (SELECT COALESCE(MAX(id), 0) FROM auth_core__role) + 1, false);
SELECT setval('auth_core__scope_id_seq', (SELECT COALESCE(MAX(id), 0) FROM auth_core__scope) + 1, false);

COMMIT;
