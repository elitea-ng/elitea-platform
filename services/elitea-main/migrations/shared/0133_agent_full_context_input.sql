-- Full model windows require agent histories above the previous 1 MiB bound.
-- Keep configuration and direct toolkit bounds unchanged.
-- This changes runtime storage only. Application tables remain unchanged.
ALTER TABLE elitea_runtime.input_bundle_entries
    DROP CONSTRAINT input_bundle_entries_content_size;

ALTER TABLE elitea_runtime.input_bundle_entries
    ADD CONSTRAINT input_bundle_entries_content_size CHECK (
        (media_type = 'application/json' AND content_size BETWEEN 1 AND 262144)
        OR
        (media_type = 'application/vnd.elitea.agent-execution-input.v1+protobuf'
            AND content_size BETWEEN 1 AND 8388608)
        OR
        (media_type = 'application/vnd.elitea.toolkit-execute-read-input.v1+protobuf'
            AND content_size BETWEEN 1 AND 1048576)
    );
