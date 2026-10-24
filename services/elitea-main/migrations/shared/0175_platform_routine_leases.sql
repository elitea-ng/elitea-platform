-- 0175_platform_routine_leases.sql — a routine pass holds a lease on its
-- registry row for as long as it runs (#1414).
--
-- Before this, a replica claimed a routine's slot by moving `next_run_at`, then
-- ran the pass. A pass longer than the interval left the slot due again while
-- it still ran, and a second replica could claim it and start the same routine
-- in parallel.
--
--   lease_token       a fencing token, incremented by every acquisition. The
--                     holder renews and releases only under the token it got,
--                     so a holder whose lease expired and was taken over can
--                     no longer write to the row.
--   lease_holder      the replica holding (or last holding) the lease, for an
--                     operator reading the table.
--   lease_expires_at  NULL when no pass runs. A pass renews it while it runs;
--                     a crashed holder stops renewing, the lease runs out, and
--                     another replica may take the routine over.
--
-- The times are the database's clock (clock_timestamp()), so replicas with
-- skewed clocks agree on whether a lease is live.
--
-- A small table (one row per routine): the constant default does not rewrite
-- it. No data, no permission.
--
-- Idempotent. No BEGIN/COMMIT: the ledgered runner wraps the file.
ALTER TABLE elitea_runtime.platform_routines
    ADD COLUMN IF NOT EXISTS lease_token      bigint      NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS lease_holder     text,
    ADD COLUMN IF NOT EXISTS lease_expires_at timestamptz;
