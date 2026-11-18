-- A failed sandbox run keeps the tail of the program's own stderr, so the
-- model that asked for the run can read a compiler diagnostic or an exception
-- message instead of only a failure code (#2223).
--
-- It is a column of its own, not result_json: the phase check above says a
-- failed row holds no result, and a result is what a worker projects into
-- state. This text is bounded diagnostics only (the supervisor keeps the last
-- 1,500 characters, control characters removed) and is meaningful only on a
-- failed row. Every row written before this migration reads NULL.
ALTER TABLE elitea_runtime.sandbox_jobs
    ADD COLUMN failure_detail TEXT
        CHECK (octet_length(failure_detail) BETWEEN 1 AND 8192);
ALTER TABLE elitea_runtime.sandbox_jobs
    ADD CONSTRAINT sandbox_failure_detail_phase
    CHECK (failure_detail IS NULL OR (phase = 'failed' AND failure_code IS NOT NULL)) NOT VALID;
ALTER TABLE elitea_runtime.sandbox_jobs VALIDATE CONSTRAINT sandbox_failure_detail_phase;
