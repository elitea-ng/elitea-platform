-- 0187_execution_security_contexts.sql — the typed security context of one
-- governed execution (ADR-0039 decision 6, issue #1115).
--
--   execution_security_contexts  the deterministic ExecutionSecurityContextV1
--                                bytes Main built at admission, and the digest
--                                the signed worker command carries in
--                                WorkerCommandV1.security_context_ref. The
--                                worker fetches the bytes under its claim
--                                through RuntimeContentService.GetInput
--                                (content id "elitea.security-context/<id>").
--
-- Written in the admission transaction, only for an execution pinned to the
-- governed profile, so a flag-off deployment writes nothing here. The bytes
-- hold references, epochs and digests only, never a credential. A row never
-- changes: UPDATE is rejected. Additive only; idempotent; no BEGIN/COMMIT.

CREATE TABLE IF NOT EXISTS elitea_runtime.execution_security_contexts (
    execution_id     text PRIMARY KEY CHECK (length(execution_id) BETWEEN 1 AND 128),
    content_id       text NOT NULL UNIQUE CHECK (content_id = 'elitea.security-context/' || execution_id),
    schema_revision  text NOT NULL CHECK (schema_revision = 'elitea.runtime.security-context.v1'),
    digest           bytea NOT NULL CHECK (octet_length(digest) = 32),
    context_bytes    bytea NOT NULL CHECK (octet_length(context_bytes) BETWEEN 1 AND 16384),
    created_at       timestamptz NOT NULL DEFAULT clock_timestamp()
);

CREATE OR REPLACE FUNCTION elitea_runtime.execution_security_context_immutable()
RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'execution security contexts are immutable';
END
$$;

DROP TRIGGER IF EXISTS execution_security_contexts_immutable
    ON elitea_runtime.execution_security_contexts;
CREATE TRIGGER execution_security_contexts_immutable
    BEFORE UPDATE ON elitea_runtime.execution_security_contexts
    FOR EACH ROW EXECUTE FUNCTION elitea_runtime.execution_security_context_immutable();
