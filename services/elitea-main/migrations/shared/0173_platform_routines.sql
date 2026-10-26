-- 0173_platform_routines.sql — the registry of the Go platform's recurring
-- routines (#1369): one row per routine, shared by elitea-main's platform
-- scheduler and by elitea-scheduler's workers, and edited by the admin
-- Schedules tab.
--
--   id                    the routine's stable name (the platform scheduler's
--                         job id, or the worker's name)
--   owner                 the process that runs it: `main` or `scheduler`
--   default_schedule      the code's schedule (env / values), refreshed at every
--                         startup; a Go duration (`24h`) or a five-field cron
--   min_interval_seconds  the shortest gap an operator may configure; the
--                         retention sweeps cannot be driven into a busy loop
--   schedule_override     the operator's schedule, NULL while the default holds
--   enabled               an operator can switch a routine off
--   last_run_at/last_outcome/next_run_at
--                         what the runner last did and plans next
--   updated_by/updated_at the last operator edit
--
-- A routine registers itself, so the table starts empty and fills when each
-- owner first starts: a fresh install keeps its env/values defaults because no
-- override exists. Nothing is copied from the old cron table, whose rows named
-- functions nothing in this stack runs.
--
-- No permission: the admin routes reuse `configuration.scheduling.schedules.view`
-- and `.edit`, which 0082 already grants in administration mode.
--
-- Idempotent. No BEGIN/COMMIT: the ledgered runner wraps the file.
CREATE TABLE IF NOT EXISTS elitea_runtime.platform_routines (
    id                   text        PRIMARY KEY,
    owner                text        NOT NULL,
    description          text        NOT NULL DEFAULT '',
    default_schedule     text        NOT NULL,
    min_interval_seconds integer     NOT NULL DEFAULT 0,
    schedule_override    text,
    enabled              boolean     NOT NULL DEFAULT true,
    last_run_at          timestamptz,
    last_outcome         text,
    next_run_at          timestamptz,
    updated_by           text,
    updated_at           timestamptz NOT NULL DEFAULT clock_timestamp(),
    CONSTRAINT platform_routines_id_length
        CHECK (octet_length(id) BETWEEN 1 AND 128),
    CONSTRAINT platform_routines_owner
        CHECK (owner IN ('main', 'scheduler')),
    CONSTRAINT platform_routines_schedule_length
        CHECK (octet_length(default_schedule) BETWEEN 1 AND 64
               AND (schedule_override IS NULL OR octet_length(schedule_override) BETWEEN 1 AND 64)),
    CONSTRAINT platform_routines_min_interval
        CHECK (min_interval_seconds >= 0),
    CONSTRAINT platform_routines_outcome
        CHECK (last_outcome IS NULL OR last_outcome IN ('ok', 'error', 'maintenance'))
);
