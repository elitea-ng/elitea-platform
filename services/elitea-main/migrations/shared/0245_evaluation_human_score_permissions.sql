-- 0245_evaluation_human_score_permissions.sql — the permissions of the human
-- score surface of Agent Evaluation (issue 2008).
--
--   models.applications.evaluation.human_score.read    list the human scores of a run
--   models.applications.evaluation.human_score.create  record a human score
--
-- Read goes to admin, editor and viewer (the same set as run.read). Create goes
-- to admin and editor. super_admin and system hold every permission by role, so
-- nothing is granted to them here. The rows are also written for projects that
-- override their role permissions (auth_core__project_role_permission), as
-- 0116 does.
--
-- Guarded like every grant migration: elitea-migrate runs against databases
-- whose auth_core tables come from elitea-main startup or a legacy dump.
DO $$
BEGIN

IF to_regclass('public.auth_core__role') IS NULL
   OR to_regclass('public.auth_core__role_permission') IS NULL THEN
    RAISE NOTICE '0245: auth_core tables absent, nothing to grant';
    RETURN;
END IF;

INSERT INTO public.auth_core__role_permission (role_id, permission)
SELECT role.id, 'models.applications.evaluation.human_score.read'
FROM public.auth_core__role AS role
WHERE role.mode = 'default' AND role.name IN ('admin', 'editor', 'viewer')
ON CONFLICT (role_id, permission) DO NOTHING;

INSERT INTO public.auth_core__role_permission (role_id, permission)
SELECT role.id, 'models.applications.evaluation.human_score.create'
FROM public.auth_core__role AS role
WHERE role.mode = 'default' AND role.name IN ('admin', 'editor')
ON CONFLICT (role_id, permission) DO NOTHING;

IF to_regclass('public.auth_core__project_role') IS NULL
   OR to_regclass('public.auth_core__project_role_permission') IS NULL THEN
    RAISE NOTICE '0245: no per-project permission tables, central grants are the whole story here';
    RETURN;
END IF;

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
    ('models.applications.evaluation.human_score.read', ARRAY['admin', 'editor', 'viewer']),
    ('models.applications.evaluation.human_score.create', ARRAY['admin', 'editor'])
) AS grant_row(permission, roles)
WHERE project_role.name = ANY (grant_row.roles)
ON CONFLICT (project_id, role_id, permission) DO NOTHING;

END
$$;
