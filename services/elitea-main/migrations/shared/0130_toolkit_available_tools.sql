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
                'toolkit.execute.read.v1',
                'toolkit.available_tools.v1'
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
            'TOOLKIT_EXECUTE_READ_RESULT', 'TOOLKIT_AVAILABLE_TOOLS_RESULT'
        )
    );
