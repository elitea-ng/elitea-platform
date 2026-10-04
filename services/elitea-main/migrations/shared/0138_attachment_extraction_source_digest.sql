-- 0138_attachment_extraction_source_digest.sql — find a filed extraction by
-- the bytes it was made from.
--
-- WHY. The chat client uploads an attached file again with every message it
-- rides on. The upload upserts the same object row and sets updated_at to
-- now(), and 0137 matches a filed extraction on object_id, byte_length and
-- updated_at. So a byte-identical re-upload missed the filed extraction and
-- was extracted again. A document that takes longer than the route's wait
-- answered "processing" on every message and never reached the model.
--
-- WHAT. source_sha256 is the SHA-256 of the bytes an extraction was made
-- from. When the (byte_length, updated_at) lookup misses, elitea-main hashes
-- the bytes it read and looks the row up by this digest. A hit is filed under
-- the object's new version and served without a new extraction.
--
-- Rows filed before this migration have no digest. They still answer the
-- (byte_length, updated_at) lookup, and the next extraction fills the digest.
--
-- NO NEW PERMISSION. Only the claim-authorized runtime content route reads
-- and writes these rows.
--
-- IDEMPOTENT. No BEGIN/COMMIT: the ledgered runner executes each file inside
-- one transaction with its ledger row (migrate/runner.go apply).

ALTER TABLE elitea_storage.attachment_extractions
    ADD COLUMN IF NOT EXISTS source_sha256 bytea;

DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint
         WHERE conname = 'attachment_extractions_source_sha256_length'
           AND conrelid = 'elitea_storage.attachment_extractions'::regclass
    ) THEN
        ALTER TABLE elitea_storage.attachment_extractions
            ADD CONSTRAINT attachment_extractions_source_sha256_length
            CHECK (source_sha256 IS NULL OR octet_length(source_sha256) = 32);
    END IF;
END
$$;
