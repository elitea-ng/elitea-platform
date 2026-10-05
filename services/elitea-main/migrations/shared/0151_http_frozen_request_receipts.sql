-- Main owns frozen request and policy identity. V1 activation remains denied.
-- Apply only after verifying no unactivated V1 experimental rows exist.
ALTER TABLE elitea_runtime.execution_http_effects
 ADD COLUMN frozen_binding_digest TEXT NOT NULL CHECK (frozen_binding_digest ~ '^[a-f0-9]{64}$'),
 ADD COLUMN policy_digest TEXT NOT NULL CHECK (policy_digest ~ '^[a-f0-9]{64}$'),
 ADD COLUMN output_bucket TEXT NOT NULL,
 ADD COLUMN receipt_wire BYTEA CHECK (receipt_wire IS NULL OR octet_length(receipt_wire) BETWEEN 1 AND 3145728),
 ADD COLUMN receipt_sha256 TEXT CHECK (receipt_sha256 IS NULL OR receipt_sha256 ~ '^[a-f0-9]{64}$'),
 ADD CONSTRAINT http_effect_wire_receipt_state CHECK (
  (state='dispatching' AND receipt_wire IS NULL AND receipt_sha256 IS NULL) OR
  (state IN ('completed','failed','uncertain') AND receipt_wire IS NOT NULL AND receipt_sha256 IS NOT NULL)
 );
