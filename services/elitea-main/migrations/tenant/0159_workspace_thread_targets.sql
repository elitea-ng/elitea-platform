-- Schedules and webhook triggers that target a handed-off workspace thread
-- (ADR-0045 decision 9, issue 1440).
--
-- A pipeline schedule and an inbound trigger can now start a cloud turn on a
-- Local work thread that was handed off to the cloud executor, in place of a
-- new pipeline or agent run. The schedule or the trigger stays on its
-- version route: the version is where the setting lives and what a mandate
-- binds. The thread, the prompt and the grant are stored on the row.
--
--   * `pipeline_schedules.target_kind` — `pipeline` (every existing row) or
--     `workspace_thread`. A trigger already has the column (0142); it gains
--     the value `workspace_thread`.
--   * `target_conversation_uuid` — the thread (a conversation of this
--     project). Set only for `workspace_thread`.
--   * `target_prompt` — the person's message each occurrence starts with,
--     1 to 32768 characters. Set only for `workspace_thread`.
--   * `target_grant` — the `continue` and `chain` parts of the unattended
--     grant, and `goal_check` (the wire form of ChatUnattendedRequest). NULL
--     is the default grant of ADR-0045 decision 10.
--   * `pipeline_triggers.last_execution_id` — the execution the trigger's
--     last admitted call started. The schedule table has the column (0133).
--   * The schedule outcome vocabulary gains `skipped_thread_busy`: the
--     thread was not CLOUD_IDLE when the occurrence fired, so no turn
--     started. It is the one exception to 0133's no-stamp rule: it stamps
--     `last_run` and uses up the occurrence, so the next attempt is the next
--     cron time. A busy thread does not fire every minute, and a thread that
--     is idle again does not get a turn off the schedule's cadence.
--
-- No data and no permission. Every existing row keeps its kind and NULL
-- target columns. Same guard as 0138: a tenant schema that has not reached
-- 0133 has neither table here.
DO $$
BEGIN
    IF to_regclass('pipeline_schedules') IS NOT NULL THEN
        ALTER TABLE pipeline_schedules
            ADD COLUMN IF NOT EXISTS target_kind varchar(16) NOT NULL DEFAULT 'pipeline';
        ALTER TABLE pipeline_schedules ADD COLUMN IF NOT EXISTS target_conversation_uuid uuid NULL;
        ALTER TABLE pipeline_schedules ADD COLUMN IF NOT EXISTS target_prompt text NULL;
        ALTER TABLE pipeline_schedules ADD COLUMN IF NOT EXISTS target_grant jsonb NULL;

        ALTER TABLE pipeline_schedules DROP CONSTRAINT IF EXISTS pipeline_schedules_target_kind_check;
        ALTER TABLE pipeline_schedules
            ADD CONSTRAINT pipeline_schedules_target_kind_check
            CHECK (target_kind IN ('pipeline', 'workspace_thread'));

        -- A thread target names its thread and its prompt; a pipeline target
        -- names neither, so a kind change cannot leave a stale thread behind.
        ALTER TABLE pipeline_schedules DROP CONSTRAINT IF EXISTS pipeline_schedules_target_check;
        ALTER TABLE pipeline_schedules
            ADD CONSTRAINT pipeline_schedules_target_check
            CHECK (
                (target_kind = 'workspace_thread'
                    AND target_conversation_uuid IS NOT NULL
                    AND target_prompt IS NOT NULL
                    AND char_length(target_prompt) BETWEEN 1 AND 32768)
                OR (target_kind = 'pipeline'
                    AND target_conversation_uuid IS NULL
                    AND target_prompt IS NULL
                    AND target_grant IS NULL)
            );

        ALTER TABLE pipeline_schedules DROP CONSTRAINT IF EXISTS pipeline_schedules_last_result_check;
        ALTER TABLE pipeline_schedules
            ADD CONSTRAINT pipeline_schedules_last_result_check
            CHECK (last_result IS NULL OR last_result IN (
                'dispatched',
                'skipped_overlap',
                'skipped_unauthorized',
                'skipped_missing_version',
                'skipped_mandate_required',
                'skipped_mandate_not_usable',
                'skipped_mandate_scope',
                'skipped_thread_busy',
                'failed'
            ));
    END IF;

    IF to_regclass('pipeline_triggers') IS NOT NULL THEN
        -- 0142 adds target_kind; a schema that has 0133 and not yet 0142
        -- gets it from 0142 first, in ledger order.
        ALTER TABLE pipeline_triggers ADD COLUMN IF NOT EXISTS target_conversation_uuid uuid NULL;
        ALTER TABLE pipeline_triggers ADD COLUMN IF NOT EXISTS target_prompt text NULL;
        ALTER TABLE pipeline_triggers ADD COLUMN IF NOT EXISTS target_grant jsonb NULL;
        ALTER TABLE pipeline_triggers ADD COLUMN IF NOT EXISTS last_execution_id varchar(64) NULL;

        ALTER TABLE pipeline_triggers DROP CONSTRAINT IF EXISTS pipeline_triggers_target_kind_check;
        ALTER TABLE pipeline_triggers
            ADD CONSTRAINT pipeline_triggers_target_kind_check
            CHECK (target_kind IN ('pipeline', 'agent', 'workspace_thread'));

        ALTER TABLE pipeline_triggers DROP CONSTRAINT IF EXISTS pipeline_triggers_target_check;
        ALTER TABLE pipeline_triggers
            ADD CONSTRAINT pipeline_triggers_target_check
            CHECK (
                (target_kind = 'workspace_thread'
                    AND target_conversation_uuid IS NOT NULL
                    AND target_prompt IS NOT NULL
                    AND char_length(target_prompt) BETWEEN 1 AND 32768)
                OR (target_kind <> 'workspace_thread'
                    AND target_conversation_uuid IS NULL
                    AND target_prompt IS NULL
                    AND target_grant IS NULL)
            );
    END IF;
END
$$;
