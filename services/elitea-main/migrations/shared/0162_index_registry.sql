-- 0162_index_registry.sql — the index registry (ADR-0031 V1, ADR-0030
-- decision 2).
--
-- WHY. Under the Python indexing path an index's metadata is an `index_meta`
-- EMBEDDING ROW in the project's own pgvector database, written by the SDK and
-- by elitea-main together. Two writers raced on it (the SDK defects list in
-- ADR-0030), and the per-project database is exactly what ADR-0031 retires. A
-- deployment that sets ELITEA_INDEXING_RUNTIME=rust keeps that metadata here
-- instead, in one elitea-main table that elitea-main alone writes: the worker
-- reports counts, skips and the embedding stamp in the typed
-- IndexIngestSummaryV1, and Main applies them in its terminal transition.
--
-- NOTHING IS MIGRATED (owner decision, 2026-10-10: no migration, no legacy
-- support). The table starts empty, indexes written under the Python path are
-- not read, and users create their indexes again. A deployment that never
-- selects the Rust runtime never touches this table.
--
-- SHARED, NOT TENANT. Admission (`index_ingest_jobs`), the output projection
-- that settles a run and the terminal-effect reconcilers all live in
-- elitea_runtime, and Main applies a successful result in the SAME transaction
-- that projects it. project_id is a plain column, as on every elitea_runtime
-- table, and is deliberately NOT a foreign key: shared/0071, 0073 and 0098 each
-- refuse a reference to `centry.project`, which a corpus-only database lacks.
--
-- NO PERMISSION. The routes this serves already exist and keep their grants
-- (models.applications.index_meta.details, .index_meta.delete,
-- .tool.patch, and the schedule edit permission). Nothing here seeds
-- auth_core, so the central-permission seeding trap does not apply.
--
-- STATE is the closed set today's index_meta carries. `created` is the
-- run-neutral first history entry and also the state of a row Main created
-- without a run; `scheduled_reindex` is a history/notification marker only and
-- is not a row state.
--
-- NAME is unique per (project, toolkit) among rows that are not deleted. A
-- delete sets deleted_at and the row waits as a TOMBSTONE for the vector
-- deletion (elitea-vector Delete, ADR-0031 decision 4) before it is purged;
-- the partial index lets the name be created again meanwhile. A sweeper retries
-- the vector deletion of each tombstone with a backoff kept in the row
-- (attempts, next_attempt_at).
--
-- RUN LIVENESS is not stored here. A run is active exactly when the execution
-- job named by execution_id/execution_generation is not terminal, or SUCCEEDED
-- less than a bounded grace ago with its result not yet applied; the
-- repository reads that from elitea_runtime.execution_jobs, so a long healthy
-- run needs no heartbeat. A run found dead is recorded with the outcome its
-- job reached (cancelled, failed, or failed "abandoned" when the job is
-- missing).
--
-- COUNTS. The *_documents/*_chunks columns and `skipped` are the LAST run's.
-- The index's totals are index_registry_documents (rows, sum of chunk_count),
-- which the list reads.
--
-- Idempotent throughout. No BEGIN/COMMIT: the ledgered runner wraps the file.

CREATE SCHEMA IF NOT EXISTS elitea_runtime;

CREATE TABLE IF NOT EXISTS elitea_runtime.index_registry (
    index_id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    project_id          integer NOT NULL CHECK (project_id > 0),
    toolkit_id          integer NOT NULL CHECK (toolkit_id > 0),
    name                text NOT NULL
        CHECK (name <> '' AND octet_length(name) <= 4096),
    state               text NOT NULL
        CHECK (state IN ('created', 'in_progress', 'completed', 'partly_indexed', 'failed', 'cancelled')),
    -- The execution that is (or last was) running this index. Cleared by a
    -- cancel, as index_meta's task_id is.
    task_id             text,
    -- The safe failure text of the last terminal failure; NULL otherwise.
    error               text,
    conversation_id     text,
    -- One entry per run, newest last, capped on write (see the repository).
    history             jsonb NOT NULL DEFAULT '[]'::jsonb
        CHECK (jsonb_typeof(history) = 'array'),
    -- The `index_data` arguments, replayed verbatim by the next reindex.
    index_configuration jsonb NOT NULL DEFAULT '{}'::jsonb
        CHECK (jsonb_typeof(index_configuration) = 'object'),
    -- Documents and chunks of the LAST run, from the typed result. The index's
    -- totals are its index_registry_documents rows.
    indexed_documents   bigint NOT NULL DEFAULT 0 CHECK (indexed_documents >= 0),
    updated_documents   bigint NOT NULL DEFAULT 0 CHECK (updated_documents >= 0),
    indexed_chunks      bigint NOT NULL DEFAULT 0 CHECK (indexed_chunks >= 0),
    failed_chunks       bigint NOT NULL DEFAULT 0 CHECK (failed_chunks >= 0),
    -- Documents the last run did not index, by closed reason.
    skipped             jsonb NOT NULL DEFAULT '{}'::jsonb
        CHECK (jsonb_typeof(skipped) = 'object'),
    -- The embedding space, stamped by the FIRST terminal result and never
    -- changed after: a later result with another dimension fails the run. A
    -- model and its dimension are stamped together or not at all.
    embedding_model     text,
    embedding_dimension integer
        CHECK (embedding_dimension IS NULL OR embedding_dimension BETWEEN 1 AND 65535),
    -- The vector collection of that space, in elitea-vector's form
    -- emb_<slug>_<dimension>, where slug is the readable model name (lower
    -- case, [a-z0-9-], truncated) then '-' and the first 12 hex digits of the
    -- SHA-256 of the exact model string, so distinct models never share one.
    collection          text,
    -- The run fence. Every transition names the execution it applies to, so a
    -- late result of an older run cannot overwrite a newer one.
    execution_id        text,
    execution_generation bigint,
    index_generation    bigint NOT NULL DEFAULT 0 CHECK (index_generation >= 0),
    meta_id             text,
    correlation_id      text,
    created_at          timestamptz NOT NULL DEFAULT clock_timestamp(),
    updated_at          timestamptz NOT NULL DEFAULT clock_timestamp(),
    deleted_at          timestamptz,
    -- The tombstone sweeper's backoff, stored in the row so a restart or
    -- another replica keeps it. Only a tombstone (deleted_at IS NOT NULL) has a
    -- meaning for either column.
    attempts            integer NOT NULL DEFAULT 0 CHECK (attempts >= 0),
    next_attempt_at     timestamptz,
    CONSTRAINT index_registry_embedding_pair CHECK (
        (embedding_model IS NULL) = (embedding_dimension IS NULL)
        AND (embedding_model IS NULL) = (collection IS NULL)
    ),
    CONSTRAINT index_registry_fence_pair CHECK (
        (execution_id IS NULL) = (execution_generation IS NULL)
    )
);

CREATE UNIQUE INDEX IF NOT EXISTS index_registry_name_active_idx
    ON elitea_runtime.index_registry (project_id, toolkit_id, name)
    WHERE deleted_at IS NULL;

CREATE INDEX IF NOT EXISTS index_registry_list_idx
    ON elitea_runtime.index_registry (project_id, toolkit_id, created_at, index_id)
    WHERE deleted_at IS NULL;

CREATE INDEX IF NOT EXISTS index_registry_execution_idx
    ON elitea_runtime.index_registry (execution_id, execution_generation)
    WHERE execution_id IS NOT NULL;

CREATE INDEX IF NOT EXISTS index_registry_tombstone_idx
    ON elitea_runtime.index_registry (next_attempt_at NULLS FIRST, deleted_at)
    WHERE deleted_at IS NOT NULL;

-- One row per indexed document, for the incremental sync of ADR-0030 decision 3:
-- a run lists the source, skips a document whose version is unchanged, rewrites
-- a changed one and deletes one that disappeared. chunk_count lets a re-index
-- report what it replaced without asking the vector store.
CREATE TABLE IF NOT EXISTS elitea_runtime.index_registry_documents (
    index_id     uuid NOT NULL
        REFERENCES elitea_runtime.index_registry (index_id) ON DELETE CASCADE,
    document_key text NOT NULL
        CHECK (document_key <> '' AND octet_length(document_key) <= 4096),
    version      text NOT NULL CHECK (octet_length(version) <= 1024),
    chunk_count  integer NOT NULL DEFAULT 0 CHECK (chunk_count >= 0),
    updated_at   timestamptz NOT NULL DEFAULT clock_timestamp(),
    PRIMARY KEY (index_id, document_key)
);

COMMENT ON TABLE elitea_runtime.index_registry IS
    'Toolkit index metadata of the Rust indexing runtime (ELITEA_INDEXING_RUNTIME=rust). elitea-main is the only writer (ADR-0030 decision 2); the worker reports a typed IndexIngestSummaryV1. Not populated from the Python index_meta rows.';
COMMENT ON COLUMN elitea_runtime.index_registry.deleted_at IS
    'Tombstone: set by an index delete, cleared by purging the row once the vector store has deleted the index namespace (ADR-0031 decision 4).';
COMMENT ON TABLE elitea_runtime.index_registry_documents IS
    'One (document_key, version) per indexed document, for incremental sync (ADR-0030 decision 3).';
