-- toolkit.call_tool.v1 admits an execution that runs ONE tool of ONE toolkit.
--
-- Two CHECK constraints enumerate the capabilities this kernel accepts, so a
-- row naming the new one is rejected until both are widened. Neither check is
-- about the new capability's own shape: the first says the capability carries
-- none of the configuration-validation columns, and the second names the
-- payload type its terminal output writes.
--
-- Nothing else in the kernel needs a change. execution_jobs, input_bundles,
-- command_outbox, execution_claims and execution_settlements carry no
-- capability discriminator, and an admission policy row is data rather than
-- schema.

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
                'toolkit.call_tool.v1'
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
            'TOOLKIT_CALL_TOOL_RESULT'
        )
    );
