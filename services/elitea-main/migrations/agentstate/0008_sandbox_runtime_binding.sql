-- Bind the original runtime instance before dispatch. This is supervisor-owned
-- receipt metadata, not a product schema or a graph checkpoint.
ALTER TABLE elitea_runtime.sandbox_jobs
    ADD COLUMN runtime_id TEXT CHECK (
        runtime_id IS NULL OR
        (octet_length(runtime_id) BETWEEN 1 AND 512 AND runtime_id !~ '[[:space:][:cntrl:]]')
    );
