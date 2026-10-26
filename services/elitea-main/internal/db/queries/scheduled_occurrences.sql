-- name: ScheduledDatabaseNow :one
SELECT clock_timestamp()::timestamptz;

-- name: GetScheduledJobCursorForUpdate :one
SELECT schedule_revision, observed_through, lease_epoch, lease_expires_at
FROM elitea_runtime.scheduled_job_cursors
WHERE job_id = sqlc.arg(job_id)
FOR UPDATE;

-- name: InsertScheduledJobCursor :exec
INSERT INTO elitea_runtime.scheduled_job_cursors (
    job_id, schedule_revision, observed_through
) VALUES (
    sqlc.arg(job_id), sqlc.arg(schedule_revision), sqlc.arg(observed_through)
);

-- name: SupersedeScheduledJobRevision :exec
UPDATE elitea_runtime.scheduled_occurrences
SET state = 'SUPERSEDED',
    lease_owner = NULL,
    claim_fence = NULL,
    lease_expires_at = NULL,
    completed_at = sqlc.arg(completed_at),
    updated_at = sqlc.arg(completed_at)
WHERE job_id = sqlc.arg(job_id)
  AND schedule_revision = sqlc.arg(schedule_revision)
  AND state = 'PENDING';

-- name: SupersedePendingScheduledJobOccurrences :exec
UPDATE elitea_runtime.scheduled_occurrences
SET state = 'SUPERSEDED',
    lease_owner = NULL,
    claim_fence = NULL,
    lease_expires_at = NULL,
    completed_at = sqlc.arg(completed_at),
    updated_at = sqlc.arg(completed_at)
WHERE job_id = sqlc.arg(job_id)
  AND state = 'PENDING'
  AND (lease_expires_at IS NULL OR lease_expires_at <= sqlc.arg(completed_at));

-- name: ClaimScheduledJobCursor :execrows
UPDATE elitea_runtime.scheduled_job_cursors
SET schedule_revision = sqlc.arg(schedule_revision),
    observed_through = sqlc.arg(observed_through),
    lease_owner = sqlc.arg(lease_owner),
    lease_epoch = sqlc.arg(lease_epoch),
    claim_fence = sqlc.arg(claim_fence),
    lease_expires_at = sqlc.arg(lease_expires_at),
    updated_at = sqlc.arg(updated_at)
WHERE job_id = sqlc.arg(job_id);

-- name: InsertScheduledOccurrence :exec
INSERT INTO elitea_runtime.scheduled_occurrences (
    invocation_id, job_id, schedule_revision, due_at, outcome_mode,
    next_attempt_at
) VALUES (
    sqlc.arg(invocation_id),
    sqlc.arg(job_id),
    sqlc.arg(schedule_revision),
    sqlc.arg(due_at),
    sqlc.arg(outcome_mode),
    sqlc.arg(due_at)
)
ON CONFLICT (job_id, schedule_revision, due_at) DO NOTHING;

-- name: AdvanceScheduledJobCursor :execrows
UPDATE elitea_runtime.scheduled_job_cursors
SET observed_through = sqlc.arg(observed_through),
    lease_owner = NULL,
    claim_fence = NULL,
    lease_expires_at = NULL,
    updated_at = clock_timestamp()
WHERE job_id = sqlc.arg(job_id)
  AND schedule_revision = sqlc.arg(schedule_revision)
  AND lease_epoch = sqlc.arg(lease_epoch)
  AND claim_fence = sqlc.arg(claim_fence);

-- name: ListClaimableScheduledOccurrences :many
SELECT occurrence.invocation_id,
       occurrence.job_id,
       occurrence.schedule_revision,
       occurrence.due_at,
       occurrence.outcome_mode,
       occurrence.lease_epoch
FROM elitea_runtime.scheduled_occurrences AS occurrence
JOIN generate_subscripts(
    sqlc.arg(job_ids)::text[],
    1
) AS registration(position)
  ON occurrence.job_id =
     (sqlc.arg(job_ids)::text[])[registration.position]
 AND occurrence.schedule_revision =
     (sqlc.arg(schedule_revisions)::text[])[registration.position]
WHERE occurrence.state = 'PENDING'
  AND occurrence.due_at <= sqlc.arg(observed_at)
  AND occurrence.next_attempt_at <= sqlc.arg(observed_at)
  AND (
      occurrence.lease_expires_at IS NULL
      OR occurrence.lease_expires_at <= sqlc.arg(observed_at)
  )
ORDER BY occurrence.due_at, occurrence.invocation_id
LIMIT sqlc.arg(page_limit)
FOR UPDATE OF occurrence SKIP LOCKED;

-- name: ClaimScheduledOccurrence :execrows
UPDATE elitea_runtime.scheduled_occurrences
SET lease_owner = sqlc.arg(lease_owner),
    lease_epoch = sqlc.arg(lease_epoch),
    claim_fence = sqlc.arg(claim_fence),
    lease_expires_at = sqlc.arg(lease_expires_at),
    attempt_count = attempt_count + 1,
    updated_at = sqlc.arg(updated_at)
WHERE invocation_id = sqlc.arg(invocation_id)
  AND state = 'PENDING';

-- name: CompleteScheduledOccurrence :execrows
UPDATE elitea_runtime.scheduled_occurrences
SET state = 'COMPLETED',
    outcome = sqlc.arg(outcome),
    admitted_at = CASE
        WHEN sqlc.arg(outcome)::text = 'durably_admitted'
            THEN clock_timestamp()
        ELSE NULL
    END,
    completed_at = clock_timestamp(),
    lease_owner = NULL,
    claim_fence = NULL,
    lease_expires_at = NULL,
    last_error_code = NULL,
    updated_at = clock_timestamp()
WHERE invocation_id = sqlc.arg(invocation_id)
  AND state = 'PENDING'
  AND lease_epoch = sqlc.arg(lease_epoch)
  AND claim_fence = sqlc.arg(claim_fence)
  AND outcome_mode = sqlc.arg(outcome_mode);

-- name: ReleaseScheduledOccurrenceForRetry :execrows
UPDATE elitea_runtime.scheduled_occurrences
SET lease_owner = NULL,
    claim_fence = NULL,
    lease_expires_at = NULL,
    next_attempt_at = clock_timestamp()
        + (sqlc.arg(retry_delay_milliseconds)::bigint * interval '1 millisecond'),
    last_error_code = sqlc.arg(last_error_code),
    updated_at = clock_timestamp()
WHERE invocation_id = sqlc.arg(invocation_id)
  AND state = 'PENDING'
  AND lease_epoch = sqlc.arg(lease_epoch)
  AND claim_fence = sqlc.arg(claim_fence);
