-- 0205_run_steps.sql — the step records of a run (ADR-0042 decisions 1 and
-- 2, issue #1499).
--
--   elitea_runtime.run_step_heads
--       One row per run (execution): the chain head the persist path locks,
--       the run's tenant project, agent, user and conversation (for legal
--       hold, retention and the conversation's run list), and how far the
--       head has been anchored into the tenant's security audit chain.
--
--       anchor_requested_at is set when the run's settlement is prepared;
--       the main.run_step_anchor routine (every minute) anchors those heads
--       off the settlement path, and step_record's hourly pass anchors any
--       settled run a mark missed.
--
--   elitea_runtime.run_steps
--       One row per step the worker reported (ReportRunSteps). The worker's
--       step_id is the idempotency key (unique per run); Main assigns seq
--       (1, 2, 3 with no gap, in arrival order) and the chain:
--       hash = sha256(prev_hash || canonical step), where the canonical step
--       covers every column below except the content's object keys and its
--       expiry, and covers the content by digest, never by value
--       (internal/application/runevidence). The content itself is in object
--       storage (ADR-0016) under the platform-scoped `run-evidence` bucket,
--       key <project>/runs/<run>/<step_id>/<inputs|outputs>. The content is
--       written BEFORE the row's transaction, so a persist that fails (and
--       a worker that never retries) would leave an object no row names:
--       each key is first journalled in run_step_content_pending, committed
--       ahead of the write, and the persist transaction removes the entries
--       of the rows it stored. The step_content routine deletes the object
--       of an entry older than an hour whose step row does not name it, then
--       the entry (row-first journal, as #1245).
--
--       Retention: the step_content sweeper sets content_expired_at and
--       content_expired_policy, then deletes the objects and clears the
--       keys; the chain still verifies because it covers the digests. A
--       deleted conversation expires its runs' content the same way (policy
--       conversation_deleted). The step_record sweeper deletes whole runs
--       (head and steps). An UPDATE trigger keeps every chained column
--       immutable.
--
--       export_txid orders the elitea.audit.v1 export the way 0196 orders
--       centry.audit_events: the inserting transaction, then id.
--
-- Platform retention rows for step_content and step_record (keep forever,
-- project overrides allowed), and the permission runs.evidence.read for
-- super_admin and admin (administration mode) and the project admin
-- (default mode). 0060 returns early on an existing database, so the grants
-- are here and in 001_initial.sql.
--
-- Every statement is guarded or idempotent. No BEGIN/COMMIT: the ledgered
-- runner wraps the file.

CREATE SCHEMA IF NOT EXISTS elitea_runtime;

CREATE TABLE IF NOT EXISTS elitea_runtime.run_step_heads (
    run_id              text NOT NULL PRIMARY KEY CHECK (length(run_id) BETWEEN 1 AND 128),
    project_id          bigint NOT NULL CHECK (project_id > 0),
    application_id      bigint CHECK (application_id IS NULL OR application_id > 0),
    actor_user_id       bigint CHECK (actor_user_id IS NULL OR actor_user_id > 0),
    conversation_uuid   uuid,
    -- The chat response message the run produced (agent_execution_jobs.
    -- client_message_id), so a reader can go from a message to its run.
    message_id          text CHECK (message_id IS NULL OR length(message_id) BETWEEN 1 AND 512),
    last_seq            bigint NOT NULL DEFAULT 0 CHECK (last_seq >= 0),
    last_hash           bytea NOT NULL DEFAULT decode(repeat('00', 32), 'hex')
                        CHECK (octet_length(last_hash) = 32),
    anchored_seq        bigint NOT NULL DEFAULT 0 CHECK (anchored_seq >= 0 AND anchored_seq <= last_seq),
    anchored_at         timestamptz,
    anchor_requested_at timestamptz,
    created_at          timestamptz NOT NULL DEFAULT clock_timestamp(),
    updated_at          timestamptz NOT NULL DEFAULT clock_timestamp()
);

-- The step_record sweep reads the runs of one project by age.
CREATE INDEX IF NOT EXISTS run_step_heads_project_age
    ON elitea_runtime.run_step_heads (project_id, updated_at);
-- A conversation's runs.
CREATE INDEX IF NOT EXISTS run_step_heads_conversation
    ON elitea_runtime.run_step_heads (project_id, conversation_uuid)
    WHERE conversation_uuid IS NOT NULL;
-- Settled runs not yet anchored (the anchoring backstop).
CREATE INDEX IF NOT EXISTS run_step_heads_unanchored
    ON elitea_runtime.run_step_heads (updated_at)
    WHERE anchored_seq < last_seq;
-- Settled runs waiting for main.run_step_anchor.
CREATE INDEX IF NOT EXISTS run_step_heads_anchor_requested
    ON elitea_runtime.run_step_heads (anchor_requested_at)
    WHERE anchor_requested_at IS NOT NULL AND anchored_seq < last_seq;

-- The row-first journal of step content written ahead of its row.
CREATE TABLE IF NOT EXISTS elitea_runtime.run_step_content_pending (
    object_key  text NOT NULL PRIMARY KEY CHECK (length(object_key) BETWEEN 1 AND 1024),
    run_id      text NOT NULL CHECK (length(run_id) BETWEEN 1 AND 128),
    step_id     uuid NOT NULL,
    project_id  bigint NOT NULL CHECK (project_id > 0),
    created_at  timestamptz NOT NULL DEFAULT clock_timestamp()
);
CREATE INDEX IF NOT EXISTS run_step_content_pending_age
    ON elitea_runtime.run_step_content_pending (created_at);

CREATE TABLE IF NOT EXISTS elitea_runtime.run_steps (
    id                      bigserial PRIMARY KEY,
    run_id                  text NOT NULL CHECK (length(run_id) BETWEEN 1 AND 128),
    seq                     bigint NOT NULL CHECK (seq > 0),
    step_id                 uuid NOT NULL,
    generation              bigint NOT NULL CHECK (generation > 0),
    project_id              bigint NOT NULL CHECK (project_id > 0),
    application_id          bigint CHECK (application_id IS NULL OR application_id > 0),
    release_id              bigint CHECK (release_id IS NULL OR release_id > 0),
    definition_digest       bytea CHECK (definition_digest IS NULL OR octet_length(definition_digest) = 32),
    kind                    text NOT NULL CHECK (kind IN (
                                'model_call', 'tool_call', 'retrieval', 'approval',
                                'delegation', 'cap', 'error', 'output')),
    -- "agent:<binding uuid>" for a governed run with an active agent
    -- identity binding, else "user:<user id>".
    actor                   text NOT NULL CHECK (length(actor) BETWEEN 1 AND 256),
    actor_user_id           bigint CHECK (actor_user_id IS NULL OR actor_user_id > 0),
    conversation_uuid       uuid,
    node_id                 text NOT NULL DEFAULT '' CHECK (length(node_id) <= 256),
    name                    text NOT NULL DEFAULT '' CHECK (length(name) <= 512),
    toolkit_id              text NOT NULL DEFAULT '' CHECK (length(toolkit_id) <= 128),
    started_at              timestamptz NOT NULL,
    ended_at                timestamptz NOT NULL,
    summary                 jsonb NOT NULL DEFAULT '{}'::jsonb
                            CHECK (jsonb_typeof(summary) = 'object'),
    retrieval               jsonb CHECK (retrieval IS NULL OR jsonb_typeof(retrieval) = 'object'),
    cited_step_ids          text[] NOT NULL DEFAULT '{}',
    cited_document_ids      text[] NOT NULL DEFAULT '{}',
    -- One content side each: the SHA-256 of the stored bytes, the stored
    -- size, the size before the worker cut it, whether it was cut, whether
    -- evidence_fields filtered it, and the object key while it is stored.
    -- No digest means the side was empty (or filtered to nothing).
    inputs_digest           bytea CHECK (inputs_digest IS NULL OR octet_length(inputs_digest) = 32),
    inputs_size             bigint NOT NULL DEFAULT 0 CHECK (inputs_size >= 0),
    inputs_original_size    bigint NOT NULL DEFAULT 0 CHECK (inputs_original_size >= 0),
    inputs_truncated        boolean NOT NULL DEFAULT false,
    inputs_filtered         boolean NOT NULL DEFAULT false,
    inputs_media_type       text NOT NULL DEFAULT '' CHECK (inputs_media_type IN ('', 'application/json', 'text/plain')),
    inputs_object_key       text CHECK (inputs_object_key IS NULL OR inputs_digest IS NOT NULL),
    outputs_digest          bytea CHECK (outputs_digest IS NULL OR octet_length(outputs_digest) = 32),
    outputs_size            bigint NOT NULL DEFAULT 0 CHECK (outputs_size >= 0),
    outputs_original_size   bigint NOT NULL DEFAULT 0 CHECK (outputs_original_size >= 0),
    outputs_truncated       boolean NOT NULL DEFAULT false,
    outputs_filtered        boolean NOT NULL DEFAULT false,
    outputs_media_type      text NOT NULL DEFAULT '' CHECK (outputs_media_type IN ('', 'application/json', 'text/plain')),
    outputs_object_key      text CHECK (outputs_object_key IS NULL OR outputs_digest IS NOT NULL),
    content_expired_at      timestamptz,
    content_expired_policy  text CHECK (content_expired_policy IS NULL OR length(content_expired_policy) BETWEEN 1 AND 128),
    prev_hash               bytea NOT NULL CHECK (octet_length(prev_hash) = 32),
    hash                    bytea NOT NULL CHECK (octet_length(hash) = 32),
    recorded_at             timestamptz NOT NULL DEFAULT clock_timestamp(),
    export_txid             bigint NOT NULL DEFAULT (pg_current_xact_id()::text::bigint),
    CONSTRAINT run_steps_seq_key UNIQUE (run_id, seq),
    CONSTRAINT run_steps_step_key UNIQUE (run_id, step_id),
    CONSTRAINT run_steps_time_order CHECK (ended_at >= started_at),
    CONSTRAINT run_steps_expiry_pair CHECK ((content_expired_at IS NULL) = (content_expired_policy IS NULL))
);

-- The elitea.audit.v1 export order.
CREATE INDEX IF NOT EXISTS run_steps_export_order
    ON elitea_runtime.run_steps (export_txid, id);
-- The step_content sweep: stored content of one project by age.
CREATE INDEX IF NOT EXISTS run_steps_content_age
    ON elitea_runtime.run_steps (project_id, recorded_at)
    WHERE content_expired_at IS NULL
      AND (inputs_object_key IS NOT NULL OR outputs_object_key IS NOT NULL);
-- Expired content whose objects are not deleted yet (the purge drain).
CREATE INDEX IF NOT EXISTS run_steps_content_purge
    ON elitea_runtime.run_steps (content_expired_at, id)
    WHERE content_expired_at IS NOT NULL
      AND (inputs_object_key IS NOT NULL OR outputs_object_key IS NOT NULL);
-- A conversation's steps (conversation deletion).
CREATE INDEX IF NOT EXISTS run_steps_conversation
    ON elitea_runtime.run_steps (project_id, conversation_uuid)
    WHERE conversation_uuid IS NOT NULL;

-- Append-only: only the content's storage state may change (its expiry and
-- object keys, which the chain does not cover).
CREATE OR REPLACE FUNCTION elitea_runtime.run_steps_chained_columns_immutable()
RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF (NEW.id, NEW.run_id, NEW.seq, NEW.step_id, NEW.generation, NEW.project_id,
        NEW.application_id, NEW.release_id, NEW.definition_digest, NEW.kind, NEW.actor,
        NEW.actor_user_id, NEW.conversation_uuid, NEW.node_id, NEW.name, NEW.toolkit_id,
        NEW.started_at, NEW.ended_at, NEW.summary, NEW.retrieval, NEW.cited_step_ids,
        NEW.cited_document_ids, NEW.inputs_digest, NEW.inputs_size, NEW.inputs_original_size,
        NEW.inputs_truncated, NEW.inputs_filtered, NEW.inputs_media_type, NEW.outputs_digest,
        NEW.outputs_size, NEW.outputs_original_size, NEW.outputs_truncated, NEW.outputs_filtered,
        NEW.outputs_media_type, NEW.prev_hash, NEW.hash, NEW.recorded_at, NEW.export_txid)
       IS DISTINCT FROM
       (OLD.id, OLD.run_id, OLD.seq, OLD.step_id, OLD.generation, OLD.project_id,
        OLD.application_id, OLD.release_id, OLD.definition_digest, OLD.kind, OLD.actor,
        OLD.actor_user_id, OLD.conversation_uuid, OLD.node_id, OLD.name, OLD.toolkit_id,
        OLD.started_at, OLD.ended_at, OLD.summary, OLD.retrieval, OLD.cited_step_ids,
        OLD.cited_document_ids, OLD.inputs_digest, OLD.inputs_size, OLD.inputs_original_size,
        OLD.inputs_truncated, OLD.inputs_filtered, OLD.inputs_media_type, OLD.outputs_digest,
        OLD.outputs_size, OLD.outputs_original_size, OLD.outputs_truncated, OLD.outputs_filtered,
        OLD.outputs_media_type, OLD.prev_hash, OLD.hash, OLD.recorded_at, OLD.export_txid) THEN
        RAISE EXCEPTION 'run step records are append-only';
    END IF;
    RETURN NEW;
END
$$;

DROP TRIGGER IF EXISTS run_steps_append_only ON elitea_runtime.run_steps;
CREATE TRIGGER run_steps_append_only
    BEFORE UPDATE ON elitea_runtime.run_steps
    FOR EACH ROW EXECUTE FUNCTION elitea_runtime.run_steps_chained_columns_immutable();

DO $$
BEGIN
    IF to_regclass('elitea_runtime.retention_policy') IS NULL THEN
        RAISE NOTICE '0205: retention_policy absent, no step retention rows';
        RETURN;
    END IF;
    INSERT INTO elitea_runtime.retention_policy
        (data_class, scope, min_age_days, max_age_days, allow_project_override)
    VALUES
        ('step_content', 'platform', 0, NULL, true),
        ('step_record',  'platform', 0, NULL, true)
    ON CONFLICT (data_class) WHERE scope = 'platform' DO NOTHING;
END
$$;

DO $$
BEGIN
    IF to_regclass('public.auth_core__role') IS NULL
       OR to_regclass('public.auth_core__role_permission') IS NULL THEN
        RAISE NOTICE '0205: auth_core tables absent, nothing to grant';
        RETURN;
    END IF;

    -- One INSERT per mode, with the mode spelled as a literal: the router's
    -- grant gate (router_permission_grant_gate_test.go) reads the grants of
    -- this directory per `mode = '...'` block.
    INSERT INTO public.auth_core__role_permission (role_id, permission)
    SELECT role.id, 'runs.evidence.read'
    FROM public.auth_core__role AS role
    WHERE role.mode = 'administration' AND role.name IN ('super_admin', 'admin')
    ON CONFLICT (role_id, permission) DO NOTHING;

    INSERT INTO public.auth_core__role_permission (role_id, permission)
    SELECT role.id, 'runs.evidence.read'
    FROM public.auth_core__role AS role
    WHERE role.mode = 'default' AND role.name = 'admin'
    ON CONFLICT (role_id, permission) DO NOTHING;

    -- legacyrbac reads the central grants only for a caller with no
    -- per-project rows (shared/0090). A project whose matrix was saved gets
    -- the string on its `admin` role here, as 0204 does.
    IF to_regclass('public.auth_core__project_role') IS NULL
       OR to_regclass('public.auth_core__project_role_permission') IS NULL THEN
        RETURN;
    END IF;

    INSERT INTO public.auth_core__project_role_permission (project_id, role_id, permission)
    SELECT DISTINCT saved.project_id, saved.role_id, 'runs.evidence.read'
    FROM (
        SELECT DISTINCT project_id, role_id
        FROM public.auth_core__project_role_permission
        WHERE role_id IS NOT NULL
    ) AS saved
    JOIN public.auth_core__project_role AS project_role
      ON project_role.id = saved.role_id
     AND project_role.project_id = saved.project_id
    WHERE project_role.name = 'admin'
    ON CONFLICT (project_id, role_id, permission) DO NOTHING;
END
$$;
