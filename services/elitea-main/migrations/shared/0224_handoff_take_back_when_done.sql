-- 0224_handoff_take_back_when_done.sql — send a running task to the cloud
-- and take it back when done (ADR-0045 decision 8; issue #1439).
--
--   elitea_runtime.workspace_handoffs.take_back_when_done
--       The person asked, at send, that the owner's desktop take the thread
--       back by itself when its cloud run completes. Set from the complete
--       body; cleared by every transition to RECLAIMING or LOCAL (a take
--       back, a release, an expiry or an erase). Main never takes a thread
--       back by itself: the desktop reads the flag and the last cloud run
--       from the handoff state.
--
--   elitea_runtime.run_chain_links.kind
--       Adds handoff: the first cloud run of a chain that a desktop turn
--       started, when the send continues it (complete with `continue`).
--       The chain's origin becomes cloud in the same transaction, so its
--       run_finished names where the chain ended.
--
-- No backfill: an existing row keeps FALSE. No permission. Idempotent.
-- No BEGIN/COMMIT: the ledgered runner wraps the file.

ALTER TABLE elitea_runtime.workspace_handoffs
    ADD COLUMN IF NOT EXISTS take_back_when_done boolean NOT NULL DEFAULT FALSE;

DO $$
BEGIN
    ALTER TABLE elitea_runtime.run_chain_links
        DROP CONSTRAINT IF EXISTS run_chain_links_kind_check;
    ALTER TABLE elitea_runtime.run_chain_links
        ADD CONSTRAINT run_chain_links_kind_check CHECK (kind IN (
            'root', 'step_limit', 'output_limit', 'tool_call_cap', 'resume', 'continue', 'goal_not_met',
            'handoff'));
END
$$;

COMMENT ON COLUMN elitea_runtime.workspace_handoffs.take_back_when_done IS
    'The owner''s desktop takes the thread back by itself when its cloud run completes (ADR-0045 decision 8). Set at complete; cleared on RECLAIMING or LOCAL.';
