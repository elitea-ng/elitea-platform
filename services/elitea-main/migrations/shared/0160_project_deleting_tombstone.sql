-- 0160_project_deleting_tombstone.sql — a project being deleted says so, and
-- no new work is admitted for it (#1211).
--
-- WHY. Deleting a project drops its PgVector database, which every execution of
-- the project uses (checkpoints, index tools). The delete therefore has to
-- know, BEFORE it removes anything, that the project has no work in flight and
-- that none can arrive afterwards. Counting the work and then deleting leaves a
-- window in which a job is admitted between the two; and a refusal that comes
-- after secrets, buckets and permissions are already gone is not a refusal.
--
-- WHAT. `centry.project.deleting_at` is the tombstone. Deprovision sets it in
-- one transaction that first takes the project row FOR UPDATE and counts the
-- non-terminal execution_jobs of the project; with any, it rolls back and the
-- delete changes nothing. The tombstone is never cleared: the row only leaves
-- through the delete, so a retried delete finds it and continues the walk.
--
-- ADMISSION. Every path that admits work inserts an elitea_runtime.execution_jobs
-- row, and the Rust worker does too, so the refusal is a BEFORE INSERT trigger
-- on that table rather than a predicate in each query. It reads the project row
-- FOR KEY SHARE: that conflicts with the delete's FOR UPDATE, so an insert that
-- races the fence waits for it, then sees the committed tombstone (READ
-- COMMITTED re-reads the updated row) and is refused. An insert that won the
-- race holds its key-share lock, so the fence waits for it and counts it.
-- SQLSTATE 55000 (object_not_in_prerequisite_state); the message names the
-- project.
--
-- The non-terminal states the delete counts are the states of 0033's
-- execution_jobs_active_capability_idx (domain/execution.NonTerminalJobStates
-- is the Go definition of the same set; QUARANTINED counts as terminal).
--
-- Idempotent. Additive: a NULL column on a pylon-owned table, no backfill.

ALTER TABLE centry.project ADD COLUMN IF NOT EXISTS deleting_at TIMESTAMPTZ;

COMMENT ON COLUMN centry.project.deleting_at IS
    'Tombstone: set by the project delete before it removes anything. A project with a value is on its way out: no work is admitted for it and nothing reuses it. Never cleared; the row leaves with the delete. (#1211, shared/0160)';

CREATE OR REPLACE FUNCTION elitea_runtime.refuse_work_for_deleting_project()
RETURNS trigger
LANGUAGE plpgsql
AS $$
DECLARE
    tombstone TIMESTAMPTZ;
    pid       INTEGER;
BEGIN
    FOR pid IN SELECT DISTINCT ids.p FROM unnest(ARRAY[NEW.resource_project_id, NEW.projection_project_id]) AS ids(p) ORDER BY ids.p LOOP
        SELECT deleting_at INTO tombstone
        FROM centry.project
        WHERE id = pid
        FOR KEY SHARE;
        IF FOUND AND tombstone IS NOT NULL THEN
            RAISE EXCEPTION 'project % is being deleted: no new work is admitted for it', pid
                USING ERRCODE = 'object_not_in_prerequisite_state';
        END IF;
    END LOOP;
    RETURN NEW;
END;
$$;

DROP TRIGGER IF EXISTS execution_jobs_refuse_deleting_project ON elitea_runtime.execution_jobs;
CREATE TRIGGER execution_jobs_refuse_deleting_project
    BEFORE INSERT ON elitea_runtime.execution_jobs
    FOR EACH ROW EXECUTE FUNCTION elitea_runtime.refuse_work_for_deleting_project();
