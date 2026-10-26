-- 0177_workspace_turn_changes.sql — the files a cloud turn changed, for the
-- web "Files changed" view of a cloud thread (#1187, ADR-0034 Phase 2).
--
-- The worker computes them from its own repository right after it publishes
-- a turn (WorkspaceRecordChanges on the runtime plane): what the turn changed
-- (its parent's tree to its commit's tree) and what every cloud turn changed
-- since the handoff snapshot. Main reads no git pack; it keeps what the worker
-- sends, bounded, minus every path the thread's exclusions or path deny list
-- match.
--
--   workspace_turn_changes  one row per published cloud commit of a handoff:
--                           the two manifests (paths, status, +/- counts, no
--                           diff text). parent_sha links the live chain, so a
--                           regenerated answer's earlier attempt is not read;
--                           depth is the turn number from the snapshot (0:
--                           unknown), which bridges a turn with no record.
--   workspace_change_diffs  the size-capped unified diff of one file of one
--                           scope (`turn` or `cumulative`). Only the newest
--                           commit of a thread keeps its cumulative diffs.
--
-- Both are erased with the cloud copy (the erasure, and a new handoff of the
-- thread). No foreign keys into tenant tables (decision 15).
--
-- Idempotent. No BEGIN/COMMIT: the ledgered runner wraps the file.

CREATE TABLE IF NOT EXISTS elitea_runtime.workspace_turn_changes (
    project_id          integer     NOT NULL,
    conversation_uuid   uuid        NOT NULL,
    commit_sha          text        NOT NULL,
    handoff_id          uuid        NOT NULL,
    parent_sha          text        NOT NULL,
    base_sha            text        NOT NULL,
    seq                 bigint      NOT NULL,
    depth               integer     NOT NULL DEFAULT 0,
    execution_id        text        NOT NULL,
    response_message_id text,
    turn_files          jsonb       NOT NULL DEFAULT '{}'::jsonb,
    cumulative_files    jsonb       NOT NULL DEFAULT '{}'::jsonb,
    created_at          timestamptz NOT NULL DEFAULT clock_timestamp(),
    PRIMARY KEY (project_id, conversation_uuid, commit_sha),
    CONSTRAINT workspace_turn_changes_commit_valid CHECK (commit_sha ~ '^[0-9a-f]{40}$'),
    CONSTRAINT workspace_turn_changes_parent_valid CHECK (parent_sha ~ '^[0-9a-f]{40}$'),
    CONSTRAINT workspace_turn_changes_base_valid CHECK (base_sha ~ '^[0-9a-f]{40}$'),
    CONSTRAINT workspace_turn_changes_depth_valid CHECK (depth >= 0)
);

CREATE INDEX IF NOT EXISTS workspace_turn_changes_handoff
    ON elitea_runtime.workspace_turn_changes (project_id, conversation_uuid, handoff_id);

CREATE TABLE IF NOT EXISTS elitea_runtime.workspace_change_diffs (
    project_id        integer NOT NULL,
    conversation_uuid uuid    NOT NULL,
    commit_sha        text    NOT NULL,
    scope             text    NOT NULL,
    path              text    NOT NULL,
    diff              text    NOT NULL,
    PRIMARY KEY (project_id, conversation_uuid, commit_sha, scope, path),
    CONSTRAINT workspace_change_diffs_scope_valid CHECK (scope IN ('turn', 'cumulative')),
    CONSTRAINT workspace_change_diffs_size_valid CHECK (octet_length(diff) <= 49152)
);
