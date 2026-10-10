-- 0160_project_deletions_journal.sql — the cleanup journal of a project delete
-- (#1211).
--
-- WHY. A project delete removes resources in places one transaction cannot
-- span: the vault rows, the artifact bytes in object storage, the tenant schema,
-- and the per-project PgVector database and role on another server. So the
-- delete is split in two:
--
--   1. ONE TRANSACTION DECIDES. It locks the project row FOR UPDATE, refuses
--      (rolls back, nothing changed) while the project has non-terminal
--      execution_jobs, records what the project owns that must be cleaned up,
--      inserts that record here, deletes every row that references the project
--      and the project row itself, and commits. Any failure in it leaves the
--      project exactly as it was.
--   2. THE CLEANUP RUNS FROM THIS ROW. Every step is idempotent and marks
--      itself done in `cleanup`. The delete runs it right after the commit; a
--      step that fails leaves the row incomplete, and a reconciler in
--      elitea-main retries it with backoff until `completed_at` is set.
--
-- After the commit nothing can create work for the project: execution_jobs
-- carries foreign keys to centry.project(id) on both resource_project_id and
-- projection_project_id, so an admission for a project with no row fails.
--
-- COLUMNS.
--   cleanup          what the decision recorded, and per-step completion:
--                    {"tenant_schema": "p_42", "system_user_id": 17,
--                     "vault_project": "42", "buckets": ["reports"],
--                     "had_vector_store": true, "vector_database": "project_42",
--                     "done": {"system_user": true, ...}}
--   attempts         cleanup runs so far (the delete's own run included).
--   last_error       the last run's failure, by step name only (no raw error:
--                    it can carry SQL or addresses).
--   next_attempt_at  backoff: the reconciler does not retry before this.
--   claimed_until    the lease of the run working the row. The reconciler
--                    claims a row in a short FOR UPDATE SKIP LOCKED transaction
--                    that stamps the lease; it does not hold a connection or a
--                    lock while the steps run.
--   completed_at     set when every step is done. A completed row stays as the
--                    record of the delete.
--
-- No foreign key to centry.project: the row outlives the project by design.
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
    'Cleanup journal of project deletes (#1211, shared/0160). The project row is deleted in the transaction that inserts this row; the steps recorded in cleanup run afterwards and are retried by a reconciler until completed_at is set.';
