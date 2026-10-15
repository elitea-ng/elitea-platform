-- 0166_vector_claim_tokens_memory_source.sql — memory on worker claim tokens
-- (worker unit M9).
--
-- 1. The `memory` source. elitea-vector keeps a memory toolkit's points under
--    their own source, `memory` (ADR-0031), so a worker-claim token must be
--    able to name it beside `toolkit_index`. shared/0160 limited
--    allowed_sources to the three sources that existed then; the constraint is
--    replaced (the four sources are listed), not relaxed further.
--
-- 2. execution_jobs.memory_toolkit_ids: the memory toolkits a run was given,
--    computed ONCE at admission from the server-side frozen toolkit set (an
--    agent run: the top-level entries of the frozen version's `tools`, which
--    Main resolves from the project's own elitea_tools rows; a direct toolkit
--    call: the resolved toolkit itself when it is a memory toolkit). Nothing a
--    caller writes (tool arguments, user input, chat history, runtime-context
--    entries) contributes. Empty for every other run. ADD COLUMN with a
--    constant default is a catalogue-only change: no table rewrite, and the
--    ACCESS EXCLUSIVE lock is held only for the catalogue update.
--
-- 3. vector_claim_tokens.memory_toolkit_ids: copied from the execution at mint
--    (never decoded from the input bundle); elitea-vector refuses a memory
--    request for any other toolkit. A claim with none is minted without the
--    `memory` source.
--
-- 4. elitea_runtime.memory_conversation_taints: a conversation in which
--    PERSONAL memory was read (set when elitea-main allows a personal-scope
--    read for any execution bound to the conversation). While a conversation
--    is tainted, every execution bound to it is refused shared writes: project
--    memory and toolkit-index upserts and deletes. Never cleared; deleted with
--    the conversation (and with the project, by the foreign key). Set and
--    checked under an advisory lock keyed by the conversation.
--
-- The token-level personal_memory_read flag an earlier draft of this file
-- added is dropped (the rule is per conversation now). No data is migrated.
--
-- Idempotent. No BEGIN/COMMIT: the ledgered runner wraps the file.

ALTER TABLE elitea_runtime.vector_claim_tokens
    DROP COLUMN IF EXISTS personal_memory_read;

ALTER TABLE elitea_runtime.vector_claim_tokens
    ADD COLUMN IF NOT EXISTS memory_toolkit_ids bigint[] NOT NULL DEFAULT '{}';

ALTER TABLE elitea_runtime.execution_jobs
    ADD COLUMN IF NOT EXISTS memory_toolkit_ids bigint[] NOT NULL DEFAULT '{}';

ALTER TABLE elitea_runtime.vector_claim_tokens
    DROP CONSTRAINT IF EXISTS vector_claim_tokens_sources;

ALTER TABLE elitea_runtime.vector_claim_tokens
    ADD CONSTRAINT vector_claim_tokens_sources
    CHECK (cardinality(allowed_sources) BETWEEN 1 AND 4
           AND allowed_sources <@ ARRAY['toolkit_index', 'deepwiki', 'inventory', 'memory']::text[]);

CREATE TABLE IF NOT EXISTS elitea_runtime.memory_conversation_taints (
    project_id        integer     NOT NULL REFERENCES centry.project (id) ON DELETE CASCADE,
    conversation_uuid uuid        NOT NULL,
    tainted_at        timestamptz NOT NULL DEFAULT clock_timestamp(),
    PRIMARY KEY (project_id, conversation_uuid)
);
