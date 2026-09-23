-- 0125_token_lifecycle.sql — the two facts a personal access token needs to
-- have its expiry announced once, and to no one else (issue #940 A3,
-- ELITEA-0750/0751/0752/0754/0755/0756).
--
-- WHAT THIS ADDS. One side-table row per access token, carrying:
--
--   issued_at            — when THIS platform minted the token. It is the only
--                          way to answer ELITEA-0755, "no notification for a
--                          token whose TOTAL lifetime is 24 hours or less": a
--                          12-hour key is inside the 24-hour warning window
--                          from the moment it exists, and warning its owner
--                          the instant they create it is noise, not a warning.
--   notified_for_expires — the `expires` value an expiry notice has already
--                          been produced for. This is the dedupe marker
--                          (ELITEA-0754): a second sweep sees its own mark and
--                          produces nothing. It stores the EXPIRES rather than
--                          a boolean so the mark can never outlive the fact it
--                          was made about.
--   notified_at          — when that notice was produced, for an operator
--                          reading the table directly.
--
-- WHY A SIDE TABLE, NOT TWO COLUMNS ON auth_core__token. `public.auth_core__token`
-- belongs to pylon's `auth_core` plugin, and this corpus has never issued DDL
-- against it — 0071_token_project_binding.sql makes the same argument at
-- length and takes the same shape for the same reason. Two migration systems
-- owning one table definition is the failure 0064 and 0065 each describe from
-- the other side.
--
-- WHY NOT DEDUPE ON THE NOTIFICATION ROW. centry.notifications.uuid is UNIQUE
-- (0065) and the artifact-bucket producer leans on `ON CONFLICT (uuid) DO
-- NOTHING` with a deterministic UUID. That is idempotent for a RETRIED tick
-- and nothing more: a user who DELETES the notification gets it again on the
-- next sweep, which for a 15-minute cadence over a 24-hour window is the same
-- notification up to ninety-six times. The mark has to live somewhere the user
-- cannot delete, so it lives here.
--
-- NO BACKFILL, AND WHAT THAT MEANS. A token that existed before this migration
-- — or one pylon minted — has no row, so `issued_at` is unknown. Unknown is
-- treated as "old enough": its owner still gets the expiry warning. The
-- alternative, defaulting issued_at to the migration's own clock, would make
-- every pre-existing key look newly minted and SUPPRESS the warning for the
-- whole population this feature exists for.
--
-- THE FOREIGN KEY IS GUARDED, for the reason 0071 states: elitea-migrate can
-- run before `001_initial.sql` creates auth_core, and a bare REFERENCES clause
-- raises there. The table is created either way — the sweep LEFT JOINs it and
-- a missing relation is 42P01 on every tick.
--
-- IDEMPOTENT throughout. No BEGIN/COMMIT: the ledgered runner executes each
-- file inside one transaction with its ledger row (migrate/runner.go apply).

CREATE SCHEMA IF NOT EXISTS elitea_identity;

CREATE TABLE IF NOT EXISTS elitea_identity.token_lifecycle (
    -- One row per token: the PRIMARY KEY is what makes the creating upsert and
    -- the sweeper's mark address the same row.
    token_id             integer PRIMARY KEY,
    issued_at            timestamptz,
    notified_for_expires timestamp,
    notified_at          timestamptz
);

-- The sweep reads "which tokens have already been told about THIS expiry", so
-- the join is on token_id and the PRIMARY KEY already serves it. This index
-- serves the other direction: an operator (or a future digest) asking which
-- notices went out recently.
CREATE INDEX IF NOT EXISTS ix_token_lifecycle_notified_at
    ON elitea_identity.token_lifecycle (notified_at);

DO $$
BEGIN
    IF to_regclass('public.auth_core__token') IS NULL THEN
        RAISE NOTICE '0125: auth_core absent, token lifecycle foreign key skipped';
        RETURN;
    END IF;
    IF EXISTS (
        SELECT 1
        FROM pg_constraint
        WHERE conname = 'token_lifecycle_token_id_fkey'
          AND conrelid = 'elitea_identity.token_lifecycle'::regclass
    ) THEN
        RETURN;
    END IF;
    ALTER TABLE elitea_identity.token_lifecycle
        ADD CONSTRAINT token_lifecycle_token_id_fkey
        FOREIGN KEY (token_id)
        REFERENCES public.auth_core__token (id)
        ON DELETE CASCADE;
END
$$;
