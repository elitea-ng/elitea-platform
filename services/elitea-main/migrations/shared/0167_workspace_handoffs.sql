-- 0167_workspace_handoffs.sql — the ownership row of a desktop ⇄ cloud
-- handoff (ADR-0034 decisions 1 and 7).
--
-- ONE ROW PER LOCAL WORK THREAD. The key is (project_id, conversation_uuid):
-- the conversation of a desktop Local work thread (`source = 'local_work'` in
-- the tenant chat_conversations). A thread with no row is LOCAL. A later send
-- of the same thread after a release REUSES the row: it gets a new handoff_id,
-- the epoch keeps counting, and the refs reset (workspace_refs, shared/0168).
--
-- THE STATE MACHINE (decision 7). `state` is one of LOCAL, HANDING_OFF,
-- CLOUD_IDLE, CLOUD_RUNNING and RECLAIMING. Every ownership change is one
-- `UPDATE ... WHERE state = $from AND epoch = $epoch`, so two devices racing
-- on a send get exactly one winner. The epoch increments on every ownership
-- change; a cloud turn's CLOUD_IDLE -> CLOUD_RUNNING -> CLOUD_IDLE keeps it
-- (the wire contract's runtime context carries the epoch of CLOUD_IDLE).
-- `reclaim_pending` is the desktop's take-back during a running cloud turn: it
-- becomes RECLAIMING at turn end, or when the 60-second grace runs out.
--
-- THE SHAS are a cache of the thread's refs for the banner and the runtime
-- context; workspace_refs is the authority.
--
-- RETENTION (decision 12). `delete_after` is when the thread's objects are
-- erased (release + return_grace_days, explicit erase = now). `deleted_at` is
-- set only after a listing of the thread's object prefix returned empty.
--
-- NO FOREIGN KEYS (decision 15): neither into the tenant schemas nor into
-- centry, so the table can move to its own PostgreSQL instance. The sweeper
-- erases the row's objects when the conversation or the project is gone.
--
-- Idempotent. No BEGIN/COMMIT: the ledgered runner wraps the file.

CREATE TABLE IF NOT EXISTS elitea_runtime.workspace_handoffs (
    project_id                  integer     NOT NULL,
    conversation_uuid           uuid        NOT NULL,
    handoff_id                  uuid        NOT NULL,
    state                       text        NOT NULL,
    epoch                       bigint      NOT NULL,
    reclaim_pending             boolean     NOT NULL DEFAULT FALSE,
    reclaim_requested_at        timestamptz,
    owner_user_id               integer     NOT NULL,
    device_id                   text        NOT NULL DEFAULT '',
    device_name                 text        NOT NULL DEFAULT '',
    token_id                    text        NOT NULL DEFAULT '',
    history_mode                text        NOT NULL DEFAULT 'none',
    has_upstream                boolean     NOT NULL DEFAULT FALSE,
    base_sha                    text,
    upstream_sha                text,
    handoff_sha                 text,
    cloud_sha                   text,
    excluded                    jsonb       NOT NULL DEFAULT '[]'::jsonb,
    stats                       jsonb       NOT NULL DEFAULT '{}'::jsonb,
    note                        text        NOT NULL DEFAULT '',
    last_seq                    bigint      NOT NULL DEFAULT 0,
    running_execution_id        text,
    running_response_message_id uuid,
    expires_at                  timestamptz,
    state_changed_at            timestamptz NOT NULL DEFAULT clock_timestamp(),
    created_at                  timestamptz NOT NULL DEFAULT clock_timestamp(),
    updated_at                  timestamptz NOT NULL DEFAULT clock_timestamp(),
    delete_after                timestamptz,
    deleted_at                  timestamptz,
    PRIMARY KEY (project_id, conversation_uuid),
    CONSTRAINT workspace_handoffs_handoff_id_uniq UNIQUE (handoff_id),
    CONSTRAINT workspace_handoffs_state_valid
        CHECK (state IN ('LOCAL', 'HANDING_OFF', 'CLOUD_IDLE', 'CLOUD_RUNNING', 'RECLAIMING')),
    CONSTRAINT workspace_handoffs_epoch_valid CHECK (epoch >= 0),
    CONSTRAINT workspace_handoffs_history_valid CHECK (history_mode IN ('none', 'full')),
    CONSTRAINT workspace_handoffs_reclaim_pending_running
        CHECK (NOT reclaim_pending OR state = 'CLOUD_RUNNING'),
    CONSTRAINT workspace_handoffs_running_execution
        CHECK ((state = 'CLOUD_RUNNING') = (running_execution_id IS NOT NULL)),
    CONSTRAINT workspace_handoffs_sha_shape CHECK (
        (base_sha IS NULL OR base_sha ~ '^[0-9a-f]{40}$') AND
        (upstream_sha IS NULL OR upstream_sha ~ '^[0-9a-f]{40}$') AND
        (handoff_sha IS NULL OR handoff_sha ~ '^[0-9a-f]{40}$') AND
        (cloud_sha IS NULL OR cloud_sha ~ '^[0-9a-f]{40}$')),
    CONSTRAINT workspace_handoffs_note_len CHECK (octet_length(note) <= 8192)
);

-- The sweeper's timed transitions (HANDING_OFF expiry, idle retention,
-- abandoned RECLAIMING, the reclaim grace) read rows that are not LOCAL.
CREATE INDEX IF NOT EXISTS workspace_handoffs_active
    ON elitea_runtime.workspace_handoffs (state, state_changed_at)
    WHERE state <> 'LOCAL';

-- The sweeper's erasure reads rows whose objects are due.
CREATE INDEX IF NOT EXISTS workspace_handoffs_erase_due
    ON elitea_runtime.workspace_handoffs (delete_after)
    WHERE deleted_at IS NULL AND delete_after IS NOT NULL;
