-- 0166_vector_claim_tokens_memory_source.sql — admit the `memory` source on a
-- worker claim token (worker unit M9).
--
-- elitea-vector keeps a memory toolkit's points under their own source,
-- `memory` (ADR-0031), so a worker-claim token must be able to name it beside
-- `toolkit_index`. shared/0160 limited allowed_sources to the three sources
-- that existed then, so a mint that included `memory` would violate the check.
-- The constraint is replaced, not relaxed further: the four sources are listed.
--
-- personal_memory_read records, per claim token, that the run read PERSONAL
-- memory (set when elitea-main answers a personal-scope read introspection).
-- From then on the run's project-memory WRITES are refused (what it read could
-- be copied into memory everyone shares). A run's other claims are covered by
-- reading the flag across the execution's token rows. The personal-memory
-- PERMISSION itself is still decided at each introspection and never stored.
-- The flag and its check serialise on the execution (an advisory lock, see
-- repos.VectorClaimTokens), so a parallel read and write of one run cannot
-- both pass.
--
-- memory_toolkit_ids are the memory toolkits the claim's frozen toolkits name
-- (all of the run's own project), written at mint. elitea-vector refuses a
-- memory request for any other toolkit id, so a token reaches only the memory
-- stores of the toolkits its run was given. Empty for a claim without a memory
-- toolkit, which is also minted without the `memory` source.
-- No data is migrated.
--
-- Idempotent. No BEGIN/COMMIT: the ledgered runner wraps the file.

ALTER TABLE elitea_runtime.vector_claim_tokens
    ADD COLUMN IF NOT EXISTS personal_memory_read boolean NOT NULL DEFAULT false;

ALTER TABLE elitea_runtime.vector_claim_tokens
    ADD COLUMN IF NOT EXISTS memory_toolkit_ids bigint[] NOT NULL DEFAULT '{}';

ALTER TABLE elitea_runtime.vector_claim_tokens
    DROP CONSTRAINT IF EXISTS vector_claim_tokens_sources;

ALTER TABLE elitea_runtime.vector_claim_tokens
    ADD CONSTRAINT vector_claim_tokens_sources
    CHECK (cardinality(allowed_sources) BETWEEN 1 AND 4
           AND allowed_sources <@ ARRAY['toolkit_index', 'deepwiki', 'inventory', 'memory']::text[]);
