-- Persistent, cross-conversation personal memory (#870).
--
-- WHAT THIS IS. Settings > Profile's "Long-term Memory" accordion
-- (features/settings/ui/profile/ProfileLongTermMemory.tsx) has always
-- rendered a dimmed "Coming soon" card. Settings > Memory
-- (features/settings/ui/memory/MemoryContextManagement.tsx) is a REAL,
-- shipped feature, but it configures context-WINDOW management (token
-- ceilings, summarization) for the CURRENT conversation — it stores no
-- content and remembers nothing across conversations. Neither page, nor
-- anything in this schema before 0136, gave a user a place to keep a fact
-- the assistant should recall in a LATER, unrelated conversation.
--
-- This table is that place: one row per remembered fact, scoped to the user
-- who wrote it and the project it lives in (memory does not cross projects,
-- matching every other personal record in this schema — chat_message_feedback
-- (0135), chat_conversation_folders — which is per (project, user) too).
--
-- SHAPE
--
--   personal_memory_entries
--     id                         bigserial   PRIMARY KEY
--     user_id                    integer     NOT NULL
--     content                    text        NOT NULL, non-blank
--     tags                       text[]      NOT NULL DEFAULT '{}'
--     source_conversation_uuid   uuid        NULL
--     enabled                    boolean     NOT NULL DEFAULT TRUE
--     created_at                 timestamptz NOT NULL DEFAULT now()
--     updated_at                 timestamptz NOT NULL DEFAULT now()
--
-- `user_id` is NOT a foreign key, matching 0135's own header: user identity
-- in this schema is authenticated centrally (auth.User.ID) and never stored
-- as a tenant-schema foreign key. No table here constrains it.
--
-- `source_conversation_uuid` records where a memory came from (the
-- "Remember this" message action stamps the owning conversation's uuid —
-- chat_conversations.uuid, chat_message_group's own external identifier
-- shape) but is DELIBERATELY NOT a foreign key and carries no ON DELETE
-- action: a memory is meant to outlive the conversation it was captured
-- from. Deleting the source conversation must not silently delete, or even
-- constrain, a fact the user already asked to keep. A dangling uuid a
-- reader cannot resolve is treated as "no known source", not an error.
--
-- `enabled` is the per-entry switch the settings panel's row toggle writes;
-- recall (the runtime context-injection path, wired separately — see
-- internal/application/personalization and the /elitea_core/memories*
-- routes' own comments) reads only `enabled = true` rows. A user can turn a
-- memory off without deleting it, then turn it back on.
--
-- `tags` is a plain text[] rather than a join table: tags here are
-- free-form user labels for the user's OWN search/filter convenience, not a
-- shared taxonomy anything else in the schema joins against — the same
-- "not worth a second table" call 0132's header explains for eval_datasets'
-- `application_id`.
--
-- No to_regclass guard: unlike 0135 (which REFERENCES chat_message_group and
-- must skip cleanly where that table is absent), this table has no foreign
-- key to anything else in the schema, so CREATE TABLE IF NOT EXISTS is safe
-- unconditionally.
--
-- Table names are UNQUALIFIED: tenant migrations run with a transaction-local
-- search_path pinned to the tenant schema (internal/infra/db/migrate/
-- runner.go:83-96).
CREATE TABLE IF NOT EXISTS personal_memory_entries (
    id bigserial PRIMARY KEY,
    user_id integer NOT NULL,
    content text NOT NULL,
    tags text[] NOT NULL DEFAULT '{}',
    source_conversation_uuid uuid,
    enabled boolean NOT NULL DEFAULT TRUE,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),

    CONSTRAINT personal_memory_entries_content_nonempty_check
        CHECK (btrim(content) <> '')
);

-- The recall path's own access shape: "this user's enabled memories, most
-- recent first" (see the runtime injector). One index serves it directly
-- instead of a sequential scan per turn.
CREATE INDEX IF NOT EXISTS ix_tenant_personal_memory_entries_user_enabled
    ON personal_memory_entries (user_id, enabled, created_at DESC);
