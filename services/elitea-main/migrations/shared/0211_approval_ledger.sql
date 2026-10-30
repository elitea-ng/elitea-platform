-- 0211_approval_ledger.sql — the approval decision ledger and unattended
-- runs (ADR-0041 decision 5, issue #1491).
--
--   elitea_runtime.approval_ledger
--       One row per approval event: a pause (a tool or a node that waits for
--       a person), a decision (approved, rejected, edited, answered), a
--       timeout, a close (a Stop, a regenerate or a new turn) and each
--       unattended outcome. Every path that decides an approval writes its
--       row in the SAME transaction as the state change, and mirrors the row
--       to the security audit outbox (0183) as
--       ai.elitea.security.v1.agent.approval with action agent.approval.<outcome>.
--       Three paths raise and decide approvals: the root chat pause
--       (chat_message_group.meta.hitl_interrupts, which is now the UI
--       projection of these rows), the per-interrupt ledger of 0157 (fan-out
--       cards) and the desktop remote toolkit confirmation (0155).
--
--       A row holds references and digests only: the tool and toolkit
--       names, and argument_digest, the SHA-256 of the canonical arguments.
--       It never holds the arguments, a comment or an answer.
--
--       phase 'pause'    the wait started (outcome paused).
--       phase 'notice'   an unattended run that waits for its owner
--                        (outcome unattended_waiting).
--       phase 'decision' how the wait ended. At most one per wait: the
--                        partial unique index is the database half of "two
--                        approvers racing get one winner".
--
--       Append-only: an UPDATE raises an error. The rows of a deprovisioned
--       project may be deleted with it; the audit chain keeps the events.
--
--   elitea_runtime.unattended_run_policies
--       What a schedule or trigger run does when a step needs an approval
--       and no person is there (on_approval_required: fail, skip or
--       notify_and_wait). One row per run conversation, written by the
--       schedule or trigger admission before the run starts. It is a
--       snapshot: a later change of the schedule does not change a run in
--       flight. "Auto-approve" is not a value.
--
--   elitea_runtime.unattended_approval_settings
--       The per-schedule and per-trigger choice (target schedule|trigger,
--       keyed by the pipeline version, as both are). No row means the
--       platform default, approvals.unattended_default, whose default is
--       fail.
--
--   elitea_runtime.unattended_approval_actions
--       The durable intent of skip and notify_and_wait: one row per waiting
--       pause. A skip row is QUEUED until Main admits the continuation that
--       answers the pause with a refusal (the model reads that the step was
--       skipped); a wait row is WAITING until a person decides or it
--       expires (expires_at), when Main closes the pause as timed_out.
--
--   execution_jobs.cancel_reason gains approval_required_unattended and
--   approval_timed_out: an unattended fan-out run that Main stops for a
--   missing approver shows the typed error instead of a quiet stop.
--
-- No permission: the ledger is read through the audit trail export, which
-- has its own grants. No backfill: a pause open at upgrade has no pause row,
-- and its decision still writes its decision row. Idempotent throughout. No
-- BEGIN/COMMIT: the ledgered runner wraps the file.

CREATE SCHEMA IF NOT EXISTS elitea_runtime;

CREATE TABLE IF NOT EXISTS elitea_runtime.approval_ledger (
    id                  bigserial PRIMARY KEY,
    project_id          bigint NOT NULL CHECK (project_id > 0),
    -- The execution that paused (the local turn for the desktop path).
    execution_id        text NOT NULL CHECK (length(execution_id) BETWEEN 1 AND 128),
    -- The chat response the pause is shown on. NULL when the path has none.
    response_message_id text CHECK (response_message_id IS NULL OR length(response_message_id) BETWEEN 1 AND 128),
    conversation_uuid   uuid,
    interrupt_id        text NOT NULL
                        CHECK (octet_length(interrupt_id) BETWEEN 1 AND 512 AND interrupt_id !~ '[\x00-\x1f]'),
    -- The 0157 card key on the interrupt path.
    interrupt_key       text CHECK (interrupt_key IS NULL OR interrupt_key ~ '^[0-9a-f]{64}$'),
    path                text NOT NULL CHECK (path IN ('root', 'interrupt', 'desktop')),
    phase               text NOT NULL CHECK (phase IN ('pause', 'notice', 'decision')),
    outcome             text NOT NULL CHECK (outcome IN (
                            'paused', 'unattended_waiting',
                            'approved', 'rejected', 'edited', 'answered', 'other',
                            'timed_out', 'cancelled', 'superseded',
                            'unattended_failed', 'unattended_skipped')),
    -- The action the decider chose, from the pause's available actions.
    action              text CHECK (action IS NULL OR action ~ '^[a-z_]{1,32}$'),
    -- A person decided (user) or Main did (system: an unattended outcome, a
    -- timeout, a run that ended). A system row names no person.
    actor_kind          text NOT NULL CHECK (actor_kind IN ('user', 'system')),
    actor_user_id       bigint CHECK (actor_user_id IS NULL OR actor_user_id > 0),
    trigger_origin      text NOT NULL DEFAULT 'manual'
                        CHECK (trigger_origin IN ('manual', 'api', 'schedule', 'webhook', 'index')),
    guardrail_type      text CHECK (guardrail_type IS NULL OR length(guardrail_type) BETWEEN 1 AND 64),
    tool_name           text CHECK (tool_name IS NULL OR length(tool_name) BETWEEN 1 AND 512),
    toolkit_name        text CHECK (toolkit_name IS NULL OR length(toolkit_name) BETWEEN 1 AND 512),
    toolkit_type        text CHECK (toolkit_type IS NULL OR length(toolkit_type) BETWEEN 1 AND 128),
    argument_digest     text CHECK (argument_digest IS NULL OR argument_digest ~ '^sha256:[0-9a-f]{64}$'),
    -- The stable reason code of a system outcome.
    reason              text CHECK (reason IS NULL OR reason ~ '^[a-z][a-z0-9_.]{0,63}$'),
    -- The mirrored security audit event (0183 outbox event_id).
    audit_event_id      uuid NOT NULL UNIQUE,
    created_at          timestamptz NOT NULL DEFAULT clock_timestamp(),
    CONSTRAINT approval_ledger_actor CHECK (
        (actor_kind = 'user' AND actor_user_id IS NOT NULL)
        OR (actor_kind = 'system' AND actor_user_id IS NULL)),
    CONSTRAINT approval_ledger_phase_outcome CHECK (
        (phase = 'pause' AND outcome = 'paused')
        OR (phase = 'notice' AND outcome = 'unattended_waiting')
        OR (phase = 'decision' AND outcome NOT IN ('paused', 'unattended_waiting')))
);

-- One pause, one notice and one decision per wait.
CREATE UNIQUE INDEX IF NOT EXISTS approval_ledger_one_per_phase
    ON elitea_runtime.approval_ledger (execution_id, interrupt_id, phase);
-- The audit trail reads a project's rows by time.
CREATE INDEX IF NOT EXISTS approval_ledger_project_time
    ON elitea_runtime.approval_ledger (project_id, created_at);
-- Stop, regenerate and the projection check read one response's rows.
CREATE INDEX IF NOT EXISTS approval_ledger_response
    ON elitea_runtime.approval_ledger (response_message_id)
    WHERE response_message_id IS NOT NULL;

CREATE OR REPLACE FUNCTION elitea_runtime.approval_ledger_append_only()
RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'elitea_runtime.approval_ledger is append-only';
END
$$;

DROP TRIGGER IF EXISTS approval_ledger_append_only ON elitea_runtime.approval_ledger;
CREATE TRIGGER approval_ledger_append_only
    BEFORE UPDATE ON elitea_runtime.approval_ledger
    FOR EACH ROW EXECUTE FUNCTION elitea_runtime.approval_ledger_append_only();

CREATE TABLE IF NOT EXISTS elitea_runtime.unattended_run_policies (
    project_id            bigint NOT NULL CHECK (project_id > 0),
    conversation_uuid     uuid NOT NULL,
    trigger_origin        text NOT NULL CHECK (trigger_origin IN ('schedule', 'webhook')),
    -- The schedule's or the trigger's pipeline version.
    version_id            bigint NOT NULL CHECK (version_id > 0),
    owner_user_id         bigint NOT NULL CHECK (owner_user_id > 0),
    on_approval_required  text NOT NULL CHECK (on_approval_required IN ('fail', 'skip', 'notify_and_wait')),
    created_at            timestamptz NOT NULL DEFAULT clock_timestamp(),
    PRIMARY KEY (project_id, conversation_uuid)
);

CREATE TABLE IF NOT EXISTS elitea_runtime.unattended_approval_settings (
    project_id            bigint NOT NULL CHECK (project_id > 0),
    target                text NOT NULL CHECK (target IN ('schedule', 'trigger')),
    version_id            bigint NOT NULL CHECK (version_id > 0),
    on_approval_required  text NOT NULL CHECK (on_approval_required IN ('fail', 'skip', 'notify_and_wait')),
    updated_by            bigint CHECK (updated_by IS NULL OR updated_by > 0),
    updated_at            timestamptz NOT NULL DEFAULT clock_timestamp(),
    PRIMARY KEY (project_id, target, version_id)
);

CREATE TABLE IF NOT EXISTS elitea_runtime.unattended_approval_actions (
    execution_id        text NOT NULL CHECK (length(execution_id) BETWEEN 1 AND 128),
    interrupt_id        text NOT NULL CHECK (octet_length(interrupt_id) BETWEEN 1 AND 512),
    project_id          bigint NOT NULL CHECK (project_id > 0),
    response_message_id text NOT NULL CHECK (length(response_message_id) BETWEEN 1 AND 128),
    conversation_uuid   uuid NOT NULL,
    path                text NOT NULL CHECK (path IN ('root', 'interrupt')),
    action              text NOT NULL CHECK (action IN ('skip', 'notify_and_wait')),
    owner_user_id       bigint NOT NULL CHECK (owner_user_id > 0),
    trigger_origin      text NOT NULL CHECK (trigger_origin IN ('schedule', 'webhook')),
    -- The refusal a skip submits: block_with_comment when the pause offers
    -- it, else reject.
    skip_action         text CHECK (skip_action IS NULL OR skip_action IN ('reject', 'block_with_comment')),
    state               text NOT NULL CHECK (state IN ('QUEUED', 'WAITING', 'DONE', 'EXPIRED', 'FAILED')),
    attempts            integer NOT NULL DEFAULT 0 CHECK (attempts BETWEEN 0 AND 1000),
    expires_at          timestamptz,
    failure_code        text CHECK (failure_code IS NULL OR failure_code ~ '^[a-z][a-z0-9_.]{0,63}$'),
    created_at          timestamptz NOT NULL DEFAULT clock_timestamp(),
    updated_at          timestamptz NOT NULL DEFAULT clock_timestamp(),
    PRIMARY KEY (execution_id, interrupt_id),
    CONSTRAINT unattended_approval_actions_shape CHECK (
        (action = 'skip' AND skip_action IS NOT NULL AND expires_at IS NULL)
        OR (action = 'notify_and_wait' AND skip_action IS NULL AND expires_at IS NOT NULL))
);

-- The sweep reads the open rows oldest first.
CREATE INDEX IF NOT EXISTS unattended_approval_actions_open
    ON elitea_runtime.unattended_approval_actions (state, updated_at)
    WHERE state IN ('QUEUED', 'WAITING');

DO $$
BEGIN
    ALTER TABLE elitea_runtime.execution_jobs
        DROP CONSTRAINT IF EXISTS execution_jobs_cancel_reason_known;
    ALTER TABLE elitea_runtime.execution_jobs
        ADD CONSTRAINT execution_jobs_cancel_reason_known
        CHECK (cancel_reason IS NULL OR cancel_reason IN (
            'cap_reached', 'approval_required_unattended', 'approval_timed_out'));
END
$$;
