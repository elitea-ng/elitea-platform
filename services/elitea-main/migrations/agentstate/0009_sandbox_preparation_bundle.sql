-- Keep validated preparation metadata before shared content publication.
-- This supervisor receipt belongs to agentstate, not the application schema.
ALTER TABLE elitea_runtime.sandbox_jobs
    ADD COLUMN preparation_bundle_json TEXT CHECK (
        preparation_bundle_json IS NULL OR octet_length(preparation_bundle_json) <= 131072
    );
