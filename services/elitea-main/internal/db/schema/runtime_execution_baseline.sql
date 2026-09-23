-- SQLC compiler projection for the post-0042 elitea_runtime tables used by
-- index admission, terminal-output reads, and configuration lifecycle writes.
--
-- This file is NOT a runtime migration. The embedded shared migration history
-- remains the only target-schema authority.

CREATE SCHEMA elitea_runtime;

CREATE TABLE elitea_runtime.input_bundles (
    input_bundle_id text PRIMARY KEY,
    immutable_version text NOT NULL,
    media_type text NOT NULL,
    resource_project_id integer NOT NULL REFERENCES centry.project(id),
    manifest_digest bytea NOT NULL,
    manifest_size bigint NOT NULL,
    manifest_bytes bytea NOT NULL,
    created_by text NOT NULL,
    created_at timestamptz NOT NULL
);

CREATE TABLE elitea_runtime.input_bundle_entries (
    input_bundle_id text NOT NULL
        REFERENCES elitea_runtime.input_bundles(input_bundle_id) ON DELETE CASCADE,
    entry_id text NOT NULL,
    entry_version text NOT NULL,
    semantic_role text NOT NULL,
    media_type text NOT NULL,
    content_digest bytea NOT NULL,
    content_size bigint NOT NULL,
    content_reference text NOT NULL,
    classification text NOT NULL,
    required_grant_audience text NOT NULL,
    content_bytes bytea NOT NULL,
    PRIMARY KEY (input_bundle_id, entry_id)
);

CREATE TABLE elitea_runtime.execution_jobs (
    execution_id text NOT NULL,
    generation bigint NOT NULL,
    command_id text NOT NULL UNIQUE,
    tenant_id text NOT NULL,
    resource_project_id integer NOT NULL REFERENCES centry.project(id),
    projection_project_id integer NOT NULL REFERENCES centry.project(id),
    actor_id text NOT NULL,
    principal_ref text NOT NULL,
    capability_id text NOT NULL,
    capability_version text NOT NULL,
    input_bundle_id text NOT NULL REFERENCES elitea_runtime.input_bundles(input_bundle_id),
    request_digest bytea NOT NULL,
    idempotency_scope text NOT NULL,
    idempotency_key text NOT NULL,
    configuration_revision_id text,
    configuration_type text,
    catalog_revision text,
    catalog_digest bytea,
    schema_id text,
    schema_revision text,
    schema_digest bytea,
    settings_entry_id text,
    state text NOT NULL,
    desired_state text NOT NULL,
    invocation_state text NOT NULL DEFAULT 'NOT_STARTED',
    admitted_at timestamptz NOT NULL,
    settled_at timestamptz,
    terminal_error_code text,
    PRIMARY KEY (execution_id, generation),
    UNIQUE (idempotency_scope, idempotency_key),
    UNIQUE (execution_id, generation, capability_id, input_bundle_id)
);

CREATE TABLE elitea_runtime.execution_admission_policies (
    capability_id text PRIMARY KEY,
    max_outstanding bigint NOT NULL,
    created_at timestamptz NOT NULL
);

CREATE TABLE elitea_runtime.command_outbox (
    outbox_id text PRIMARY KEY,
    execution_id text NOT NULL,
    generation bigint NOT NULL,
    stream_name text NOT NULL,
    dispatch_ordinal bigint NOT NULL,
    resource_class text NOT NULL,
    isolation_class text NOT NULL,
    priority integer NOT NULL,
    deadline timestamptz NOT NULL,
    limits_revision text NOT NULL,
    traceparent text NOT NULL,
    tracestate text NOT NULL,
    created_at timestamptz NOT NULL,
    prepared_signed_envelope_bytes bytea,
    prepared_signed_envelope_digest bytea,
    prepared_signature_profile integer,
    prepared_key_id text,
    prepared_at timestamptz,
    published_at timestamptz,
    published_envelope_digest bytea,
    authority_granted_at timestamptz,
    publish_attempts integer NOT NULL,
    last_error_code text,
    retired_at timestamptz,
    retirement_code text,
    last_visibility_at timestamptz,
    FOREIGN KEY (execution_id, generation)
        REFERENCES elitea_runtime.execution_jobs(execution_id, generation),
    UNIQUE (execution_id, generation)
);

CREATE TABLE elitea_runtime.execution_claims (
    claim_id text PRIMARY KEY,
    execution_id text NOT NULL,
    generation bigint NOT NULL,
    workload_session_id text NOT NULL,
    workload_identity text NOT NULL,
    producer_id text NOT NULL,
    claim_attempt bigint NOT NULL,
    lease_epoch bigint NOT NULL,
    fence_token bytea NOT NULL,
    claimed_at timestamptz NOT NULL,
    lease_expires_at timestamptz NOT NULL,
    released_at timestamptz,
    release_reason text,
    initial_output_watermark bigint NOT NULL,
    FOREIGN KEY (execution_id, generation)
        REFERENCES elitea_runtime.execution_jobs(execution_id, generation)
);

CREATE TABLE elitea_runtime.index_ingest_jobs (
    execution_id text NOT NULL,
    generation bigint NOT NULL,
    index_generation bigint NOT NULL,
    capability_id text NOT NULL,
    input_bundle_id text NOT NULL,
    toolkit_configuration_entry_id text NOT NULL,
    tool_parameters_entry_id text NOT NULL,
    llm_model_entry_id text,
    llm_configuration_entry_id text,
    mcp_tokens_entry_id text,
    embedding_binding_entry_id text,
    client_stream_id text,
    client_message_id text,
    sio_event text,
    index_meta_id text,
    index_meta_correlation_id text,
    index_meta_initialized_at timestamptz,
    index_meta_initialization_status text NOT NULL,
    index_meta_initialization_claim_token text,
    index_meta_initialization_claim_expires_at timestamptz,
    index_meta_initialization_attempt_count integer NOT NULL,
    index_meta_initialization_next_attempt_at timestamptz,
    index_meta_initialization_last_error_code text,
    index_meta_initialization_resolved_at timestamptz,
    index_meta_initialization_failed_at timestamptz,
    index_meta_terminal_state text,
    index_meta_terminal_occurred_at timestamptz,
    index_meta_terminal_status text,
    index_meta_terminal_claim_token text,
    index_meta_terminal_claim_expires_at timestamptz,
    index_meta_terminal_attempt_count integer,
    index_meta_terminal_next_attempt_at timestamptz,
    index_meta_terminal_last_error_code text,
    index_meta_terminalized_at timestamptz,
    index_manual_stop_requested_at timestamptz,
    index_manual_cleanup_status text,
    index_manual_cleanup_claim_token text,
    index_manual_cleanup_claim_expires_at timestamptz,
    index_manual_cleanup_attempt_count integer,
    index_manual_cleanup_next_attempt_at timestamptz,
    index_manual_cleanup_last_error_code text,
    index_manual_cleanup_resolved_at timestamptz,
    index_meta_task_restamp_source_event_id text,
    index_meta_task_restamp_occurred_at timestamptz,
    index_meta_task_restamp_created_on double precision,
    index_meta_task_restamp_status text,
    index_meta_task_restamp_claim_token text,
    index_meta_task_restamp_claim_expires_at timestamptz,
    index_meta_task_restamp_attempt_count integer,
    index_meta_task_restamp_next_attempt_at timestamptz,
    index_meta_task_restamp_last_error_code text,
    index_meta_task_restamped_at timestamptz,
    toolkit_id integer NOT NULL,
    index_name text NOT NULL,
    initiator text NOT NULL,
    PRIMARY KEY (execution_id, generation),
    FOREIGN KEY (execution_id, generation, capability_id, input_bundle_id)
        REFERENCES elitea_runtime.execution_jobs
                   (execution_id, generation, capability_id, input_bundle_id)
);

CREATE TABLE elitea_runtime.agent_execution_jobs (
    execution_id text NOT NULL,
    generation bigint NOT NULL,
    capability_id text NOT NULL,
    input_bundle_id text NOT NULL,
    request_entry_id text NOT NULL,
    client_stream_id text NOT NULL,
    client_message_id text NOT NULL,
    client_execution_generation text NOT NULL,
    sio_event text NOT NULL,
    PRIMARY KEY (execution_id, generation),
    FOREIGN KEY (execution_id, generation, capability_id, input_bundle_id)
        REFERENCES elitea_runtime.execution_jobs
                   (execution_id, generation, capability_id, input_bundle_id),
    FOREIGN KEY (input_bundle_id, request_entry_id)
        REFERENCES elitea_runtime.input_bundle_entries (input_bundle_id, entry_id)
);

CREATE TABLE elitea_runtime.toolkit_execute_read_jobs (
    execution_id text NOT NULL,
    generation bigint NOT NULL,
    capability_id text NOT NULL,
    input_bundle_id text NOT NULL,
    request_entry_id text NOT NULL,
    PRIMARY KEY (execution_id, generation),
    FOREIGN KEY (execution_id, generation, capability_id, input_bundle_id)
        REFERENCES elitea_runtime.execution_jobs
                   (execution_id, generation, capability_id, input_bundle_id),
    FOREIGN KEY (input_bundle_id, request_entry_id)
        REFERENCES elitea_runtime.input_bundle_entries (input_bundle_id, entry_id)
);

CREATE TABLE elitea_runtime.toolkit_execute_read_results (
    execution_id text NOT NULL,
    generation bigint NOT NULL,
    event_id text NOT NULL UNIQUE,
    result_json bytea NOT NULL,
    toolkit_type text NOT NULL,
    toolkit_name text NOT NULL,
    tool_name text NOT NULL,
    projected_at timestamptz NOT NULL,
    PRIMARY KEY (execution_id, generation),
    FOREIGN KEY (execution_id, generation)
        REFERENCES elitea_runtime.toolkit_execute_read_jobs (execution_id, generation),
    FOREIGN KEY (event_id)
        REFERENCES elitea_runtime.output_inbox (event_id)
);

CREATE TABLE elitea_runtime.execution_replay_events (
    cursor bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    event_id text NOT NULL UNIQUE,
    execution_id text NOT NULL,
    generation bigint NOT NULL,
    projection_project_id integer NOT NULL REFERENCES centry.project(id),
    event_type text NOT NULL,
    event_bytes bytea NOT NULL,
    event_digest bytea NOT NULL,
    created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    FOREIGN KEY (execution_id, generation)
        REFERENCES elitea_runtime.execution_jobs(execution_id, generation)
);

CREATE TABLE elitea_runtime.execution_replay_state (
    execution_id text NOT NULL,
    generation bigint NOT NULL,
    projection_project_id integer NOT NULL REFERENCES centry.project(id),
    last_node_sequence bigint NOT NULL DEFAULT 0,
    last_node_event_id text,
    last_node_event_bytes bytea,
    last_node_event_digest bytea,
    last_node_cursor bigint,
    pruned_through_cursor bigint NOT NULL DEFAULT 0,
    retained_progress_events bigint NOT NULL DEFAULT 0,
    retained_progress_bytes bigint NOT NULL DEFAULT 0,
    updated_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    PRIMARY KEY (execution_id, generation),
    FOREIGN KEY (execution_id, generation)
        REFERENCES elitea_runtime.execution_jobs(execution_id, generation)
);

CREATE TABLE elitea_runtime.index_generation_counters (
    resource_project_id integer NOT NULL REFERENCES centry.project(id),
    toolkit_id integer NOT NULL,
    index_name text NOT NULL,
    last_generation bigint NOT NULL,
    updated_at timestamptz NOT NULL,
    PRIMARY KEY (resource_project_id, toolkit_id, index_name)
);

CREATE TABLE elitea_runtime.index_result_artifacts (
    artifact_id text NOT NULL,
    immutable_version text NOT NULL,
    execution_id text NOT NULL,
    generation bigint NOT NULL,
    resource_project_id integer NOT NULL REFERENCES centry.project(id),
    media_type text NOT NULL,
    byte_length bigint NOT NULL,
    digest bytea NOT NULL,
    classification text NOT NULL,
    storage_record_id text NOT NULL UNIQUE,
    bytes_verified_at timestamptz NOT NULL,
    metadata_created_at timestamptz NOT NULL,
    PRIMARY KEY (artifact_id, immutable_version),
    FOREIGN KEY (execution_id, generation)
        REFERENCES elitea_runtime.execution_jobs(execution_id, generation),
    UNIQUE (
        artifact_id, immutable_version, execution_id, generation,
        storage_record_id
    )
);

CREATE TABLE elitea_runtime.index_ingest_results (
    logical_output_id text NOT NULL,
    execution_id text NOT NULL,
    generation bigint NOT NULL,
    input_bundle_id text NOT NULL
        REFERENCES elitea_runtime.input_bundles(input_bundle_id),
    input_bundle_digest bytea NOT NULL,
    artifact_id text,
    artifact_immutable_version text,
    artifact_storage_record_id text,
    completion_status text,
    completion_message text,
    projected_at timestamptz NOT NULL,
    PRIMARY KEY (execution_id, generation, logical_output_id),
    FOREIGN KEY (
        artifact_id, artifact_immutable_version, execution_id, generation,
        artifact_storage_record_id
    ) REFERENCES elitea_runtime.index_result_artifacts (
        artifact_id, immutable_version, execution_id, generation,
        storage_record_id
    )
);

CREATE TABLE elitea_runtime.configuration_lifecycle_outbox (
    event_id uuid PRIMARY KEY,
    resource_project_id integer NOT NULL,
    configuration_uuid uuid NOT NULL,
    revision bigint NOT NULL,
    operation text NOT NULL,
    actor_id integer NOT NULL,
    sanitized_snapshot json NOT NULL,
    snapshot_digest bytea NOT NULL,
    state text NOT NULL,
    attempt_count integer NOT NULL,
    available_at timestamptz NOT NULL,
    last_attempt_at timestamptz,
    lease_owner text,
    lease_expires_at timestamptz,
    delivered_at timestamptz,
    dead_at timestamptz,
    last_error_code text,
    created_at timestamptz NOT NULL,
    updated_at timestamptz NOT NULL,
    UNIQUE (resource_project_id, configuration_uuid, revision)
);
