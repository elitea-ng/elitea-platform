-- Supersedes the logical-activation-keyed draft; original draft stays unchanged.
-- Apply in Main shared migration history. Root assigns its migration number.
-- Worker checkpoints remain in agentstate. Main reads identity columns only.
CREATE TABLE elitea_runtime.code_debug_artifacts (
 tenant_id TEXT NOT NULL CHECK(octet_length(tenant_id) BETWEEN 1 AND 256),
 project_id BIGINT NOT NULL CHECK(project_id>0),
 execution_id TEXT NOT NULL CHECK(octet_length(execution_id) BETWEEN 1 AND 256),
 original_visit_id BYTEA NOT NULL CHECK(octet_length(original_visit_id)=32),
 original_visit_revision BIGINT NOT NULL CHECK(original_visit_revision=1),
 original_visit_digest BYTEA NOT NULL CHECK(octet_length(original_visit_digest)=32),
 attempt SMALLINT NOT NULL CHECK(attempt BETWEEN 1 AND 16),
 activation_id BYTEA NOT NULL CHECK(octet_length(activation_id)=32),
 actor_id BIGINT NOT NULL CHECK(actor_id>0),
 original_generation BIGINT NOT NULL CHECK(original_generation>0),
 admission_json BYTEA NOT NULL CHECK(octet_length(admission_json) BETWEEN 1 AND 3145728),
 object_key TEXT NOT NULL CHECK(object_key ~ '^[a-f0-9]{64}\.json$'),
 state TEXT NOT NULL CHECK(state IN ('staging','committed','abandoned')),
 created_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
 committed_at TIMESTAMPTZ,
 abandoned_at TIMESTAMPTZ,
 cleaned_at TIMESTAMPTZ,
 PRIMARY KEY(tenant_id,project_id,execution_id,original_generation,original_visit_id,original_visit_revision,original_visit_digest),
 UNIQUE(tenant_id,project_id,execution_id,original_generation,original_visit_id),
 UNIQUE(project_id,object_key),
 FOREIGN KEY(execution_id,original_generation) REFERENCES elitea_runtime.execution_jobs(execution_id,generation) ON DELETE RESTRICT,
 CHECK((state='staging' AND committed_at IS NULL AND abandoned_at IS NULL AND cleaned_at IS NULL) OR (state='committed' AND committed_at IS NOT NULL AND abandoned_at IS NULL AND cleaned_at IS NULL) OR (state='abandoned' AND committed_at IS NULL AND abandoned_at IS NOT NULL))
);

CREATE INDEX code_debug_staging_cleanup ON elitea_runtime.code_debug_artifacts(created_at) WHERE state='staging';
CREATE INDEX code_debug_abandoned_cleanup ON elitea_runtime.code_debug_artifacts(abandoned_at) WHERE state='abandoned' AND cleaned_at IS NULL;
