-- 0170_http_action_rules_and_effects_v3.sql — HTTP action rules (project
-- admins, inside the operator egress allowlist) and revision-2 effect identity
-- (T-5D 5d-1; services/elitea-worker-rust/docs/http-action-node-design.md §5, §8).
--
-- http_action_rules
--   One row per project rule. Pipeline YAML names a rule by `name`; origin,
--   path template, method and credential (configuration_id) live only here.
--   The admin API validates the origin with the egress guard at write time and
--   the dialler checks again at dial time. `revision` increases on every write
--   and is pinned (with the rule id) in each effect row, so an edit or a disable
--   is ordered against Begin, never interleaved. Rules are never hard-deleted by
--   the API (disable instead); a project deletion removes them.
-- http_action_rule_audit
--   One row per accepted write (create/update), in the write transaction: who,
--   which revision, and the rule body as stored (no credential values — a rule
--   holds only a configuration id).
-- execution_http_effects (revision 2)
--   operation / inputs_digest / inputs_bytes: the rule name and the Worker's
--   canonical inputs, so recovery re-renders the request from stored bytes.
--   attempt: one row per (activation, attempt). A new attempt is admitted only
--   after the previous one ended uncertain or failed and only for class I rules
--   (safe method, or replay=idempotency_key with a bound key).
--   rule_id / rule_revision / rule_wire: the exact rule read inside Begin
--   (canonical, its SHA-256 is policy_digest), so recovery re-renders the
--   request with the rule that produced it even after a later edit.
--   dispatch_deadline: after it a `dispatching` row is reported uncertain and
--   never dispatched again. Revision-1 rows (none in production) keep NULLs.
CREATE TABLE elitea_runtime.http_action_rules (
 id BIGSERIAL PRIMARY KEY,
 project_id BIGINT NOT NULL CHECK (project_id BETWEEN 1 AND 2147483647),
 name TEXT NOT NULL CHECK (octet_length(name) BETWEEN 1 AND 128 AND name ~ '^[a-z][a-z0-9_]*(\.[a-z][a-z0-9_]*)*$'),
 origin TEXT NOT NULL CHECK (octet_length(origin) BETWEEN 9 AND 262 AND origin ~ '^https://[A-Za-z0-9]([A-Za-z0-9.-]{0,251}[A-Za-z0-9])?(:[0-9]{1,5})?$'),
 path_template TEXT NOT NULL CHECK (octet_length(path_template) BETWEEN 1 AND 2048 AND left(path_template, 1) = '/'),
 method TEXT NOT NULL CHECK (method IN ('GET','HEAD','OPTIONS','POST','PUT','PATCH','DELETE')),
 query_names TEXT[] NOT NULL DEFAULT '{}' CHECK (cardinality(query_names) <= 32),
 configuration_id INTEGER NOT NULL CHECK (configuration_id >= 0),
 toolkit TEXT NOT NULL CHECK (toolkit ~ '^[A-Za-z0-9_.:-]{1,128}$'),
 tool TEXT NOT NULL CHECK (tool ~ '^[A-Za-z0-9_.:-]{1,128}$'),
 output_bucket TEXT CHECK (output_bucket IS NULL OR output_bucket ~ '^[A-Za-z0-9._-]{1,128}$'),
 replay TEXT NOT NULL DEFAULT 'never' CHECK (replay IN ('never','idempotency_key')),
 enabled BOOLEAN NOT NULL DEFAULT TRUE,
 revision BIGINT NOT NULL DEFAULT 1 CHECK (revision > 0),
 created_by BIGINT NOT NULL CHECK (created_by > 0),
 updated_by BIGINT NOT NULL CHECK (updated_by > 0),
 created_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
 updated_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
 UNIQUE (project_id, name)
);

CREATE TABLE elitea_runtime.http_action_rule_audit (
 rule_id BIGINT NOT NULL REFERENCES elitea_runtime.http_action_rules(id) ON DELETE CASCADE,
 revision BIGINT NOT NULL CHECK (revision > 0),
 project_id BIGINT NOT NULL CHECK (project_id BETWEEN 1 AND 2147483647),
 actor_id BIGINT NOT NULL CHECK (actor_id > 0),
 action TEXT NOT NULL CHECK (action IN ('create','update')),
 rule_json JSONB NOT NULL CHECK (octet_length(rule_json::text) <= 16384),
 created_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
 PRIMARY KEY (rule_id, revision)
);
CREATE INDEX http_action_rule_audit_project_idx ON elitea_runtime.http_action_rule_audit(project_id, created_at);

ALTER TABLE elitea_runtime.execution_http_effects
 ADD COLUMN operation TEXT CHECK (operation IS NULL OR (octet_length(operation) BETWEEN 1 AND 128 AND operation ~ '^[a-z][a-z0-9_]*(\.[a-z][a-z0-9_]*)*$')),
 ADD COLUMN inputs_digest TEXT CHECK (inputs_digest IS NULL OR inputs_digest ~ '^[a-f0-9]{64}$'),
 ADD COLUMN inputs_bytes BYTEA CHECK (inputs_bytes IS NULL OR octet_length(inputs_bytes) BETWEEN 1 AND 262144),
 ADD COLUMN attempt INTEGER NOT NULL DEFAULT 0 CHECK (attempt BETWEEN 0 AND 15),
 ADD COLUMN rule_id BIGINT CHECK (rule_id IS NULL OR rule_id > 0),
 ADD COLUMN rule_revision BIGINT CHECK (rule_revision IS NULL OR rule_revision > 0),
 ADD COLUMN rule_wire BYTEA CHECK (rule_wire IS NULL OR octet_length(rule_wire) BETWEEN 2 AND 16384),
 ADD COLUMN dispatch_deadline TIMESTAMPTZ,
 ADD CONSTRAINT execution_http_effects_v3_shape CHECK (
  (operation IS NULL AND inputs_digest IS NULL AND inputs_bytes IS NULL AND rule_id IS NULL AND rule_revision IS NULL AND rule_wire IS NULL AND dispatch_deadline IS NULL)
  OR (operation IS NOT NULL AND inputs_digest IS NOT NULL AND inputs_bytes IS NOT NULL AND rule_id IS NOT NULL AND rule_revision IS NOT NULL AND rule_wire IS NOT NULL AND dispatch_deadline IS NOT NULL)
 );
ALTER TABLE elitea_runtime.execution_http_effects DROP CONSTRAINT execution_http_effects_pkey;
ALTER TABLE elitea_runtime.execution_http_effects ADD PRIMARY KEY (execution_id, generation, activation_id, attempt);

-- Permissions (decision D4, pending confirmation): manage = project admins;
-- list (rule names only, for the pipeline editor) = admin, editor, viewer.
DO $$
BEGIN
IF to_regclass('public.auth_core__role') IS NULL
   OR to_regclass('public.auth_core__role_permission') IS NULL THEN
    RAISE NOTICE '0170: auth_core tables absent, nothing to grant';
    RETURN;
END IF;

INSERT INTO public.auth_core__role_permission (role_id, permission)
SELECT role.id, grant_row.permission
FROM public.auth_core__role AS role
CROSS JOIN (VALUES
    ('models.http_action_rules.manage', ARRAY['admin']),
    ('models.http_action_rules.list', ARRAY['admin','editor','viewer'])
) AS grant_row(permission, roles)
WHERE role.mode = 'default' AND role.name = ANY (grant_row.roles)
ON CONFLICT (role_id, permission) DO NOTHING;

IF to_regclass('public.auth_core__project_role') IS NULL
   OR to_regclass('public.auth_core__project_role_permission') IS NULL THEN
    RAISE NOTICE '0170: no per-project permission tables, the central grant is the whole story here';
    RETURN;
END IF;

-- A project whose role matrix carries override rows hides central grants
-- (legacyrbac falls back only when no override exists), so the override rows
-- get the same grants.
INSERT INTO public.auth_core__project_role_permission (project_id, role_id, permission)
SELECT DISTINCT overridden.project_id, overridden.role_id, grant_row.permission
FROM (
    SELECT DISTINCT project_id, role_id
    FROM public.auth_core__project_role_permission
    WHERE role_id IS NOT NULL
) AS overridden
JOIN public.auth_core__project_role AS project_role
  ON project_role.id = overridden.role_id
 AND project_role.project_id = overridden.project_id
CROSS JOIN (VALUES
    ('models.http_action_rules.manage', ARRAY['admin']),
    ('models.http_action_rules.list', ARRAY['admin','editor','viewer'])
) AS grant_row(permission, roles)
WHERE project_role.name = ANY (grant_row.roles)
ON CONFLICT (project_id, role_id, permission) DO NOTHING;
END
$$;
