-- 0176_workspace_handoff_rest_pending.sql — the desktop can take back the
-- cloud turns it left behind (#1187, ADR-0034 §4.6 "up to turn k").
--
-- A take back may apply the cloud turns only up to turn k. The release then
-- says how many turns it did not take, and the cloud copy keeps them for the
-- project's return grace: while the grace runs, the owner's desktop may take
-- the thread back again (LOCAL → RECLAIMING, the rest take back) and apply
-- the remaining turns.
--
--   rest_pending  TRUE on a released LOCAL row whose release left turns not
--                 taken, and on the RECLAIMING row of a rest take back. Every
--                 other transition clears it. A rest RECLAIMING row keeps the
--                 release's delete_after: when the grace runs out the sweeper
--                 returns it to LOCAL (never to CLOUD_IDLE) and the erasure
--                 proceeds.
--
-- No data: every existing row is FALSE, which is what it was.
ALTER TABLE elitea_runtime.workspace_handoffs
    ADD COLUMN rest_pending boolean NOT NULL DEFAULT FALSE;
