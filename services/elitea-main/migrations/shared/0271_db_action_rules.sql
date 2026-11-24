-- 0271_db_action_rules.sql — DB action rules (project admins, inside the
-- operator DB egress allowlist) and write audit (T-5E 5e-2;
-- handoffs/point5-wave1-20261008/T-5E-db-node.md §3.2, §4).
--
-- db_action_rules
--   One row per project rule. Pipeline YAML names a rule by `name`; the SQL
--   configuration reference (`configuration_id` of type `sql`), dialect,
--   database_name, mode (read | write), and write marker_table live only here.
--   Host, port, username and password live only in the referenced `sql`
--   configuration (never in YAML or rule rows). The admin API validates the
--   referenced configuration's host/port against ELITEA_DB_EGRESS_ALLOWLIST
--   at write time, and the Worker's Rust egress guard checks again at every
--   dial (5e-1). `revision` increases on every write. Rules are never
--   hard-deleted by the API (disable instead); a project deletion removes them.
-- db_action_rule_audit
--   One row per accepted write (create/update), in the write transaction: who,
--   which revision, and the rule body as stored (no host or credential values
--   — a rule holds only a configuration id).
CREATE TABLE elitea_runtime.db_action_rules (
 id BIGSERIAL PRIMARY KEY,
 project_id BIGINT NOT NULL CHECK (project_id BETWEEN 1 AND 2147483647),
 name TEXT NOT NULL CHECK (octet_length(name) BETWEEN 1 AND 128 AND name ~ '^[a-z][a-z0-9_]*(\.[a-z][a-z0-9_]*)*$'),
 configuration_id INTEGER NOT NULL CHECK (configuration_id > 0),
 dialect TEXT NOT NULL CHECK (dialect IN ('postgres','mysql')),
 database_name TEXT NOT NULL CHECK (octet_length(database_name) BETWEEN 1 AND 128 AND database_name ~ '^[A-Za-z0-9_][A-Za-z0-9_$-]{0,127}$'),
 mode TEXT NOT NULL DEFAULT 'read' CHECK (mode IN ('read','write')),
 marker_table TEXT CHECK (
  (mode = 'read' AND marker_table IS NULL)
  OR (mode = 'write' AND marker_table IS NOT NULL AND octet_length(marker_table) BETWEEN 1 AND 128 AND marker_table ~ '^[A-Za-z_][A-Za-z0-9_]{0,62}(\.[A-Za-z_][A-Za-z0-9_]{0,62})?$')
 ),
 enabled BOOLEAN NOT NULL DEFAULT TRUE,
 revision BIGINT NOT NULL DEFAULT 1 CHECK (revision > 0),
 created_by BIGINT NOT NULL CHECK (created_by > 0),
 updated_by BIGINT NOT NULL CHECK (updated_by > 0),
 created_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
 updated_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
 UNIQUE (project_id, name)
);

CREATE TABLE elitea_runtime.db_action_rule_audit (
 rule_id BIGINT NOT NULL REFERENCES elitea_runtime.db_action_rules(id) ON DELETE CASCADE,
 revision BIGINT NOT NULL CHECK (revision > 0),
 project_id BIGINT NOT NULL CHECK (project_id BETWEEN 1 AND 2147483647),
 actor_id BIGINT NOT NULL CHECK (actor_id > 0),
 action TEXT NOT NULL CHECK (action IN ('create','update')),
 rule_json JSONB NOT NULL CHECK (octet_length(rule_json::text) <= 16384),
 created_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
 PRIMARY KEY (rule_id, revision)
);
CREATE INDEX db_action_rule_audit_project_idx ON elitea_runtime.db_action_rule_audit(project_id, created_at);

-- Permissions: manage = project admins; list (rule names, dialect, mode,
-- enabled only — never host/database/credentials, for the pipeline editor) =
-- admin, editor, viewer.
DO $$
BEGIN
IF to_regclass('public.auth_core__role') IS NULL
   OR to_regclass('public.auth_core__role_permission') IS NULL THEN
    RAISE NOTICE '0271: auth_core tables absent, nothing to grant';
    RETURN;
END IF;

INSERT INTO public.auth_core__role_permission (role_id, permission)
SELECT role.id, grant_row.permission
FROM public.auth_core__role AS role
CROSS JOIN (VALUES
    ('models.db_action_rules.manage', ARRAY['admin']),
    ('models.db_action_rules.list', ARRAY['admin','editor','viewer'])
) AS grant_row(permission, roles)
WHERE role.mode = 'default' AND role.name = ANY (grant_row.roles)
ON CONFLICT (role_id, permission) DO NOTHING;

IF to_regclass('public.auth_core__project_role') IS NULL
   OR to_regclass('public.auth_core__project_role_permission') IS NULL THEN
    RAISE NOTICE '0271: no per-project permission tables, the central grant is the whole story here';
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
    ('models.db_action_rules.manage', ARRAY['admin']),
    ('models.db_action_rules.list', ARRAY['admin','editor','viewer'])
) AS grant_row(permission, roles)
WHERE project_role.name = ANY (grant_row.roles)
ON CONFLICT (project_id, role_id, permission) DO NOTHING;
END
$$;
