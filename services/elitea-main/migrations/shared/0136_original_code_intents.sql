-- SOURCE-ONLY shared-store proposal. Root allocates ordered number/checksum.
-- Never run by this private packet.
CREATE TABLE elitea_runtime.original_code_visits (
 execution_id TEXT NOT NULL CHECK(execution_id ~ '^[0-9a-f]{32}$'),
 generation BIGINT NOT NULL CHECK(generation>0),
 visit_id TEXT NOT NULL CHECK(visit_id ~ '^[0-9a-f]{64}$'),
 visit_digest TEXT NOT NULL CHECK(visit_digest ~ '^[0-9a-f]{64}$'),
 activation_id TEXT NOT NULL CHECK(activation_id ~ '^[0-9a-f]{64}$'),
 attempt SMALLINT NOT NULL CHECK(attempt BETWEEN 1 AND 16),
 record_json BYTEA NOT NULL CHECK(octet_length(record_json) BETWEEN 1 AND 16384),
 registered_claim_id TEXT NOT NULL,
 created_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
 PRIMARY KEY(execution_id,generation,visit_id),
 UNIQUE(execution_id,generation,activation_id,attempt),
 FOREIGN KEY(execution_id,generation) REFERENCES elitea_runtime.execution_jobs(execution_id,generation)
);
CREATE TABLE elitea_runtime.original_code_intents (
 execution_id TEXT NOT NULL,generation BIGINT NOT NULL,visit_id TEXT NOT NULL,
 dispatch_activation TEXT NOT NULL CHECK(dispatch_activation ~ '^[0-9a-f]{64}$'),
 binding_json BYTEA NOT NULL CHECK(octet_length(binding_json) BETWEEN 1 AND 8192),
 binding_digest TEXT NOT NULL CHECK(binding_digest ~ '^[0-9a-f]{64}$'),
 compiled_selector_json BYTEA NOT NULL CHECK(octet_length(compiled_selector_json) BETWEEN 1 AND 131072),
 registered_claim_id TEXT NOT NULL,
 created_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
 PRIMARY KEY(execution_id,generation,visit_id),
 UNIQUE(execution_id,generation,dispatch_activation),
 FOREIGN KEY(execution_id,generation,visit_id) REFERENCES elitea_runtime.original_code_visits(execution_id,generation,visit_id)
);
CREATE TABLE elitea_runtime.original_code_owner_receipts (
 execution_id TEXT NOT NULL,generation BIGINT NOT NULL,dispatch_activation TEXT NOT NULL,
 node_receipt_sha256 TEXT NOT NULL CHECK(node_receipt_sha256 ~ '^[0-9a-f]{64}$'),
 receipt_wire BYTEA NOT NULL CHECK(octet_length(receipt_wire) BETWEEN 1 AND 1048576),
 receipt_sha256 TEXT NOT NULL CHECK(receipt_sha256 ~ '^[0-9a-f]{64}$'),
 admitted_claim_id TEXT NOT NULL,
 created_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
 PRIMARY KEY(execution_id,generation,dispatch_activation,node_receipt_sha256),
 FOREIGN KEY(execution_id,generation,dispatch_activation) REFERENCES elitea_runtime.original_code_intents(execution_id,generation,dispatch_activation)
);
CREATE FUNCTION elitea_runtime.original_code_record_immutable() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN RAISE EXCEPTION 'Original Code authority records are immutable'; END $$;
CREATE TRIGGER original_code_visit_immutable BEFORE UPDATE ON elitea_runtime.original_code_visits FOR EACH ROW EXECUTE FUNCTION elitea_runtime.original_code_record_immutable();
CREATE TRIGGER original_code_intent_immutable BEFORE UPDATE ON elitea_runtime.original_code_intents FOR EACH ROW EXECUTE FUNCTION elitea_runtime.original_code_record_immutable();
CREATE TRIGGER original_code_receipt_immutable BEFORE UPDATE ON elitea_runtime.original_code_owner_receipts FOR EACH ROW EXECUTE FUNCTION elitea_runtime.original_code_record_immutable();
