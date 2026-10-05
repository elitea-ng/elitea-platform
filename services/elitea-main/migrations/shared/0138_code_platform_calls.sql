-- Root assigns the final migration number during coherent composition.
-- Main owns call receipts. Worker retains node and graph business state.
CREATE TABLE elitea_runtime.original_code_broker_bindings (
 execution_id TEXT NOT NULL,
 original_generation BIGINT NOT NULL CHECK (original_generation>0),
 visit_id TEXT NOT NULL,
 prepared_sha256 TEXT NOT NULL CHECK (prepared_sha256 ~ '^[a-f0-9]{64}$'),
 prepared_fingerprint TEXT NOT NULL CHECK (prepared_fingerprint ~ '^[a-f0-9]{64}$'),
 policy_sha256 TEXT NOT NULL CHECK (policy_sha256 ~ '^[a-f0-9]{64}$'),
 dependency_bundle_sha256 TEXT NOT NULL CHECK (dependency_bundle_sha256='' OR dependency_bundle_sha256 ~ '^[a-f0-9]{64}$'),
 broker_json BYTEA NOT NULL CHECK (octet_length(broker_json) BETWEEN 1 AND 512),
 retained_runtime_id TEXT CHECK (octet_length(retained_runtime_id) BETWEEN 1 AND 512),
 retained_runtime_kind TEXT CHECK (retained_runtime_kind IN ('docker','kubernetes')),
 last_owner_epoch BIGINT CHECK (last_owner_epoch>0),
 CHECK ((retained_runtime_id IS NULL AND retained_runtime_kind IS NULL AND last_owner_epoch IS NULL) OR (retained_runtime_id IS NOT NULL AND retained_runtime_kind IS NOT NULL AND last_owner_epoch IS NOT NULL)),
 PRIMARY KEY(execution_id,original_generation,visit_id),
 FOREIGN KEY(execution_id,original_generation,visit_id) REFERENCES elitea_runtime.original_code_visits(execution_id,generation,visit_id) ON DELETE CASCADE
);

CREATE TABLE elitea_runtime.code_platform_jobs (
 tenant_id TEXT NOT NULL CHECK (octet_length(tenant_id) BETWEEN 1 AND 256),
 execution_id TEXT NOT NULL,
 original_generation BIGINT NOT NULL CHECK (original_generation>0),
 activation_sha256 BYTEA NOT NULL CHECK (octet_length(activation_sha256)=32),
 prepared_sha256 BYTEA NOT NULL CHECK (octet_length(prepared_sha256)=32),
 policy_sha256 BYTEA NOT NULL CHECK (octet_length(policy_sha256)=32),
 retained_runtime_id TEXT NOT NULL CHECK (octet_length(retained_runtime_id) BETWEEN 1 AND 512),
 project_id BIGINT NOT NULL CHECK (project_id BETWEEN 1 AND 2147483647),
 actor_id BIGINT NOT NULL CHECK (actor_id BETWEEN 1 AND 2147483647),
 max_calls INTEGER NOT NULL CHECK (max_calls BETWEEN 1 AND 4096),
 max_total_bytes BIGINT NOT NULL CHECK (max_total_bytes BETWEEN 1 AND 67108864),
 last_sequence INTEGER NOT NULL DEFAULT 0 CHECK (last_sequence BETWEEN 0 AND 4096),
 total_bytes BIGINT NOT NULL DEFAULT 0 CHECK (total_bytes>=0 AND total_bytes<=max_total_bytes),
 PRIMARY KEY(execution_id,activation_sha256),
 FOREIGN KEY(execution_id,original_generation) REFERENCES elitea_runtime.execution_jobs(execution_id,generation) ON DELETE CASCADE
);
CREATE TABLE elitea_runtime.code_platform_calls (
 execution_id TEXT NOT NULL,
 activation_sha256 BYTEA NOT NULL CHECK (octet_length(activation_sha256)=32),
 sequence INTEGER NOT NULL CHECK (sequence BETWEEN 1 AND 4096),
 operation TEXT NOT NULL CHECK (octet_length(operation) BETWEEN 1 AND 32),
 effect_id TEXT NOT NULL CHECK (effect_id ~ '^[a-f0-9]{64}$'),
 call_sha256 BYTEA NOT NULL CHECK (octet_length(call_sha256)=32),
 resource_sha256 BYTEA NOT NULL CHECK (octet_length(resource_sha256)=32),
 arguments_sha256 BYTEA NOT NULL CHECK (octet_length(arguments_sha256)=32),
 payload_sha256 BYTEA NOT NULL CHECK (octet_length(payload_sha256)=32),
 frame_sha256 BYTEA NOT NULL CHECK (octet_length(frame_sha256)=32),
 frame_bytes BIGINT NOT NULL CHECK (frame_bytes BETWEEN 8 AND 327688),
 intent_ref TEXT NOT NULL CHECK (intent_ref ~ '^[a-f0-9]{64}\.cp1$'),
 state TEXT NOT NULL CHECK (state IN ('prepared','dispatching','uncertain','committed')),
 dispatch_claim_id TEXT REFERENCES elitea_runtime.execution_claims(claim_id),
 dispatch_generation BIGINT,
 dispatch_lease_epoch BIGINT,
 response_ref TEXT CHECK (response_ref ~ '^[a-f0-9]{64}\.cp1$'),
 response_sha256 BYTEA CHECK (response_sha256 IS NULL OR octet_length(response_sha256)=32),
 response_bytes BIGINT CHECK (response_bytes BETWEEN 1 AND 2162696),
 owner_receipt TEXT CHECK (octet_length(owner_receipt)<=1024),
 created_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
 resolved_at TIMESTAMPTZ,
 PRIMARY KEY(execution_id,activation_sha256,sequence),
 UNIQUE(effect_id),
 FOREIGN KEY(execution_id,activation_sha256) REFERENCES elitea_runtime.code_platform_jobs(execution_id,activation_sha256) ON DELETE CASCADE,
 CHECK ((state='prepared' AND dispatch_claim_id IS NULL AND dispatch_generation IS NULL AND dispatch_lease_epoch IS NULL) OR
        (state<>'prepared' AND dispatch_claim_id IS NOT NULL AND dispatch_generation>0 AND dispatch_lease_epoch>0)),
 CHECK ((state='committed' AND response_ref IS NOT NULL AND response_sha256 IS NOT NULL AND response_bytes IS NOT NULL AND resolved_at IS NOT NULL) OR
        (state<>'committed' AND response_ref IS NULL AND response_sha256 IS NULL AND response_bytes IS NULL AND resolved_at IS NULL))
);
CREATE UNIQUE INDEX code_platform_one_inflight_idx
 ON elitea_runtime.code_platform_calls(execution_id,activation_sha256)
 WHERE state IN ('prepared','dispatching','uncertain');
CREATE INDEX code_platform_reconcile_idx
 ON elitea_runtime.code_platform_calls(execution_id,activation_sha256,sequence)
 WHERE state IN ('dispatching','uncertain');

-- The child and this relation commit in native toolkit admission's transaction.
CREATE TABLE elitea_runtime.code_platform_toolkit_children (
 effect_id TEXT PRIMARY KEY REFERENCES elitea_runtime.code_platform_calls(effect_id) ON DELETE CASCADE,
 child_execution_id TEXT NOT NULL UNIQUE,
 child_generation BIGINT NOT NULL CHECK (child_generation=1),
 toolkit_id BIGINT NOT NULL CHECK (toolkit_id BETWEEN 1 AND 2147483647),
 toolkit_revision TEXT NOT NULL CHECK (toolkit_revision ~ '^[a-f0-9]{64}$'),
 arguments_sha256 BYTEA NOT NULL CHECK (octet_length(arguments_sha256)=32),
 created_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
 FOREIGN KEY(child_execution_id,child_generation) REFERENCES elitea_runtime.execution_jobs(execution_id,generation) ON DELETE CASCADE
);
