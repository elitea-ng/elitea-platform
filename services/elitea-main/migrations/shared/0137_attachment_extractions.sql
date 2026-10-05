-- 0137_attachment_extractions.sql — the sidecar for extracted chat attachment text.
--
-- WHAT THIS STORES. elitea-main now extracts the text of a chat attachment
-- (PDF, DOCX, PPTX, XLSX, plain text) the first time the native runtime reads
-- it (internal/infra/extract, internal/infra/storage/runtime_attachment_object.go).
-- One row holds the result for ONE stored object: the normalised text, the
-- page/slide/sheet/section map with byte offsets into that text, the source's
-- unit count, the units with little or no text (usually scanned pages), the
-- token estimate, and the extractor version. A file that has no text this
-- platform can read gets a row too, with status 'refused' and the reason, so
-- it is not parsed again on every turn.
--
-- WHY A ROW AND NOT AN OBJECT. A sidecar OBJECT in the attachment bucket would
-- appear in the conversation's attachment listing and in the artifacts API as
-- a file the user never uploaded. A row is invisible to both, and it is
-- deleted with its object through the foreign key.
--
-- STALENESS. A re-upload under the same key keeps the object id and changes
-- byte_length and/or updated_at (queries/artifact_storage.sql upserts with
-- updated_at = now()). Every read matches all of object_id, extractor_version,
-- source_byte_length and source_updated_at, so text extracted from old bytes
-- or by an older extractor never answers for the current object.
--
-- SIZE. content is at most 20 MiB (the extractor's own text limit), which
-- PostgreSQL stores out of line (TOAST).
--
-- NO NEW PERMISSION. Only the claim-authorized runtime content route reads and
-- writes these rows; no product or admin route exposes them.
--
-- IDEMPOTENT throughout. No BEGIN/COMMIT: the ledgered runner executes each file
-- inside one transaction with its ledger row (migrate/runner.go apply).

CREATE SCHEMA IF NOT EXISTS elitea_storage;

CREATE TABLE IF NOT EXISTS elitea_storage.attachment_extractions (
    object_id          bigint      PRIMARY KEY
                                   REFERENCES elitea_storage.objects (id) ON DELETE CASCADE,
    extractor_version  text        NOT NULL CHECK (length(extractor_version) BETWEEN 1 AND 64),
    source_byte_length bigint      NOT NULL CHECK (source_byte_length >= 0),
    source_updated_at  timestamptz NOT NULL,
    status             text        NOT NULL CHECK (status IN ('extracted', 'refused')),
    -- reason is set for a refusal only: empty, too_large, unsupported_format,
    -- encrypted, malformed, unsafe_structure, no_text.
    reason             text        CHECK (reason IS NULL OR length(reason) BETWEEN 1 AND 64),
    format             text        CHECK (format IS NULL OR format IN ('pdf', 'docx', 'pptx', 'xlsx', 'text')),
    content            text        CHECK (content IS NULL OR octet_length(content) <= 20971520),
    units              jsonb       NOT NULL DEFAULT '[]'::jsonb,
    unit_count         integer     NOT NULL DEFAULT 0 CHECK (unit_count >= 0),
    low_text_units     integer[]   NOT NULL DEFAULT '{}',
    -- partial_reason is set when the extraction stopped at a limit:
    -- page_limit, text_limit or cell_limit.
    partial_reason     text        CHECK (partial_reason IS NULL OR length(partial_reason) BETWEEN 1 AND 32),
    token_estimate     bigint      NOT NULL DEFAULT 0 CHECK (token_estimate >= 0),
    extracted_at       timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT attachment_extractions_status_shape CHECK (
        (status = 'extracted' AND content IS NOT NULL AND format IS NOT NULL AND reason IS NULL)
        OR (status = 'refused' AND content IS NULL AND reason IS NOT NULL)
    )
);
