ALTER TABLE elitea_runtime.execution_jobs
    DROP CONSTRAINT execution_jobs_capability_payload;

ALTER TABLE elitea_runtime.execution_jobs
    ADD CONSTRAINT execution_jobs_capability_payload CHECK (
        (
            capability_id = 'configuration.validate.v1'
            AND num_nonnulls(
                configuration_revision_id,
                configuration_type,
                catalog_revision,
                catalog_digest,
                schema_id,
                schema_revision,
                schema_digest,
                settings_entry_id
            ) = 8
        )
        OR
        (
            capability_id IN (
                'index.ingest.v1',
                'agent.execute.application.v1',
                'agent.execute.adhoc.v1',
                'toolkit.call_tool.v1',
                'toolkit.execute.read.v1'
            )
            AND num_nonnulls(
                configuration_revision_id,
                configuration_type,
                catalog_revision,
                catalog_digest,
                schema_id,
                schema_revision,
                schema_digest,
                settings_entry_id
            ) = 0
        )
    );

-- Migration 0054 bounded protobuf agent inputs in the shared entry table.
-- Direct toolkit execution uses a different protobuf contract with the same
-- one-megabyte authoritative-input bound, so an upgrade must extend the
-- existing constraint before the first toolkit input is admitted. The sqlc
-- schema is not an upgrade mechanism and cannot make this change for an
-- already-migrated database.
ALTER TABLE elitea_runtime.input_bundle_entries
    DROP CONSTRAINT input_bundle_entries_content_size;

ALTER TABLE elitea_runtime.input_bundle_entries
    ADD CONSTRAINT input_bundle_entries_content_size CHECK (
        (
            media_type = 'application/json'
            AND content_size BETWEEN 1 AND 262144
        )
        OR
        (
            media_type IN (
                'application/vnd.elitea.agent-execution-input.v1+protobuf',
                'application/vnd.elitea.toolkit-execute-read-input.v1+protobuf'
            )
            AND content_size BETWEEN 1 AND 1048576
        )
    );

-- Migration 0056 limits the output inbox to the existing result families.
-- Add the direct toolkit result before its projector writes the first frame.
ALTER TABLE elitea_runtime.output_inbox
    DROP CONSTRAINT output_inbox_payload_type;

ALTER TABLE elitea_runtime.output_inbox
    ADD CONSTRAINT output_inbox_payload_type CHECK (
        payload_type IN (
            'CONFIGURATION_VALIDATION',
            'RUNTIME_FAILURE',
            'INDEX_INGEST_RESULT',
            'AGENT_EXECUTION_RESULT',
            'TOOLKIT_CALL_TOOL_RESULT',
            'TOOLKIT_EXECUTE_READ_RESULT'
        )
    );

CREATE TABLE elitea_runtime.toolkit_execute_read_jobs (
    execution_id TEXT NOT NULL,
    generation BIGINT NOT NULL,
    capability_id TEXT NOT NULL,
    input_bundle_id TEXT NOT NULL,
    request_entry_id TEXT NOT NULL,
    PRIMARY KEY (execution_id, generation),
    FOREIGN KEY (execution_id, generation, capability_id, input_bundle_id)
        REFERENCES elitea_runtime.execution_jobs
                   (execution_id, generation, capability_id, input_bundle_id)
        ON DELETE CASCADE,
    FOREIGN KEY (input_bundle_id, request_entry_id)
        REFERENCES elitea_runtime.input_bundle_entries (input_bundle_id, entry_id),
    CONSTRAINT toolkit_execute_read_jobs_capability CHECK (
        capability_id = 'toolkit.execute.read.v1'
    ),
    CONSTRAINT toolkit_execute_read_jobs_request_entry CHECK (
        octet_length(request_entry_id) BETWEEN 1 AND 256
    )
);

CREATE INDEX toolkit_execute_read_jobs_bundle_idx
    ON elitea_runtime.toolkit_execute_read_jobs
       (input_bundle_id, request_entry_id, execution_id, generation);

CREATE TABLE elitea_runtime.toolkit_execute_read_results (
    execution_id TEXT NOT NULL,
    generation BIGINT NOT NULL,
    event_id TEXT NOT NULL UNIQUE,
    result_json BYTEA NOT NULL,
    toolkit_type TEXT NOT NULL,
    toolkit_name TEXT NOT NULL,
    tool_name TEXT NOT NULL,
    projected_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    PRIMARY KEY (execution_id, generation),
    FOREIGN KEY (execution_id, generation)
        REFERENCES elitea_runtime.toolkit_execute_read_jobs (execution_id, generation)
        ON DELETE CASCADE,
    FOREIGN KEY (event_id)
        REFERENCES elitea_runtime.output_inbox (event_id),
    CONSTRAINT toolkit_execute_read_results_payload CHECK (
        octet_length(result_json) BETWEEN 1 AND 49152
    ),
    CONSTRAINT toolkit_execute_read_results_identity CHECK (
        octet_length(toolkit_type) BETWEEN 1 AND 256
        AND octet_length(toolkit_name) BETWEEN 1 AND 256
        AND octet_length(tool_name) BETWEEN 1 AND 256
    )
);
