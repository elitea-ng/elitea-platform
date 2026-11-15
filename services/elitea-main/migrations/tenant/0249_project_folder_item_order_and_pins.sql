-- 0249_project_folder_item_order_and_pins.sql — order inside a folder, and the
-- personal Pinned group of the Agents, Pipelines, Skills, Toolkits and MCPs
-- lists (issue 1628, ADR-0051 M2).
--
-- This EXTENDS the model of 0247 instead of adding a second one:
--
--   project_folder_items.position   the entity's place inside its folder.
--       Same rule as the chat folders and project_folders.position: sorts
--       DESCENDING (highest first), a move to the top lands one gap (1,000,000)
--       above the folder's current maximum, a drop between two entities takes
--       the midpoint and the neighbour ids win over any client position. Rows
--       written by 0247 keep 0 and read in entity-id order, as before.
--
--   project_entity_pins             what ONE member pinned. Private to that
--       user: another member never sees it. centry.social_pins is NOT reused
--       because it holds ONE shared pin per (entity, project, entity_id), is
--       owned by the legacy schema, and feeds the shared is_pinned flag of
--       configuration lists; a "private" pin there would pin for the whole
--       project. This table has the same shape per user and nothing else reads
--       it. A pin whose entity is deleted or changed kind is filtered out at
--       read time, like folder items (the id spaces are the entity tables').
--
-- Unqualified names: the runner pins search_path to the tenant schema. No
-- BEGIN/COMMIT. Idempotent.

ALTER TABLE project_folder_items ADD COLUMN IF NOT EXISTS position integer NOT NULL DEFAULT 0;

CREATE TABLE IF NOT EXISTS project_entity_pins (
    user_id bigint NOT NULL,
    entity_kind varchar(16) NOT NULL
        CONSTRAINT project_entity_pins_kind_check CHECK (entity_kind IN ('agent', 'pipeline', 'skill', 'toolkit', 'mcp')),
    entity_id integer NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (user_id, entity_kind, entity_id)
);
