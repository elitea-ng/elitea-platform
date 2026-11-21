-- A terminal index-metadata effect that fails validation used to stay PENDING
-- for ever: the reconciler refused it on every claim, and because the claim
-- path returned at the first invalid row, it also took the rest of its batch
-- down with it. The reconciler now records INVALID_TERMINAL_EFFECT, backs the
-- effect off, and after a bounded number of attempts moves it to QUARANTINED,
-- which no claim selects again. A quarantined row keeps its error code and
-- gets a terminalized_at, so an operator can find it:
--
--   SELECT execution_id, generation, index_meta_terminal_last_error_code
--   FROM elitea_runtime.index_ingest_jobs
--   WHERE index_meta_terminal_status = 'QUARANTINED';
--
-- Only the three CHECK constraints that enumerate the statuses change; 0043 is
-- released and stays as it is.
ALTER TABLE elitea_runtime.index_ingest_jobs
    DROP CONSTRAINT index_ingest_jobs_meta_terminal_status,
    DROP CONSTRAINT index_ingest_jobs_meta_terminal_resolution,
    DROP CONSTRAINT index_ingest_jobs_meta_terminal_retry;

ALTER TABLE elitea_runtime.index_ingest_jobs
    ADD CONSTRAINT index_ingest_jobs_meta_terminal_status CHECK (
        index_meta_terminal_status IS NULL
        OR index_meta_terminal_status IN (
            'PENDING', 'APPLIED', 'SUPERSEDED', 'QUARANTINED'
        )
    ),
    ADD CONSTRAINT index_ingest_jobs_meta_terminal_resolution CHECK (
        (
            index_meta_terminal_status IS NULL
            AND index_meta_terminalized_at IS NULL
        )
        OR (
            index_meta_terminal_status = 'PENDING'
            AND index_meta_terminalized_at IS NULL
        )
        OR (
            index_meta_terminal_status IN ('APPLIED', 'SUPERSEDED', 'QUARANTINED')
            AND index_meta_terminalized_at IS NOT NULL
        )
    ),
    ADD CONSTRAINT index_ingest_jobs_meta_terminal_retry CHECK (
        (
            index_meta_terminal_status IS NULL
            AND index_meta_terminal_attempt_count IS NULL
            AND index_meta_terminal_next_attempt_at IS NULL
            AND index_meta_terminal_last_error_code IS NULL
        )
        OR (
            index_meta_terminal_status = 'PENDING'
            AND index_meta_terminal_attempt_count IS NOT NULL
            AND index_meta_terminal_attempt_count >= 0
            AND index_meta_terminal_next_attempt_at IS NOT NULL
        )
        OR (
            index_meta_terminal_status IN ('APPLIED', 'SUPERSEDED')
            AND index_meta_terminal_attempt_count IS NOT NULL
            AND index_meta_terminal_attempt_count > 0
            AND index_meta_terminal_next_attempt_at IS NULL
            AND index_meta_terminal_last_error_code IS NULL
        )
        OR (
            index_meta_terminal_status = 'QUARANTINED'
            AND index_meta_terminal_attempt_count IS NOT NULL
            AND index_meta_terminal_attempt_count > 0
            AND index_meta_terminal_next_attempt_at IS NULL
            AND index_meta_terminal_last_error_code IS NOT NULL
        )
    );
