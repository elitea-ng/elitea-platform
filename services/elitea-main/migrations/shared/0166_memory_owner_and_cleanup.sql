-- 0166_memory_owner_and_cleanup.sql — what the memory toolkit's server-side
-- owner enforcement and its toolkit-delete cleanup need (worker unit M9).
--
-- 1. elitea_runtime.vector_claim_tokens.personal_memory_allowed. elitea-vector
--    stamps a memory point's owner from the token and narrows every memory
--    call by it (ADR-0031, the `memory` source). The PERSONAL scope is the
--    token's user, and it is refused for a claim whose conversation has more
--    than one person in it: what a personal memory recalls or saves there
--    would be shown to the others. The mint decides this once, from the
--    execution's conversation, and stores it with the token; introspection
--    hands it to elitea-vector. DEFAULT true: a token minted before this
--    migration belongs to an execution that has settled or will, and carried
--    no memory source anyway.
--
-- 2. centry.toolkit_deletions.namespace. A memory toolkit's data is the vector
--    namespace its `namespace` setting names, and the setting is gone with the
--    toolkit row. The delete's transaction records it here, so the reconciler
--    (which only has this row) can ask elitea-vector to delete the namespace.
--    NULL for every other toolkit type.
--
-- Idempotent. No BEGIN/COMMIT: the ledgered runner wraps the file.

ALTER TABLE elitea_runtime.vector_claim_tokens
    ADD COLUMN IF NOT EXISTS personal_memory_allowed boolean NOT NULL DEFAULT true;

ALTER TABLE centry.toolkit_deletions
    ADD COLUMN IF NOT EXISTS namespace text;

COMMENT ON COLUMN elitea_runtime.vector_claim_tokens.personal_memory_allowed IS
    'false when the claim''s conversation has more than one person: the PERSONAL memory scope is refused (shared/0166).';
COMMENT ON COLUMN centry.toolkit_deletions.namespace IS
    'The memory namespace string of a deleted memory toolkit (shared/0166); NULL for other types.';
