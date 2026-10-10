-- 0160_vector_claim_tokens.sql — the per-claim worker token for elitea-vector
-- (ADR-0031 decision 1: "the worker gets one per claim").
--
-- WHAT. When a worker claims an execution of a capability that may use
-- vectors (index.ingest.v1, toolkit.call_tool.v1, agent.execute.*), Main mints
-- a random bearer, returns it once in the claim receipt
-- (elitea.runtime.v1.VectorClaimTokenV1) and keeps only its SHA-256 here,
-- with what it is bound to: the claim, the execution, the execution's
-- resource project and actor, and the vector sources it may use.
--
-- WHY NOT AN auth_core__token ROW (like a provider callback token, 0159). A
-- callback token is a PAT: it authenticates the whole public API as its
-- owner. This bearer authenticates nothing but elitea-vector's token
-- introspection, and only while its claim lives, so it does not belong in the
-- PAT store and never passes the PAT validator.
--
-- REVOCATION IS READ, NOT WRITTEN. Introspection joins the claim and the
-- execution on every call and answers inactive once the claim is released
-- (settled, aborted, taken over), its lease has lapsed (a lost claim), the
-- execution is SETTLING or terminal, or its desired state is not RUNNING
-- (cancelled, draining). No settle, cancel or takeover path has to remember
-- to revoke anything, so none can forget to.
--
-- BOUNDED LAG. "Read on every call" is every call to Main. elitea-vector
-- caches an introspection answer, so a revoked token keeps working there for
-- at most that cache's cap: 5 s for a worker claim token (this table's
-- tokens), 60 s by default for an engine callback token (0159) and never
-- more than 300 s whatever is configured.
--
-- One row per claim: a repeated claim receipt for the same claim replaces the
-- hash, so only the bearer of the latest receipt works. The rows go with
-- their claim and execution (ON DELETE CASCADE) under the replay-retention
-- sweep (0047).
--
-- No permission, no backfill (no claim before this one holds a token).
-- Idempotent throughout. No BEGIN/COMMIT: the ledgered runner wraps the file.

CREATE TABLE IF NOT EXISTS elitea_runtime.vector_claim_tokens (
    token_sha256        bytea PRIMARY KEY,
    claim_id            text NOT NULL UNIQUE
        REFERENCES elitea_runtime.execution_claims (claim_id) ON DELETE CASCADE,
    execution_id        text NOT NULL,
    generation          bigint NOT NULL,
    -- Copied from execution_jobs when minted, so the read can require both
    -- to still agree.
    resource_project_id integer NOT NULL,
    actor_id            text NOT NULL,
    -- Payload keywords of elitea.vector.v1.Source.
    allowed_sources     text[] NOT NULL,
    expires_at          timestamptz NOT NULL,
    created_at          timestamptz NOT NULL DEFAULT clock_timestamp(),
    FOREIGN KEY (execution_id, generation)
        REFERENCES elitea_runtime.execution_jobs (execution_id, generation)
        ON DELETE CASCADE,
    CONSTRAINT vector_claim_tokens_hash_length
        CHECK (octet_length(token_sha256) = 32),
    CONSTRAINT vector_claim_tokens_project
        CHECK (resource_project_id > 0),
    CONSTRAINT vector_claim_tokens_actor
        CHECK (actor_id ~ '^[1-9][0-9]{0,18}$'),
    CONSTRAINT vector_claim_tokens_sources
        CHECK (cardinality(allowed_sources) BETWEEN 1 AND 4
               AND allowed_sources <@ ARRAY['toolkit_index', 'deepwiki', 'inventory']::text[]),
    CONSTRAINT vector_claim_tokens_expiry_order
        CHECK (expires_at > created_at)
);

CREATE INDEX IF NOT EXISTS vector_claim_tokens_execution_idx
    ON elitea_runtime.vector_claim_tokens (execution_id, generation);
