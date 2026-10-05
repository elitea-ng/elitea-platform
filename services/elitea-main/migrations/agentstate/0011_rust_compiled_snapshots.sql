-- Compile and execute keep distinct intent. Terminal receipts stay immutable.
-- Supervisors set intent before dispatch under the original fenced job lease.
ALTER TABLE elitea_runtime.sandbox_jobs
 ADD COLUMN compiled_purpose TEXT CHECK (compiled_purpose IN ('compile','execute')),
 ADD COLUMN compiled_snapshot_key BYTEA CHECK (octet_length(compiled_snapshot_key)=32),
 ADD COLUMN compiled_base_request_digest BYTEA CHECK (octet_length(compiled_base_request_digest)=32),
 ADD COLUMN compiled_descriptor_json BYTEA CHECK (octet_length(compiled_descriptor_json) BETWEEN 1 AND 16384),
 ADD COLUMN compiled_export_verified BOOLEAN NOT NULL DEFAULT FALSE,
 -- Immutable captured export provenance. Publication recovery may renew the live job lease.
 ADD COLUMN compiled_export_lease_epoch BIGINT CHECK (compiled_export_lease_epoch>0 AND compiled_export_lease_epoch<=lease_epoch),
 ADD COLUMN runtime_cleanup_confirmed_at TIMESTAMPTZ,
 ADD CONSTRAINT sandbox_compiled_intent_shape CHECK (
  (compiled_purpose IS NULL AND compiled_snapshot_key IS NULL AND compiled_base_request_digest IS NULL
   AND compiled_descriptor_json IS NULL AND NOT compiled_export_verified AND compiled_export_lease_epoch IS NULL)
  OR (compiled_purpose IS NOT NULL AND compiled_snapshot_key IS NOT NULL AND compiled_base_request_digest IS NOT NULL)),
 ADD CONSTRAINT sandbox_compiled_export_shape CHECK (
  (NOT compiled_export_verified AND compiled_export_lease_epoch IS NULL) OR
  (compiled_export_verified AND compiled_purpose='compile' AND compiled_descriptor_json IS NOT NULL AND compiled_export_lease_epoch IS NOT NULL));

-- Main reserves quota before content upload. Publishing rows are not readable.
-- The original completed compiler receipt owns provenance, never a caller root.
-- compilation_lease_epoch is the immutable export epoch, not a renewed publication lease.
-- job_key is the existing activation-domain hash; runtime_id is the existing bound original runtime.
CREATE TABLE elitea_runtime.rust_compiled_snapshots (
 tenant_id TEXT NOT NULL,
 project_id INTEGER NOT NULL,
 snapshot_key BYTEA NOT NULL CHECK (octet_length(snapshot_key)=32),
 descriptor_root BYTEA NOT NULL CHECK (octet_length(descriptor_root)=32),
 descriptor_json BYTEA NOT NULL CHECK (octet_length(descriptor_json) BETWEEN 1 AND 16384),
 content_bytes BIGINT NOT NULL CHECK (content_bytes BETWEEN 1 AND 33570816),
 compilation_job_key BYTEA NOT NULL CHECK (octet_length(compilation_job_key)=32),
 compilation_request_digest BYTEA NOT NULL CHECK (octet_length(compilation_request_digest)=32),
 compilation_runtime_id TEXT NOT NULL CHECK (octet_length(compilation_runtime_id) BETWEEN 1 AND 512),
 compilation_lease_epoch BIGINT NOT NULL CHECK (compilation_lease_epoch>0),
 compilation_receipt_sha256 BYTEA CHECK (octet_length(compilation_receipt_sha256)=32),
 state TEXT NOT NULL CHECK (state IN ('publishing','ready','evicting')),
 created_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
 expires_at TIMESTAMPTZ NOT NULL,
 eviction_owner TEXT,
 eviction_until TIMESTAMPTZ,
 eviction_epoch BIGINT NOT NULL DEFAULT 0 CHECK (eviction_epoch>=0),
 PRIMARY KEY (tenant_id,project_id,snapshot_key),
 FOREIGN KEY (tenant_id,project_id,compilation_job_key)
  REFERENCES elitea_runtime.sandbox_jobs(tenant_id,project_id,job_key) ON DELETE RESTRICT,
 CHECK ((eviction_owner IS NULL AND eviction_until IS NULL)
  OR (state='evicting' AND octet_length(eviction_owner) BETWEEN 1 AND 128 AND eviction_until IS NOT NULL)),
 CHECK (state<>'ready' OR compilation_receipt_sha256 IS NOT NULL),
 CHECK (expires_at>created_at)
);
CREATE INDEX rust_compiled_snapshots_tenant_quota ON elitea_runtime.rust_compiled_snapshots(tenant_id,project_id);
CREATE INDEX rust_compiled_snapshots_expiry ON elitea_runtime.rust_compiled_snapshots(expires_at,tenant_id,project_id,snapshot_key);

-- Worker dispatch identity remains stable across generations. Store the selected
-- canonical descriptor before compiled execution admission; expiry never turns
-- an existing recorded compiled dispatch into a fresh compilation.
ALTER TABLE elitea_runtime.sandbox_dispatches
 ADD COLUMN compiled_descriptor_json BYTEA CHECK (octet_length(compiled_descriptor_json) BETWEEN 1 AND 16384);
