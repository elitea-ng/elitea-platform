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
-- ROLLING DEPLOY. A replica of the previous release claims with
--   UPDATE ... SET next_run_at = $3 WHERE id = $1 AND enabled
--                  AND next_run_at IS NOT DISTINCT FROM $2
-- which knows nothing of the lease. The BEFORE UPDATE trigger below refuses
-- (skips) any update that moves next_run_at while another holder's lease is
-- live and that touches neither the lease nor updated_at: that is exactly the
-- old claim and the old "plan the next run" write. A leased writer always
-- changes the lease (Release clears lease_expires_at), and an operator's edit
-- (Store.Update) always stamps updated_at, so both pass. What the guard cannot
-- give an old replica is a lease of its own: while old replicas still run,
-- a new replica may start alongside an old replica's long pass, as before this
-- migration (the passes are idempotent). Once the rollout ends, every claim is
-- leased.
--
-- A small table (one row per routine): the constant default does not rewrite
-- it. No data, no permission.
--
-- Idempotent. No BEGIN/COMMIT: the ledgered runner wraps the file.
ALTER TABLE elitea_runtime.platform_routines
    ADD COLUMN IF NOT EXISTS lease_token      bigint      NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS lease_holder     text,
    ADD COLUMN IF NOT EXISTS lease_expires_at timestamptz;

CREATE OR REPLACE FUNCTION elitea_runtime.platform_routines_lease_guard() RETURNS trigger
LANGUAGE plpgsql AS $body$
BEGIN
    IF OLD.lease_expires_at IS NOT NULL
       AND OLD.lease_expires_at > clock_timestamp()
       AND NEW.next_run_at IS DISTINCT FROM OLD.next_run_at
       AND NEW.lease_token = OLD.lease_token
       AND NEW.lease_holder IS NOT DISTINCT FROM OLD.lease_holder
       AND NEW.lease_expires_at IS NOT DISTINCT FROM OLD.lease_expires_at
       AND NEW.updated_at = OLD.updated_at THEN
        RETURN NULL;
    END IF;
    RETURN NEW;
END
$body$;

DROP TRIGGER IF EXISTS platform_routines_lease_guard ON elitea_runtime.platform_routines;
CREATE TRIGGER platform_routines_lease_guard BEFORE UPDATE ON elitea_runtime.platform_routines
    FOR EACH ROW EXECUTE FUNCTION elitea_runtime.platform_routines_lease_guard();
