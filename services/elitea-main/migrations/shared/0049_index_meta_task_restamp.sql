-- The current legacy main runtime restores the execution task_id after the
-- synchronous SDK resets index metadata for an index_data run. Persist that
-- external effect as a bounded, retryable intent beside the immutable index
-- admission. The intent is populated by the authenticated exact-generation
-- NodeEvent transaction; PgVector is reconciled later and never participates in
-- output ingestion.
ALTER TABLE elitea_runtime.index_ingest_jobs
    ADD COLUMN index_meta_task_restamp_source_event_id TEXT,
    ADD COLUMN index_meta_task_restamp_occurred_at TIMESTAMPTZ,
    ADD COLUMN index_meta_task_restamp_created_on DOUBLE PRECISION,
    ADD COLUMN index_meta_task_restamp_status TEXT,
    ADD COLUMN index_meta_task_restamp_claim_token TEXT,
    ADD COLUMN index_meta_task_restamp_claim_expires_at TIMESTAMPTZ,
    ADD COLUMN index_meta_task_restamp_attempt_count INTEGER,
    ADD COLUMN index_meta_task_restamp_next_attempt_at TIMESTAMPTZ,
    ADD COLUMN index_meta_task_restamp_last_error_code TEXT,
    ADD COLUMN index_meta_task_restamped_at TIMESTAMPTZ;

ALTER TABLE elitea_runtime.index_ingest_jobs
    ADD CONSTRAINT index_ingest_jobs_meta_task_restamp_source CHECK (
        (
            index_meta_task_restamp_source_event_id IS NULL
            AND index_meta_task_restamp_occurred_at IS NULL
            AND index_meta_task_restamp_created_on IS NULL
            AND index_meta_task_restamp_status IS NULL
        )
        OR (
            octet_length(index_meta_task_restamp_source_event_id)
                BETWEEN 1 AND 512
            AND index_meta_task_restamp_occurred_at IS NOT NULL
            AND index_meta_task_restamp_created_on IS NOT NULL
            AND index_meta_task_restamp_created_on
                <> 'NaN'::double precision
            AND index_meta_task_restamp_created_on
                NOT IN (
                    'Infinity'::double precision,
                    '-Infinity'::double precision
                )
            AND index_meta_task_restamp_created_on > 0
            AND index_meta_task_restamp_status IS NOT NULL
        )
    ),
    ADD CONSTRAINT index_ingest_jobs_meta_task_restamp_status CHECK (
        index_meta_task_restamp_status IS NULL
        OR index_meta_task_restamp_status
            IN ('PENDING', 'APPLIED', 'SUPERSEDED')
    ),
    ADD CONSTRAINT index_ingest_jobs_meta_task_restamp_resolution CHECK (
        (
            index_meta_task_restamp_status IS NULL
            AND index_meta_task_restamped_at IS NULL
        )
        OR (
            index_meta_task_restamp_status = 'PENDING'
            AND index_meta_task_restamped_at IS NULL
        )
        OR (
            index_meta_task_restamp_status IN ('APPLIED', 'SUPERSEDED')
            AND index_meta_task_restamped_at IS NOT NULL
        )
    ),
    ADD CONSTRAINT index_ingest_jobs_meta_task_restamp_claim CHECK (
        (
            index_meta_task_restamp_claim_token IS NULL
            AND index_meta_task_restamp_claim_expires_at IS NULL
        )
        OR (
            index_meta_task_restamp_status = 'PENDING'
            AND index_meta_task_restamp_claim_token IS NOT NULL
            AND index_meta_task_restamp_claim_expires_at IS NOT NULL
            AND octet_length(index_meta_task_restamp_claim_token)
                BETWEEN 1 AND 256
        )
    ),
    ADD CONSTRAINT index_ingest_jobs_meta_task_restamp_retry CHECK (
        (
            index_meta_task_restamp_status IS NULL
            AND index_meta_task_restamp_attempt_count IS NULL
            AND index_meta_task_restamp_next_attempt_at IS NULL
            AND index_meta_task_restamp_last_error_code IS NULL
        )
        OR (
            index_meta_task_restamp_status = 'PENDING'
            AND index_meta_task_restamp_attempt_count IS NOT NULL
            AND index_meta_task_restamp_attempt_count >= 0
            AND index_meta_task_restamp_next_attempt_at IS NOT NULL
        )
        OR (
            index_meta_task_restamp_status IN ('APPLIED', 'SUPERSEDED')
            AND index_meta_task_restamp_attempt_count IS NOT NULL
            AND index_meta_task_restamp_attempt_count > 0
            AND index_meta_task_restamp_next_attempt_at IS NULL
            AND index_meta_task_restamp_last_error_code IS NULL
        )
    ),
    ADD CONSTRAINT index_ingest_jobs_meta_task_restamp_last_error CHECK (
        index_meta_task_restamp_last_error_code IS NULL
        OR octet_length(index_meta_task_restamp_last_error_code)
            BETWEEN 1 AND 64
    ),
    ADD CONSTRAINT index_ingest_jobs_meta_task_restamp_requires_initialization CHECK (
        index_meta_task_restamp_status IS NULL
        OR index_meta_initialized_at IS NOT NULL
    );

-- Seed source-backed events accepted before this writer existed. This is safe
-- for the single-version PoV startup because migrations finish before Main
-- accepts output. A rolling mixed-version rollout still requires the normal
-- post-cutover migration/backfill gate used by other source-driven effects.
WITH restamp_sources AS (
    SELECT DISTINCT ON (r.execution_id, r.generation)
           r.execution_id,
           r.generation,
           r.event_id,
           r.created_at,
           (
               convert_from(r.event_bytes, 'UTF8')::jsonb
                   #>> '{response_metadata,created_at}'
           )::double precision AS created_on
    FROM elitea_runtime.execution_replay_events AS r
    WHERE r.event_type = 'execution.node_event'
      AND convert_from(r.event_bytes, 'UTF8')::jsonb->>'type'
            = 'agent_index_data_status'
      AND convert_from(r.event_bytes, 'UTF8')::jsonb
            #>> '{response_metadata,state}' = 'in_progress'
      AND jsonb_typeof(
            convert_from(r.event_bytes, 'UTF8')::jsonb
                #> '{response_metadata,created_at}'
          ) = 'number'
    ORDER BY r.execution_id, r.generation, r.cursor
)
UPDATE elitea_runtime.index_ingest_jobs AS i
SET index_meta_task_restamp_source_event_id = source.event_id,
    index_meta_task_restamp_occurred_at = source.created_at,
    index_meta_task_restamp_created_on = source.created_on,
    index_meta_task_restamp_status = 'PENDING',
    index_meta_task_restamp_attempt_count = 0,
    index_meta_task_restamp_next_attempt_at = clock_timestamp()
FROM restamp_sources AS source
WHERE i.execution_id = source.execution_id
  AND i.generation = source.generation
  AND i.capability_id = 'index.ingest.v1'
  AND i.index_meta_initialized_at IS NOT NULL
  AND i.index_meta_task_restamp_status IS NULL;

CREATE INDEX index_ingest_jobs_meta_task_restamp_pending_idx
    ON elitea_runtime.index_ingest_jobs (
        index_meta_task_restamp_next_attempt_at,
        execution_id,
        generation
    )
    INCLUDE (
        index_meta_task_restamp_source_event_id,
        index_meta_task_restamp_occurred_at,
        index_meta_task_restamp_created_on,
        index_meta_task_restamp_claim_expires_at
    )
    WHERE index_meta_task_restamp_status = 'PENDING';
