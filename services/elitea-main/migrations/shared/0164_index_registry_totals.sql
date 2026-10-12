-- 0164_index_registry_totals.sql — the index's totals on the registry row
-- (ADR-0030 stage I1, native Rust indexing).
--
-- WHY. 0162 derived an index's totals (how many documents it holds, how many
-- chunks) from index_registry_documents: count the rows, sum chunk_count. That
-- assumed the per-document rows are the whole index. They are not: the worker
-- reports a run in ONE output frame bounded at 64 KiB
-- (outputapp.MaxIndexIngestResultBytes), so IndexIngestSummaryV1.documents and
-- removed_document_keys are a BOUNDED SUBSET of what the run touched
-- (documents_truncated says when they are not all of it). A sum over a subset
-- is wrong, and silently so, on exactly the large indexes people look at.
--
-- The worker knows the whole index after the run — it listed the source and
-- wrote the vector store — and reports two integers, total_documents and
-- total_chunks, which always fit the frame. Main stores them here, in the
-- transaction that applies the result, and the list shows them. The
-- per-document rows keep their job (the incremental sync of ADR-0030 decision
-- 3: version per document key) and are no longer summed.
--
-- DEFAULT 0, not migrated: 0162 shipped with no deployment on the Rust runtime
-- (owner decision 2026-10-10: no legacy support), so every existing row is a
-- fresh one and the first result that lands fills the columns.
--
-- Idempotent throughout. No BEGIN/COMMIT: the ledgered runner wraps the file.

ALTER TABLE elitea_runtime.index_registry
    ADD COLUMN IF NOT EXISTS total_documents bigint NOT NULL DEFAULT 0 CHECK (total_documents >= 0),
    ADD COLUMN IF NOT EXISTS total_chunks    bigint NOT NULL DEFAULT 0 CHECK (total_chunks >= 0);

COMMENT ON COLUMN elitea_runtime.index_registry.total_documents IS
    'Documents in the whole index after the last applied result (IndexIngestSummaryV1.total_documents). The list shows this, not a count of index_registry_documents, which is a bounded subset.';
COMMENT ON COLUMN elitea_runtime.index_registry.total_chunks IS
    'Chunks in the whole index after the last applied result (IndexIngestSummaryV1.total_chunks).';
