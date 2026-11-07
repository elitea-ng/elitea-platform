-- 0163_agent_memory_entries.sql — an agent's own memory, per project
-- (ADR-0050 decision 1b, issue 1607).
--
-- agent_memory_entries
--   One remembered entry of one agent in this project. The key is the agent
--   registry key, (project, application_id): the project is the schema, so the
--   same agent in another project has another, empty memory, and nothing here
--   is exported or forked with the agent.
--
--   kind             fact | preference | episode | cursor. A cursor is the
--                    heartbeat's last-seen marker (decision 2), system-managed.
--   content          at most 2 KiB, never blank. A credential is refused
--                    before it gets here (internal/domain/agentmemory).
--   subject_user_id  the person the entry is about, or NULL. Such an entry is
--                    recalled only when that person is in the conversation and
--                    is visible only to that person, the agent's owners and
--                    the project's memory administrators.
--   pinned           pinned entries are recalled first and never archived by
--                    the limit.
--   status           pending (waiting for a person, never recalled) | active |
--                    archived (kept for review, never recalled).
--   version          1 on insert, +1 on every edit. Each version's text is in
--                    agent_memory_entry_versions.
--   provenance       the conversation, message and run that wrote it, and who:
--                    the agent (written_by_kind agent) or a person editing it
--                    (person, written_by_user_id). None is a foreign key: an
--                    entry outlives the conversation it came from.
--
-- application_id is NOT a foreign key either: deleting the agent deletes its
-- memory after the retention period (ADR-0042), not at once.
--
-- agent_memory_entry_versions
--   Every version of every entry, the current one included, so the history
--   survives an edit. Deleted with the entry.
--
-- agent_memory_subject_optouts
--   A person who turned off "remember things about me" for one agent. The
--   agent cannot write an entry about them while the row exists.
--
-- No to_regclass guard: no table here references another. Unqualified names:
-- the runner pins search_path to the tenant schema. No BEGIN/COMMIT.
CREATE TABLE IF NOT EXISTS agent_memory_entries (
    id                 bigserial PRIMARY KEY,
    application_id     bigint      NOT NULL CHECK (application_id > 0),
    kind               text        NOT NULL
                       CHECK (kind IN ('fact', 'preference', 'episode', 'cursor')),
    content            text        NOT NULL
                       CHECK (btrim(content) <> '' AND octet_length(content) <= 2048),
    subject_user_id    integer     CHECK (subject_user_id IS NULL OR subject_user_id > 0),
    pinned             boolean     NOT NULL DEFAULT false,
    status             text        NOT NULL DEFAULT 'active'
                       CHECK (status IN ('pending', 'active', 'archived')),
    version            integer     NOT NULL DEFAULT 1 CHECK (version > 0),
    conversation_uuid  uuid,
    message_id         text        CHECK (message_id IS NULL OR length(message_id) BETWEEN 1 AND 64),
    execution_id       text        CHECK (execution_id IS NULL OR length(execution_id) BETWEEN 1 AND 128),
    written_by_kind    text        NOT NULL CHECK (written_by_kind IN ('agent', 'person')),
    written_by_user_id integer     CHECK (written_by_user_id IS NULL OR written_by_user_id > 0),
    created_at         timestamptz NOT NULL DEFAULT clock_timestamp(),
    updated_at         timestamptz NOT NULL DEFAULT clock_timestamp()
);

-- Recall and the limit: one agent's entries by status, newest first.
CREATE INDEX IF NOT EXISTS ix_agent_memory_entries_agent_status
    ON agent_memory_entries (application_id, status, pinned DESC, updated_at DESC);

-- The Memory tab's "about this person" filter and a person's own entries.
CREATE INDEX IF NOT EXISTS ix_agent_memory_entries_subject
    ON agent_memory_entries (application_id, subject_user_id)
    WHERE subject_user_id IS NOT NULL;

CREATE TABLE IF NOT EXISTS agent_memory_entry_versions (
    entry_id           bigint      NOT NULL REFERENCES agent_memory_entries (id) ON DELETE CASCADE,
    version            integer     NOT NULL CHECK (version > 0),
    content            text        NOT NULL CHECK (octet_length(content) <= 2048),
    kind               text        NOT NULL
                       CHECK (kind IN ('fact', 'preference', 'episode', 'cursor')),
    status             text        NOT NULL CHECK (status IN ('pending', 'active', 'archived')),
    pinned             boolean     NOT NULL,
    written_by_kind    text        NOT NULL CHECK (written_by_kind IN ('agent', 'person')),
    written_by_user_id integer     CHECK (written_by_user_id IS NULL OR written_by_user_id > 0),
    created_at         timestamptz NOT NULL DEFAULT clock_timestamp(),
    PRIMARY KEY (entry_id, version)
);

CREATE TABLE IF NOT EXISTS agent_memory_subject_optouts (
    application_id bigint      NOT NULL CHECK (application_id > 0),
    user_id        integer     NOT NULL CHECK (user_id > 0),
    created_at     timestamptz NOT NULL DEFAULT clock_timestamp(),
    PRIMARY KEY (application_id, user_id)
);
