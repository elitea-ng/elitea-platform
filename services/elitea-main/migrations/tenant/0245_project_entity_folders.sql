-- 0245_project_entity_folders.sql — folders for the Agents, Pipelines, Skills,
-- Toolkits and MCPs lists (issue 2048).
--
-- ONE model for all five lists, keyed by `entity_kind`:
--
--   agent      applications.id (a classic agent)
--   pipeline   applications.id (an application with a pipeline version)
--   skill      skills.id
--   toolkit    elitea_tools.id (a toolkit that is not an MCP)
--   mcp        elitea_tools.id (an MCP toolkit)
--
-- Unlike chat_conversation_folders, which belong to one user, these folders are
-- PROJECT-scoped and shared with every member, exactly like the entities they
-- group. `created_by` is recorded for audit only; it gates nothing.
--
-- A folder holds entities of ONE kind. `project_folder_items` places an entity
-- in at most one folder (PRIMARY KEY (entity_kind, entity_id)); no row means the
-- entity sits at the root. The composite FK (folder_id, entity_kind) makes it
-- impossible to put a skill into an agent folder even by a writer that skips
-- the API. Deleting a folder cascades to its item rows ONLY: the entities are
-- never touched, they simply return to the root.
--
-- `position` follows the chat-folder rule: sorts DESCENDING (highest first),
-- a new folder is created one PositionGap (1,000,000) above the current maximum.
--
-- Names are unique per kind, case-insensitively, so two members cannot make two
-- folders both called "Drafts" in the same list.
--
-- Unqualified names: the runner pins search_path to the tenant schema. No
-- BEGIN/COMMIT. Idempotent.

CREATE TABLE IF NOT EXISTS project_folders (
    id serial PRIMARY KEY,
    entity_kind varchar(16) NOT NULL
        CONSTRAINT project_folders_kind_check CHECK (entity_kind IN ('agent', 'pipeline', 'skill', 'toolkit', 'mcp')),
    name varchar(128) NOT NULL,
    position integer NOT NULL DEFAULT 0,
    created_by bigint,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT project_folders_id_kind_key UNIQUE (id, entity_kind)
);

CREATE UNIQUE INDEX IF NOT EXISTS project_folders_kind_name_key
    ON project_folders (entity_kind, lower(name));

CREATE TABLE IF NOT EXISTS project_folder_items (
    entity_kind varchar(16) NOT NULL,
    entity_id integer NOT NULL,
    folder_id integer NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (entity_kind, entity_id),
    CONSTRAINT project_folder_items_folder_fkey
        FOREIGN KEY (folder_id, entity_kind) REFERENCES project_folders (id, entity_kind) ON DELETE CASCADE
);

CREATE INDEX IF NOT EXISTS project_folder_items_folder_idx ON project_folder_items (folder_id);
