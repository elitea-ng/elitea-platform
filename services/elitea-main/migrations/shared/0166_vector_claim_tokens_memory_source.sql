-- 0166_vector_claim_tokens_memory_source.sql — admit the `memory` source on a
-- worker claim token (worker unit M9).
--
-- elitea-vector keeps a memory toolkit's points under their own source,
-- `memory` (ADR-0031), so a worker-claim token must be able to name it beside
-- `toolkit_index`. shared/0160 limited allowed_sources to the three sources
-- that existed then, so a mint that included `memory` would violate the check.
-- The constraint is replaced, not relaxed further: the four sources are listed.
--
-- Nothing else changes: no column is added (the personal-memory permission is
-- decided at each introspection, never stored), and no data is migrated.
--
-- Idempotent. No BEGIN/COMMIT: the ledgered runner wraps the file.

ALTER TABLE elitea_runtime.vector_claim_tokens
    DROP CONSTRAINT IF EXISTS vector_claim_tokens_sources;

ALTER TABLE elitea_runtime.vector_claim_tokens
    ADD CONSTRAINT vector_claim_tokens_sources
    CHECK (cardinality(allowed_sources) BETWEEN 1 AND 4
           AND allowed_sources <@ ARRAY['toolkit_index', 'deepwiki', 'inventory', 'memory']::text[]);
