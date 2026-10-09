-- 0155_local_turn_executions.sql — the execution of an agent turn that runs on
-- the user's machine (ADR-0029 decision 5c, client contract 1.5).
--
-- WHY A TABLE OF ITS OWN. A desktop local turn is started and committed over
-- the public API with the caller's native token or PAT. No worker ever runs
-- it, and the desktop never joins the worker protocol (ADR-0029 decision 1).
-- elitea_runtime.execution_jobs is that protocol's kernel: every row there is
-- bound to an input bundle, a signed command in command_outbox, the admission
-- capacity trigger (0126) and the replay-event stream. A local turn in it
-- would count against the cloud agents' admission cap, hang the events
-- stream on heartbeats, and need a fake input bundle and a fake capability.
--
-- What a local turn needs from "an execution" is narrow, and this table holds
-- exactly that:
--   * an id the /llm edge keeps in X-Elitea-Execution-Id, because it names a
--     live execution of the caller in the project
--     (repos.ExecutionAttributionVerifier, the same rule as execution_jobs);
--   * the binding to one project, actor, conversation and question, so the
--     commit can be refused to anyone else, and to the credential family that
--     started it (token_id: a native device session's anchor token, stable
--     across access-token rotation, or the personal access token; plus
--     native_client_id), so another device or token of the same user can
--     neither commit it, nor call a remote toolkit in it, nor have /llm usage
--     attributed to it;
--   * a deadline, so a turn the desktop never commits expires the way an
--     unclaimed cloud run does (24 h, the cloud agent deadline);
--   * the commit, recorded once, so a retried commit is idempotent.
--
-- The transcript itself is NOT here: the commit writes the existing tenant
-- chat and trace projection tables (chat_message_group, chat_message_items,
-- chat_messages_text, chat_message_trace_step), so every reader of a turn,
-- the changes_since delta included, sees a local turn like any other.
--
-- execution_id is the shape the /llm edge accepts and the runtime mints: 32
-- lowercase hex characters. It cannot collide with an execution_jobs id in
-- practice (both are 128 random bits), and the verifier reads both tables.
--
-- NO PERMISSION: the routes gate on `models.chat.messages.create`, which 0068
-- already grants. IDEMPOTENT. No BEGIN/COMMIT: the ledgered runner wraps each
-- file.
CREATE TABLE IF NOT EXISTS elitea_runtime.local_turn_executions (
    execution_id TEXT PRIMARY KEY,
    -- ON DELETE CASCADE: a local turn is nothing without its project, and
    -- the project delete (projectprovisioning.referencingDeletes) also clears
    -- it explicitly, so the delete names what it removes.
    project_id INTEGER NOT NULL REFERENCES centry.project(id) ON DELETE CASCADE,
    actor_id TEXT NOT NULL,
    token_id TEXT NOT NULL,
    native_client_id TEXT NOT NULL DEFAULT '',
    conversation_uuid UUID NOT NULL,
    question_id UUID NOT NULL,
    response_message_id UUID NOT NULL,
    target_participant_id INTEGER NOT NULL,
    -- The agent version the turn runs, PINNED at start: the answering
    -- participant's application and mapped version at that moment (NULL for a
    -- model turn, or an agent of another project). A remote toolkit call is
    -- authorized against this version, so switching the participant's version
    -- mid-turn does not change what the running turn may call.
    application_id INTEGER,
    version_id INTEGER,
    memories_used INTEGER NOT NULL DEFAULT 0,
    started_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    expires_at TIMESTAMPTZ NOT NULL,
    committed_at TIMESTAMPTZ,
    commit_digest BYTEA,
    CONSTRAINT local_turn_executions_id_shape
        CHECK (execution_id ~ '^[0-9a-f]{32}$'),
    CONSTRAINT local_turn_executions_actor
        CHECK (actor_id ~ '^[1-9][0-9]{0,18}$'),
    CONSTRAINT local_turn_executions_agent
        CHECK ((application_id IS NULL) = (version_id IS NULL)
               AND (application_id IS NULL OR (application_id > 0 AND version_id > 0))),
    CONSTRAINT local_turn_executions_deadline
        CHECK (expires_at > started_at),
    CONSTRAINT local_turn_executions_memories
        CHECK (memories_used >= 0),
    CONSTRAINT local_turn_executions_commit
        CHECK ((committed_at IS NULL) = (commit_digest IS NULL)),
    CONSTRAINT local_turn_executions_commit_digest
        CHECK (commit_digest IS NULL OR octet_length(commit_digest) = 32),
    -- One execution per question of one caller in one project: a retried
    -- start replays it rather than opening a second one.
    CONSTRAINT local_turn_executions_question
        UNIQUE (project_id, actor_id, question_id)
);

-- The /llm attribution check reads by (execution_id, project_id, actor_id);
-- the primary key serves it. This index serves an operator or an analytics
-- read of one caller's local turns in a project, newest first.
CREATE INDEX IF NOT EXISTS local_turn_executions_actor_started_idx
    ON elitea_runtime.local_turn_executions (project_id, actor_id, started_at DESC);

-- The sensitive-tool confirmations a turn's remote toolkit calls consumed
-- (ADR-0029 decision 5b, localturn/confirmation.go). A confirmation is bound
-- to one call: its interrupt_id derives from call_digest (the turn, toolkit,
-- agent version, tool and arguments digest) and sequence (how many
-- confirmations of that call the turn consumed before), and the server
-- recomputes it from the call it receives. It is SINGLE USE: the primary key
-- refuses a second consumption. A retry of the consuming request (the same
-- idempotency_key) is the same call and is accepted; any other request
-- presenting the id is not. Rows go with their turn.
CREATE TABLE IF NOT EXISTS elitea_runtime.local_turn_confirmations (
    execution_id TEXT NOT NULL
        REFERENCES elitea_runtime.local_turn_executions(execution_id) ON DELETE CASCADE,
    interrupt_id TEXT NOT NULL,
    call_digest TEXT NOT NULL,
    sequence INTEGER NOT NULL,
    idempotency_key TEXT NOT NULL,
    consumed_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    PRIMARY KEY (execution_id, interrupt_id),
    CONSTRAINT local_turn_confirmations_call_sequence
        UNIQUE (execution_id, call_digest, sequence),
    CONSTRAINT local_turn_confirmations_call_shape
        CHECK (call_digest ~ '^[0-9a-f]{64}$' AND sequence >= 0),
    CONSTRAINT local_turn_confirmations_interrupt_shape
        CHECK (interrupt_id ~ '^hitl_[0-9a-f]{32}$'),
    CONSTRAINT local_turn_confirmations_key_shape
        CHECK (idempotency_key ~ '^[A-Za-z0-9_-]{1,128}$')
);
