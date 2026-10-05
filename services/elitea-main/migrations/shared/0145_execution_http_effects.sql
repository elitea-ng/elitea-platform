-- Main owns each HTTP effect under its existing execution kernel.
CREATE TABLE elitea_runtime.execution_http_effects (
 execution_id TEXT NOT NULL,
 generation BIGINT NOT NULL,
 activation_id TEXT NOT NULL CHECK (activation_id ~ '^[a-f0-9]{64}$'),
 effect_id TEXT NOT NULL CHECK (effect_id ~ '^[a-f0-9]{64}$'),
 request_digest TEXT NOT NULL CHECK (request_digest ~ '^[a-f0-9]{64}$'),
 -- Sealed endpoint credentials never enter this record.
 request_bytes BYTEA NOT NULL CHECK (octet_length(request_bytes) BETWEEN 1 AND 393216),
 credential_revision TEXT NOT NULL,
 dispatch_claim_id TEXT NOT NULL REFERENCES elitea_runtime.execution_claims(claim_id),
 dispatch_lease_epoch BIGINT NOT NULL CHECK (dispatch_lease_epoch > 0),
 state TEXT NOT NULL CHECK (state IN ('dispatching','completed','failed','uncertain')),
 receipt_json JSONB,
 created_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
 resolved_at TIMESTAMPTZ,
 PRIMARY KEY(execution_id,generation,activation_id),
 UNIQUE(effect_id),
 FOREIGN KEY(execution_id,generation) REFERENCES elitea_runtime.execution_jobs(execution_id,generation) ON DELETE CASCADE,
 CHECK ((state='dispatching' AND receipt_json IS NULL AND resolved_at IS NULL) OR
        (state IN ('completed','failed','uncertain') AND receipt_json IS NOT NULL AND resolved_at IS NOT NULL)),
 CHECK (receipt_json IS NULL OR octet_length(receipt_json::text)<=3145728)
);
CREATE INDEX execution_http_effects_reconcile_idx ON elitea_runtime.execution_http_effects(execution_id,generation)
 WHERE state IN ('dispatching','uncertain');
