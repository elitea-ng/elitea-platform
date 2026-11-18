-- 0254_delegation_migration.sql — the one-time migration of existing
-- compositions to delegation grants (ADR-0044 amendment 1, A1.3; delegation
-- epic 2156 story S6).
--
-- The migration walks every composition edge of the project (an agent that
-- attaches an agent or pipeline as a tool, and a pipeline whose Agent nodes
-- run an attached agent or pipeline) and, for each edge whose callee holds
-- more than its caller, writes a PENDING grant flagged `legacy` with origin
-- `migration` (tenant 0252). It never writes an approved grant: that would
-- turn a migration into a blanket approval nobody read, and bypass
-- maker-checker under the governed profile.
--
--   delegation_migration_runs   one row per finished run, keyed by the run's
--                    name. The startup job reads it so a restart or a second
--                    replica does not walk the project again, and the
--                    "Delegations awaiting review" page reads the counts. The
--                    row is written in the same transaction as the grants and
--                    the notifications, so a run either happened completely or
--                    not at all.
--
-- The partial index serves the admin page: pending and stale grants are the
-- queue, and they are few next to the history.
--
-- Guarded (idempotent): the runner pins search_path to the tenant schema.
-- No BEGIN/COMMIT.
CREATE TABLE IF NOT EXISTS delegation_migration_runs (
    name text PRIMARY KEY,
    edges integer NOT NULL DEFAULT 0,
    contained integer NOT NULL DEFAULT 0,
    created integer NOT NULL DEFAULT 0,
    notified integer NOT NULL DEFAULT 0,
    completed_at timestamptz NOT NULL DEFAULT clock_timestamp()
);

CREATE INDEX IF NOT EXISTS delegation_grants_queue_idx
    ON delegation_grants (state, legacy)
    WHERE state IN ('pending', 'stale');
