-- 0161_project_deletions_journal.sql — the cleanup journal of a project delete
-- (#1211).
--
-- WHY. A project delete removes resources in places one transaction cannot
-- span: the artifact bytes in object storage, the tenant schema (a DROP SCHEMA
-- that waits on every lock in it), and the per-project PgVector database and
-- role on another server. So the delete is split in two:
--
--   1. ONE TRANSACTION DECIDES. It locks the project row FOR UPDATE, refuses
--      (rolls back, nothing changed) while the project has non-terminal
--      execution_jobs, inserts a row here recording what is left to clean up,
--      REVOKES THE PROJECT'S IDENTITY (the vault, the system PAT and user, the
--      project roles and memberships, the token bindings: plain SQL in this
--      database, so they die atomically with the row), deletes every row that
--      references the project and the project row itself, and commits. Any
--      failure in it leaves the project exactly as it was.
--   2. THE SLOW AND EXTERNAL CLEANUP RUNS FROM THIS ROW: the artifact bytes, the
--      tenant schema, the PgVector database. Every step is idempotent and marks
--      itself done in `cleanup`. The run is detached from the request, under its
--      own deadline and holding the row's lease; the delete request waits for it
--      only for a short budget (HTTP 202 when some steps are still pending) and
--      the run goes on without it. A step that fails or is not reached leaves the
--      row incomplete, and a reconciler in elitea-main retries it with backoff
--      until `completed_at` is set.
--
-- The tenant ROWS are not part of step 2: the tenant tables' owner_id foreign
-- keys cascade from the project row, so they go in the decision (under a
-- lock_timeout and a statement_timeout). Only the empty schema object is dropped
-- by the journal.
--
-- After the commit nothing can create work for the project: execution_jobs
-- carries foreign keys to centry.project(id) on both resource_project_id and
-- projection_project_id, so an admission for a project with no row fails.
--
-- AN ID CAN COME BACK (explicit ids, a restored database). Every destructive
-- step first checks that no centry.project row exists for the id; if one does,
-- the row is closed as superseded (cleanup.superseded) and nothing is touched.
-- Provision refuses an id whose row here is incomplete.
--
-- A DELETE that finds no project row but finds leftovers (the tenant schema, live
-- buckets, the PgVector database) and no row here ADOPTS them: it inserts this
-- row and cleans up as usual.
--
-- COLUMNS.
--   cleanup          what the decision recorded, and per-step completion:
--                    {"had_vector_store": true, "vector_database": "project_42",
--                     "done": {"artifact_buckets": true, ...}}
--                    had_vector_store is the decision's probe; the PgVector drop
--                    runs whatever it says, and it only decides how a failed
--                    drop is reported. quiet_drop_failures counts the failed
--                    drops for a store nothing recorded (the journal gives up
--                    after a limit and notes it under `notes`); superseded marks
--                    a row closed because the id belongs to a live project.
--   attempts         cleanup runs so far (the delete's own run included). A run
--                    the process stopped is not counted.
--   last_error       the last run's failure, by step name only (no raw error:
--                    it can carry SQL or addresses), or "interrupted" for a run
--                    the process stopped.
--   next_attempt_at  backoff: the reconciler does not retry before this.
--   claimed_until    the lease of the run working the row (stamped with
--                    clock_timestamp(), and restamped when the run starts, so a
--                    decision that waited on a lock does not shorten it). The
--                    reconciler claims ONE row at a time in a short FOR UPDATE
--                    SKIP LOCKED transaction that stamps the lease, works it,
--                    then claims the next; it does not hold a connection or a
--                    lock while the steps run, and leases nothing it is not
--                    working. Released (NULL) when a run ends.
--   completed_at     set when every step is done. A completed row stays as the
--                    record of the delete.
--
-- No foreign key to centry.project: the row outlives the project by design. A
-- COMPLETE row for an id is reopened (steps, attempts and lease reset) if a
-- project with that id is deleted again; an incomplete one refuses the delete
-- (and, from the other side, refuses Provision that id).
-- Idempotent. No BEGIN/COMMIT: the ledgered runner wraps the file.

CREATE TABLE IF NOT EXISTS centry.project_deletions (
    project_id      BIGINT      PRIMARY KEY,
    cleanup         JSONB       NOT NULL,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    attempts        INTEGER     NOT NULL DEFAULT 0,
    last_error      TEXT,
    next_attempt_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    claimed_until   TIMESTAMPTZ,
    completed_at    TIMESTAMPTZ
);

-- The reconciler's read: incomplete rows, oldest retry first.
CREATE INDEX IF NOT EXISTS project_deletions_pending_idx
    ON centry.project_deletions (next_attempt_at, project_id)
    WHERE completed_at IS NULL;

COMMENT ON TABLE centry.project_deletions IS
    'Cleanup journal of project deletes (#1211, shared/0161). The project row is deleted in the transaction that inserts this row; the steps recorded in cleanup run afterwards and are retried by a reconciler until completed_at is set.';
