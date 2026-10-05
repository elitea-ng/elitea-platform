-- Source-only reserved candidate. Root must resolve the migration sequence.
-- Main owns one saved-child relation for HTTP/Code/debug/workspace/broker.
-- The sole immutable definition bytes/frame owner is shared by root and child.
CREATE TABLE elitea_runtime.execution_captured_definitions (
 resource_project_id bigint NOT NULL CHECK(resource_project_id BETWEEN 1 AND 2147483647),
 application_id bigint NOT NULL CHECK(application_id BETWEEN 0 AND 2147483647),
 version_id bigint NOT NULL CHECK(version_id BETWEEN 0 AND 2147483647),
 definition_sha256 text NOT NULL CHECK(definition_sha256 ~ '^[0-9a-f]{64}$'),
 definition_bytes bytea NOT NULL CHECK(octet_length(definition_bytes) BETWEEN 1 AND 1048576),
 byte_length bigint NOT NULL CHECK(byte_length=octet_length(definition_bytes)),
 created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 CHECK((application_id=0 AND version_id=0) OR(application_id>0 AND version_id>0)),
 PRIMARY KEY(resource_project_id,application_id,version_id,definition_sha256)
);
-- Source selection is a protected actor/definition relation, never another bytes cache.
CREATE TABLE elitea_runtime.execution_definition_source_refs (
 source_id text PRIMARY KEY CHECK(source_id ~ '^[0-9a-f]{64}$'),
 revision bigint NOT NULL CHECK(revision=1),
 digest_sha256 text NOT NULL CHECK(digest_sha256 ~ '^[0-9a-f]{64}$'),
 resource_project_id bigint NOT NULL,actor_id bigint NOT NULL CHECK(actor_id>0),
 application_id bigint NOT NULL,version_id bigint NOT NULL,
 definition_sha256 text NOT NULL,
 canonical_wire bytea NOT NULL CHECK(octet_length(canonical_wire) BETWEEN 1 AND 8192),
 UNIQUE(source_id,digest_sha256),
 UNIQUE(resource_project_id,actor_id,application_id,version_id,definition_sha256),
 FOREIGN KEY(resource_project_id,application_id,version_id,definition_sha256)
 REFERENCES elitea_runtime.execution_captured_definitions(resource_project_id,application_id,version_id,definition_sha256)
);
CREATE TABLE elitea_runtime.execution_saved_child_scopes (
 execution_id text NOT NULL, generation bigint NOT NULL,
 scope_id text NOT NULL CHECK(scope_id ~ '^[0-9a-f]{64}$'),
 revision bigint NOT NULL CHECK(revision=1),
 digest_sha256 text NOT NULL CHECK(digest_sha256 ~ '^[0-9a-f]{64}$'),
 canonical_wire bytea NOT NULL CHECK(octet_length(canonical_wire) BETWEEN 1 AND 524288),
 parent_scope_id text, state text NOT NULL CHECK(state IN ('active','retired')),
 registration_claim_id text NOT NULL, registration_lease_epoch bigint NOT NULL,
 created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 PRIMARY KEY(execution_id,generation,scope_id),
 UNIQUE(execution_id,generation,scope_id,revision,digest_sha256),
 FOREIGN KEY(execution_id,generation) REFERENCES elitea_runtime.execution_jobs(execution_id,generation),
 FOREIGN KEY(execution_id,generation,parent_scope_id) REFERENCES elitea_runtime.execution_saved_child_scopes(execution_id,generation,scope_id)
);
CREATE TABLE elitea_runtime.execution_saved_child_definitions (
 execution_id text NOT NULL,generation bigint NOT NULL,scope_id text NOT NULL,
 member_path text NOT NULL CHECK(octet_length(member_path)<=512),
 graph_thread_id text NOT NULL CHECK(octet_length(graph_thread_id) BETWEEN 1 AND 1024),
 resource_project_id bigint NOT NULL,
 application_id bigint NOT NULL CHECK(application_id BETWEEN 1 AND 2147483647),
 version_id bigint NOT NULL CHECK(version_id BETWEEN 1 AND 2147483647),
 definition_sha256 text NOT NULL CHECK(definition_sha256 ~ '^[0-9a-f]{64}$'),
 yaml_sha256 text NOT NULL CHECK(yaml_sha256 ~ '^[0-9a-f]{64}$'),
 PRIMARY KEY(execution_id,generation,scope_id,member_path),
 UNIQUE(execution_id,generation,scope_id,graph_thread_id),
 FOREIGN KEY(execution_id,generation,scope_id) REFERENCES elitea_runtime.execution_saved_child_scopes(execution_id,generation,scope_id),
 FOREIGN KEY(resource_project_id,application_id,version_id,definition_sha256)
 REFERENCES elitea_runtime.execution_captured_definitions(resource_project_id,application_id,version_id,definition_sha256)
);
ALTER TABLE elitea_runtime.execution_http_effects
 ADD COLUMN saved_child_scope_id text,
 ADD COLUMN saved_child_scope_revision bigint,
 ADD COLUMN saved_child_scope_digest text,
 ADD COLUMN saved_child_graph_thread text,
 ADD CONSTRAINT execution_http_effects_saved_child_shape CHECK(
 (saved_child_scope_id IS NULL AND saved_child_scope_revision IS NULL AND saved_child_scope_digest IS NULL AND saved_child_graph_thread IS NULL)
 OR(saved_child_scope_id IS NOT NULL AND saved_child_scope_revision IS NOT NULL AND saved_child_scope_digest IS NOT NULL AND saved_child_graph_thread IS NOT NULL
 AND saved_child_scope_id ~ '^[0-9a-f]{64}$' AND saved_child_scope_revision=1 AND saved_child_scope_digest ~ '^[0-9a-f]{64}$' AND octet_length(saved_child_graph_thread) BETWEEN 1 AND 1024)),
 ADD CONSTRAINT execution_http_effects_saved_child_owner FOREIGN KEY(execution_id,generation,saved_child_scope_id,saved_child_scope_revision,saved_child_scope_digest)
 REFERENCES elitea_runtime.execution_saved_child_scopes(execution_id,generation,scope_id,revision,digest_sha256);

-- One Main internal pre-redemption capture component, shared by all purposes.
CREATE TABLE elitea_runtime.execution_saved_child_captures (
 execution_id text NOT NULL,generation bigint NOT NULL,
 application_id bigint NOT NULL CHECK(application_id BETWEEN 1 AND 2147483647),
 version_id bigint NOT NULL CHECK(version_id BETWEEN 1 AND 2147483647),
 definition_sha256 text NOT NULL CHECK(definition_sha256 ~ '^[0-9a-f]{64}$'),
 root_input_sha256 text NOT NULL CHECK(root_input_sha256 ~ '^[0-9a-f]{64}$'),
 resource_project_id bigint NOT NULL,actor_id bigint NOT NULL,
 source_id text NOT NULL CHECK(source_id ~ '^[0-9a-f]{64}$'),
 source_digest_sha256 text NOT NULL CHECK(source_digest_sha256 ~ '^[0-9a-f]{64}$'),
 PRIMARY KEY(execution_id,generation,application_id,version_id,definition_sha256),
 FOREIGN KEY(execution_id,generation) REFERENCES elitea_runtime.execution_jobs(execution_id,generation),
 FOREIGN KEY(source_id,source_digest_sha256) REFERENCES elitea_runtime.execution_definition_source_refs(source_id,digest_sha256),
 FOREIGN KEY(resource_project_id,application_id,version_id,definition_sha256)
 REFERENCES elitea_runtime.execution_captured_definitions(resource_project_id,application_id,version_id,definition_sha256)
);
