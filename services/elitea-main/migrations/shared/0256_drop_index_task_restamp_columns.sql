-- Retire the index v1 task-restamp intent (#1382).
--
-- Migration 0049 added these columns for a retryable "restore the execution
-- task_id after the SDK resets index metadata" effect. #1378 removed the last
-- writer and reader, and the index v2 cutover preflight no longer counts
-- pending restamps, so nothing touches them. 0049 is released and stays as
-- it is; this migration removes what it created. Dropping the columns also
-- drops their CHECK constraints and the pending partial index.
ALTER TABLE elitea_runtime.index_ingest_jobs
    DROP COLUMN IF EXISTS index_meta_task_restamp_source_event_id,
    DROP COLUMN IF EXISTS index_meta_task_restamp_occurred_at,
    DROP COLUMN IF EXISTS index_meta_task_restamp_created_on,
    DROP COLUMN IF EXISTS index_meta_task_restamp_status,
    DROP COLUMN IF EXISTS index_meta_task_restamp_claim_token,
    DROP COLUMN IF EXISTS index_meta_task_restamp_claim_expires_at,
    DROP COLUMN IF EXISTS index_meta_task_restamp_attempt_count,
    DROP COLUMN IF EXISTS index_meta_task_restamp_next_attempt_at,
    DROP COLUMN IF EXISTS index_meta_task_restamp_last_error_code,
    DROP COLUMN IF EXISTS index_meta_task_restamped_at;
