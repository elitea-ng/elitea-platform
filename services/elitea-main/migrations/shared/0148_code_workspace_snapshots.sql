-- Root assigns the migration number after the original Code visit migration.
-- A selected base belongs to one original visit. Changing it cannot reacquire.
CREATE TABLE elitea_runtime.code_workspace_snapshots (
 execution_id text NOT NULL,
 generation bigint NOT NULL CHECK (generation > 0),
 visit_id text NOT NULL CHECK (visit_id ~ '^[a-f0-9]{64}$'),
 visit_digest_sha256 text NOT NULL CHECK (visit_digest_sha256 ~ '^[a-f0-9]{64}$'),
 activation_id text NOT NULL CHECK (activation_id ~ '^[a-f0-9]{64}$'),
 tenant_id text NOT NULL CHECK (length(tenant_id) BETWEEN 1 AND 256),
 resource_project_id bigint NOT NULL CHECK (resource_project_id > 0),
 actor_id bigint NOT NULL CHECK (actor_id > 0),
 base_prepared_sha256 text NOT NULL CHECK (base_prepared_sha256 ~ '^[a-f0-9]{64}$'),
 selection_sha256 text NOT NULL CHECK (selection_sha256 ~ '^[a-f0-9]{64}$'),
 policy_sha256 text NOT NULL CHECK (policy_sha256 ~ '^[a-f0-9]{64}$'),
 manifest_sha256 text CHECK (manifest_sha256 ~ '^[a-f0-9]{64}$'),
 created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 ready_at timestamptz,
 PRIMARY KEY (execution_id,generation,visit_id),
 CHECK ((manifest_sha256 IS NULL) = (ready_at IS NULL)),
 FOREIGN KEY (execution_id,generation,visit_id)
  REFERENCES elitea_runtime.original_code_visits (execution_id,generation,visit_id) ON DELETE RESTRICT
);
