-- SQLC compiler projection for the current EliteA tenant chat tables used by
-- the first direct agent-execution slice.
--
-- This file is NOT a runtime migration. These tables already exist in each
-- p_<project_id> schema and remain owned by the current tenant-schema lifecycle.
-- Keep this projection aligned with the deployed PostgreSQL shape.

CREATE TABLE chat_conversations (
    id serial PRIMARY KEY,
    uuid uuid NOT NULL UNIQUE,
    name varchar NOT NULL,
    is_private boolean NOT NULL DEFAULT TRUE,
    author_id integer NOT NULL,
    meta jsonb NOT NULL DEFAULT '{}'::jsonb,
    source varchar(64) NOT NULL DEFAULT 'elitea',
    instructions varchar,
    created_at timestamp NOT NULL DEFAULT now(),
    updated_at timestamp
);

CREATE TABLE chat_participants (
    id serial PRIMARY KEY,
    uuid uuid NOT NULL UNIQUE,
    entity_name varchar NOT NULL,
    entity_meta jsonb NOT NULL DEFAULT '{}'::jsonb,
    meta json NOT NULL DEFAULT '{}'::json
);

CREATE TABLE chat_participant_mapping (
    id serial PRIMARY KEY,
    conversation_id integer NOT NULL REFERENCES chat_conversations(id),
    participant_id integer NOT NULL REFERENCES chat_participants(id),
    entity_settings jsonb NOT NULL DEFAULT '{}'::jsonb,
    created_at timestamp NOT NULL DEFAULT now(),
    updated_at timestamp,
    UNIQUE (participant_id, conversation_id)
);

CREATE TABLE chat_message_group (
    id serial PRIMARY KEY,
    uuid uuid NOT NULL UNIQUE,
    author_participant_id integer NOT NULL REFERENCES chat_participants(id),
    conversation_id integer NOT NULL REFERENCES chat_conversations(id),
    sent_to_id integer REFERENCES chat_participants(id),
    reply_to_id integer REFERENCES chat_message_group(id),
    meta jsonb NOT NULL DEFAULT '{}'::jsonb,
    is_streaming boolean NOT NULL DEFAULT FALSE,
    created_at timestamp NOT NULL DEFAULT now(),
    updated_at timestamp,
    task_id varchar(64)
);

CREATE TABLE chat_message_items (
    id serial PRIMARY KEY,
    uuid uuid NOT NULL UNIQUE,
    item_type varchar(50) NOT NULL,
    order_index integer NOT NULL,
    meta jsonb NOT NULL DEFAULT '{}'::jsonb,
    created_at timestamp NOT NULL DEFAULT now(),
    updated_at timestamp,
    message_group_id integer NOT NULL REFERENCES chat_message_group(id)
);

CREATE TABLE chat_messages_text (
    id integer PRIMARY KEY REFERENCES chat_message_items(id) ON DELETE CASCADE,
    content text NOT NULL
);

-- Existing current-schema trace projection. This is a SQLC compiler input,
-- not a migration; the tenant lifecycle remains the table owner.
CREATE TABLE chat_message_trace_step (
    id bigint PRIMARY KEY,
    message_group_id integer NOT NULL REFERENCES chat_message_group(id) ON DELETE CASCADE,
    kind text NOT NULL,
    run_id text,
    parent_agent_name text,
    parent_agent_call_id text,
    started_at timestamptz,
    finished_at timestamptz,
    is_error boolean NOT NULL DEFAULT FALSE,
    has_visible_content boolean NOT NULL DEFAULT TRUE,
    tool_name text,
    tool_inputs jsonb,
    tool_output text,
    finish_reason text,
    step_type text,
    text text,
    thinking text,
    model_name text,
    attrs jsonb
);

-- Existing current-schema support-assistant context projection. This is a
-- SQLC compiler input only, not a migration or a replacement schema.
CREATE TABLE chat_messages_context (
    context_data jsonb NOT NULL,
    context_type text,
    id integer PRIMARY KEY REFERENCES chat_message_items(id) ON DELETE CASCADE
);

-- Chat attachment payload child, the third 1:1 sibling of chat_messages_text
-- and chat_messages_context above. This is a SQLC compiler input only: the
-- tenant migration chain owns the real DDL, in
-- migrations/tenant/0127_chat_message_attachment_items.sql (#606 option 1),
-- and this declaration must never be read as the definition of the table.
--
-- Column-for-column identical to 0127, which in turn is MEASURED from the
-- pg_catalog dump of the live legacy database
-- (testdata/postgres/legacy-centry-catalog.json, schema p_1) rather than
-- transcribed from pylon's SQLAlchemy model. Two details that look like
-- oversights here are load-bearing and are copied deliberately:
--
--   * `content` is `json`, NOT `jsonb`. The deployed column is `json`, and in
--     a deployment where pylon and this service both touch the row, `jsonb`
--     would normalise whitespace, reorder keys and drop duplicates, so the
--     bytes read back would not be the bytes written. 0127 records the same
--     reasoning at length.
--   * There is no index beyond the primary key, because the deployed table has
--     none and the only access path is a join on `id` from chat_message_items.
--
-- The item_type discriminator that selects this payload is the literal
-- `attachment_message` (elitea_core/models/message_items/attachment.py:15-17
-- `polymorphic_identity`), NOT `attachment`.
CREATE TABLE chat_messages_attachment (
    id integer PRIMARY KEY REFERENCES chat_message_items(id) ON DELETE CASCADE,
    name varchar(256) NOT NULL,
    bucket varchar(256) NOT NULL,
    attachment_type varchar(256) NOT NULL,
    content json
);

-- Canvas payload tables (tenant migration 0129), projected because the turn
-- resolvers read a canvas's NEWEST version into the chat history the model is
-- shown (agent_chat.sql). Same columns as 0129; the authors table is omitted
-- because no query here reads it.
CREATE TABLE chat_messages_canvas (
    name text NOT NULL,
    canvas_type varchar NOT NULL,
    id integer PRIMARY KEY REFERENCES chat_message_items(id) ON DELETE CASCADE
);

CREATE TABLE chat_canvas_versions (
    id serial PRIMARY KEY,
    code_language varchar(32),
    canvas_content text NOT NULL,
    canvas_item_id integer NOT NULL REFERENCES chat_messages_canvas(id) ON DELETE CASCADE,
    created_at timestamp NOT NULL DEFAULT now()
);

-- Current application/tool ownership projection used only to keep admission
-- queries type-checked. The tenant schema lifecycle continues to own this
-- already-existing table; this compiler input is not a migration.
CREATE TABLE entity_tool_mapping (
    id serial PRIMARY KEY,
    tool_id integer NOT NULL,
    entity_id integer NOT NULL,
    entity_version_id integer NOT NULL,
    entity_type varchar NOT NULL,
    created_at timestamp NOT NULL DEFAULT now(),
    updated_at timestamp,
    selected_tools jsonb,
    CONSTRAINT entity_tool_mapping_version_tool_type_unique
        UNIQUE (entity_version_id, tool_id, entity_type)
);

-- Existing current-schema Skills projections used only to keep direct
-- application admission type-checked. These declarations are SQLC compiler
-- inputs, not migrations; the tenant schema lifecycle remains their owner.
CREATE TABLE skills (
    id serial PRIMARY KEY,
    name varchar(128) NOT NULL,
    description varchar(2304) NOT NULL,
    owner_id integer NOT NULL,
    author_id integer NOT NULL,
    created_at timestamp NOT NULL DEFAULT now(),
    uuid uuid UNIQUE DEFAULT gen_random_uuid(),
    meta jsonb DEFAULT '{}'::jsonb
);

CREATE TABLE skill_versions (
    id serial PRIMARY KEY,
    skill_id integer NOT NULL REFERENCES skills(id) ON DELETE CASCADE,
    name varchar(128) NOT NULL DEFAULT 'base',
    instructions text NOT NULL,
    author_id integer NOT NULL,
    created_at timestamp NOT NULL DEFAULT now(),
    uuid uuid UNIQUE DEFAULT gen_random_uuid(),
    meta jsonb DEFAULT '{}'::jsonb,
    CONSTRAINT skill_version_name_unique UNIQUE (skill_id, name)
);

CREATE TABLE entity_skill_mapping (
    id serial PRIMARY KEY,
    entity_version_id integer NOT NULL,
    entity_type varchar(50) NOT NULL,
    skill_id integer NOT NULL REFERENCES skills(id) ON DELETE CASCADE,
    skill_version_id integer REFERENCES skill_versions(id),
    created_at timestamp NOT NULL DEFAULT now(),
    updated_at timestamp,
    CONSTRAINT entity_skill_mapping_unique
        UNIQUE (entity_version_id, skill_id, entity_type)
);
