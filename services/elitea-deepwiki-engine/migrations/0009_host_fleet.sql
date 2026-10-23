-- The host fleet: fleet-wide stops and fleet-wide generation slots
-- (multi-replica host, #scaling).
--
-- The sub-application host (services/elitea-subapp-host) runs as more than
-- one replica over the invocation tables of 0002. Two facts lived only in
-- the memory of one replica, and this migration puts them in the database:
--
--   invocations.labels      the facts a deletion finds a run by (project,
--                           wiki). Before, a deletion stopped only the runs
--                           of the replica it landed on.
--   invocations.holds_slot  true for a wiki generation, which takes one of
--                           the fleet's generation slots until it ends.
--   host_replicas           one row for each live host replica: its slot
--                           count (MAX_PARALLEL_WORKERS), whether it accepts
--                           new work (false while it drains), and when it
--                           last proved it is alive.
--
-- The fleet capacity is the sum of the slots of the live replicas that
-- accept work. The host admits a generation only while the unfinished
-- holds_slot rows are fewer than that sum, and /slots reports the same two
-- numbers, so the UI and the admission agree.
--
-- Every statement is idempotent: a database that already has a column or
-- the table keeps it.

ALTER TABLE invocations
    ADD COLUMN IF NOT EXISTS labels jsonb NOT NULL DEFAULT '{}'::jsonb,
    ADD COLUMN IF NOT EXISTS holds_slot boolean NOT NULL DEFAULT false;

CREATE TABLE IF NOT EXISTS host_replicas (
    owner     text        PRIMARY KEY,
    slots     integer     NOT NULL,
    admitting boolean     NOT NULL DEFAULT true,
    seen_at   timestamptz NOT NULL DEFAULT now()
);
