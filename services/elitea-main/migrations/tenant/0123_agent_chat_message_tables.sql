-- Give the agent-execution chat tables an owner in this repository.
--
-- internal/db/queries/{agent_chat,agent_cancel}.sql reference this table set
-- 155 times, and until now no migration created any of it.
-- internal/db/schema/agent_chat_baseline.sql declares it for the sqlc
-- compiler and says why it is not a migration: "These tables already exist in
-- each p_<project_id> schema and remain owned by the current tenant-schema
-- lifecycle." That lifecycle is the legacy runtime's. The assumption holds in
-- deploy/centry-hybrid, which still runs the main legacy runtime; it does not
-- hold in a deployment built to drop the legacy runtime, where an authenticated
-- agent turn fails with `relation "chat_messages_text" does not exist`
-- (SQLSTATE 42P01) before it can dispatch. See #287.
--
-- This migration owns the WHOLE graph, not just the three leaf tables the
-- 42P01 named. An earlier revision created only chat_messages_text,
-- chat_messages_context and chat_message_trace_step, and took the five tables
-- they hang off — chat_conversations, chat_participants,
-- chat_participant_mapping, chat_message_group, chat_message_items — as
-- pre-existing. Nothing in this repository creates those either, so the
-- omission failed twice over: the leaves' REFERENCES clauses had no target, so
-- the tenant chain could not apply to a clean database at all, and a
-- legacy-free deployment that got past that would still have met the same 42P01
-- one table further in. It was invisible during the first round because the
-- only stack it was verified against restores a staging dump
-- (deploy/scripts/load-staging-dump.sh), where the legacy runtime had already
-- created all eight.
--
-- Every statement is IF NOT EXISTS because in a mixed deployment the legacy
-- runtime has already created these tables and owns their shape: there this
-- migration must record itself and change nothing. It becomes the definition
-- only where nothing else defines them. That is also why the parents are added
-- to this migration rather than a later one — 0123 must not reference a table
-- that a higher version number has yet to create, and it has never been applied
-- anywhere durable, only to local clean-slate stacks.
--
-- The column lists mirror agent_chat_baseline.sql exactly, with two corrections
-- the projection could not express:
--
-- * chat_message_trace_step.id is declared there as a bare `bigint PRIMARY
--   KEY`, but repos/agent_trace.go inserts a trace step WITHOUT supplying an
--   id, so the column must carry a default. The integration fixtures that
--   exercise that insert use BIGSERIAL
--   (index_activity_postgres_integration_test.go:553,
--   messagetraces_postgres_integration_test.go:174); a projection has no reason
--   to record a default, so sqlc's view dropped it.
--
-- * chat_conversations.uuid gets DEFAULT gen_random_uuid(). The projection
--   records none, but deploy/scripts/fix-uuid-defaults.sql exists purely to add
--   that default back to this column across every deployed project schema, so a
--   schema we create ourselves should not be born needing the repair.

-- The conversation a message group belongs to, and the participants that author
-- and receive it. No query in this repository INSERTs into either — conversation
-- and participant creation still happens outside the Go service — but every
-- agent-turn read joins through them, so they have to exist for the graph below
-- to have anywhere to point.
CREATE TABLE IF NOT EXISTS chat_conversations (
    id serial PRIMARY KEY,
    uuid uuid NOT NULL UNIQUE DEFAULT gen_random_uuid(),
    name varchar NOT NULL,
    is_private boolean NOT NULL DEFAULT TRUE,
    author_id integer NOT NULL,
    meta jsonb NOT NULL DEFAULT '{}'::jsonb,
    source varchar(64) NOT NULL DEFAULT 'elitea',
    instructions varchar,
    created_at timestamp NOT NULL DEFAULT now(),
    updated_at timestamp
);

CREATE TABLE IF NOT EXISTS chat_participants (
    id serial PRIMARY KEY,
    uuid uuid NOT NULL UNIQUE,
    entity_name varchar NOT NULL,
    entity_meta jsonb NOT NULL DEFAULT '{}'::jsonb,
    meta json NOT NULL DEFAULT '{}'::json
);

-- Which participants are in which conversation, and with what per-conversation
-- settings. The UNIQUE is what makes "this agent, in this conversation" a single
-- row the turn resolver can look up rather than a set it has to disambiguate.
CREATE TABLE IF NOT EXISTS chat_participant_mapping (
    id serial PRIMARY KEY,
    conversation_id integer NOT NULL REFERENCES chat_conversations(id),
    participant_id integer NOT NULL REFERENCES chat_participants(id),
    entity_settings jsonb NOT NULL DEFAULT '{}'::jsonb,
    created_at timestamp NOT NULL DEFAULT now(),
    updated_at timestamp,
    UNIQUE (participant_id, conversation_id)
);

-- One turn: the question group the user sends, or the response group the agent
-- streams back into. reply_to_id is the self-reference that pairs them, and
-- task_id is how a streaming response is matched to its execution.
CREATE TABLE IF NOT EXISTS chat_message_group (
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

-- The ordered parts of one group. item_type is the discriminator that says
-- which of the 1:1 payload tables below holds this item's content.
CREATE TABLE IF NOT EXISTS chat_message_items (
    id serial PRIMARY KEY,
    uuid uuid NOT NULL UNIQUE,
    item_type varchar(50) NOT NULL,
    order_index integer NOT NULL,
    meta jsonb NOT NULL DEFAULT '{}'::jsonb,
    created_at timestamp NOT NULL DEFAULT now(),
    updated_at timestamp,
    message_group_id integer NOT NULL REFERENCES chat_message_group(id)
);

-- One row per text message item. Shares chat_message_items' primary key rather
-- than carrying its own, which is what makes the item's type discriminator and
-- its payload impossible to disagree.
CREATE TABLE IF NOT EXISTS chat_messages_text (
    id integer PRIMARY KEY REFERENCES chat_message_items(id) ON DELETE CASCADE,
    content text NOT NULL
);

-- Same 1:1 shape for context items (support-assistant context and friends).
-- context_type is nullable: the adhoc resolver treats a NULL type as "not a
-- support_assistant_context" and refuses the turn on it, so the column has to
-- be able to hold that state rather than defaulting to something plausible.
CREATE TABLE IF NOT EXISTS chat_messages_context (
    context_data jsonb NOT NULL,
    context_type text,
    id integer PRIMARY KEY REFERENCES chat_message_items(id) ON DELETE CASCADE
);

-- Execution-step drill-down (#277). Written by repos/agent_trace.go as the
-- worker streams node events, read by the message-traces API, and deleted
-- wholesale per message group on regeneration.
CREATE TABLE IF NOT EXISTS chat_message_trace_step (
    id bigserial PRIMARY KEY,
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

-- PostgreSQL does not index a referencing column for you. Every access path in
-- the query set is by message group — the trace read, the FOR UPDATE lock and
-- the regeneration DELETE — so without this each one degrades to a sequential
-- scan over the project's whole trace history as conversations accumulate.
CREATE INDEX IF NOT EXISTS chat_message_trace_step_message_group_id_idx
    ON chat_message_trace_step (message_group_id, id);

-- Same reasoning for the two referencing columns the graph above adds. Items
-- are always read, counted and ordered by group — InsertCurrentAgentTextItem
-- runs a count(*) over this column to pick the next order_index on every single
-- streamed text item — and groups are always read by conversation.
CREATE INDEX IF NOT EXISTS chat_message_items_message_group_id_idx
    ON chat_message_items (message_group_id, order_index);

CREATE INDEX IF NOT EXISTS chat_message_group_conversation_id_idx
    ON chat_message_group (conversation_id, id);
