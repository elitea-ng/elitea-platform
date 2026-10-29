-- 0197_command_outbox_refusal.sql — a command Main refused to prepare
-- (ADR-0044 decision 2, issue #1490).
--
--   elitea_runtime.command_outbox.refusal_code / refusal_message
--                             set when Main can never prepare the command as
--                             it stands: today only EGRESS_POLICY_INVALID, a
--                             worker egress allowlist that cannot be
--                             assembled (a malformed platform or project
--                             entry, a list over its bounds, an input the
--                             command names that is missing). The same
--                             statement moves the row's deadline to now, so
--                             the existing no-authority deadline retirement
--                             settles the run at its next pass with every
--                             command type's own terminal projection (index
--                             metadata, notifications, task status), and no
--                             retirement or terminal error code is added. The
--                             retirement writes the refusal, not the deadline
--                             text, as the run's `execution.failed` event:
--                             {"code": refusal_code, "safe_message":
--                             refusal_message, "retryable": false}. The
--                             message says what is wrong and who fixes it; it
--                             names an administrator's allowlist entry, never
--                             a credential or a setting's value.
--
-- A refusal is only ever recorded on a row that was never prepared: a
-- prepared command is published as prepared, and never assembled again.
-- Nullable columns with no default: a catalogue-only change, no rewrite. The
-- CHECK is added NOT VALID, so the ALTER does not scan the outbox under its
-- ACCESS EXCLUSIVE lock; PostgreSQL still enforces it for every new and
-- updated row, and every existing row holds NULL in both columns. No data,
-- no permission.

ALTER TABLE elitea_runtime.command_outbox
    ADD COLUMN IF NOT EXISTS refusal_code TEXT,
    ADD COLUMN IF NOT EXISTS refusal_message TEXT;

DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint
        WHERE conname = 'command_outbox_refusal'
          AND conrelid = 'elitea_runtime.command_outbox'::regclass
    ) THEN
        ALTER TABLE elitea_runtime.command_outbox
            ADD CONSTRAINT command_outbox_refusal CHECK (
                (refusal_code IS NULL AND refusal_message IS NULL)
                OR (
                    refusal_code = 'EGRESS_POLICY_INVALID'
                    AND octet_length(refusal_message) BETWEEN 1 AND 1024
                    AND prepared_at IS NULL
                    AND authority_granted_at IS NULL
                )
            ) NOT VALID;
    END IF;
END $$;
