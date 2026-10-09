-- 0158_social_feedbacks_project.sql — the shared feedback table, with a project.
--
-- WHY. Feedback lives in centry.social_feedbacks, the legacy (pylon) shared
-- table: one row per submission, keyed by author, with no project. Nothing in
-- this repository ever created it; it exists only on databases carried over
-- from legacy, so a fresh install had no table to write to or list from. The
-- Go routes had been reading and writing a p_N.social_feedbacks that no tenant
-- migration creates either, so the listing answered 500.
--
-- WHAT. The legacy table shape is created if absent (existing tables and rows
-- are untouched), and gains a nullable project_id. Legacy rows keep NULL: they
-- were written without a project and stay visible only to their author. New
-- rows record the project they were submitted in, so a project's listing shows
-- that project's feedback and nothing else.
--
-- THE LIST GRANT. The list route gates on `models.social.feedbacks.list`, which
-- no migration granted (0080 granted only `.create`), so on a clean database the
-- listing would answer 403 to everyone. testdata/postgres/legacy-rbac-matrix.json
-- gives it to default-mode `admin`, `editor` and `viewer`, exactly as it gives
-- `.create`; this file copies that split. `system` and `super_admin` are omitted,
-- as everywhere else in this corpus. Like 0156, a second block hands it to every
-- project role whose saved matrix holds `.create` (legacyrbac reads the central
-- grants only for a caller with no per-project rows). Additive; never revokes.
--
-- No foreign key and no default: the column is a label, not an ownership
-- constraint, and the project row may be deleted while feedback is kept.
-- No tenant (p_N) table is created.
--
-- Idempotent. The indexes are plain (not CONCURRENTLY): the runner applies a
-- file in one transaction, and the table is small (one row per submission).
CREATE TABLE IF NOT EXISTS centry.social_feedbacks (
    id          SERIAL PRIMARY KEY,
    user_id     INTEGER NOT NULL,
    referrer    VARCHAR,
    description TEXT NOT NULL,
    rating      INTEGER NOT NULL,
    user_agent  VARCHAR,
    created_at  TIMESTAMP WITHOUT TIME ZONE NOT NULL DEFAULT now()
);

ALTER TABLE centry.social_feedbacks ADD COLUMN IF NOT EXISTS project_id INTEGER NULL;

CREATE INDEX IF NOT EXISTS social_feedbacks_project_id_idx
    ON centry.social_feedbacks (project_id, id);

-- Legacy rows (no project) are listed to their author only.
CREATE INDEX IF NOT EXISTS social_feedbacks_legacy_author_idx
    ON centry.social_feedbacks (user_id, id) WHERE project_id IS NULL;

DO $$
BEGIN

IF to_regclass('public.auth_core__role') IS NULL
   OR to_regclass('public.auth_core__role_permission') IS NULL THEN
    RAISE NOTICE '0158: auth_core tables absent, nothing to grant';
    RETURN;
END IF;

INSERT INTO public.auth_core__role_permission (role_id, permission)
SELECT role.id, 'models.social.feedbacks.list'
FROM public.auth_core__role AS role
WHERE role.mode = 'default' AND role.name IN ('admin', 'editor', 'viewer')
ON CONFLICT (role_id, permission) DO NOTHING;

IF to_regclass('public.auth_core__project_role') IS NULL
   OR to_regclass('public.auth_core__project_role_permission') IS NULL THEN
    RAISE NOTICE '0158: no per-project permission tables, central grants are the whole story here';
    RETURN;
END IF;

INSERT INTO public.auth_core__project_role_permission (project_id, role_id, permission)
SELECT DISTINCT held.project_id, held.role_id, 'models.social.feedbacks.list'
FROM public.auth_core__project_role_permission AS held
JOIN public.auth_core__project_role AS project_role
  ON project_role.id = held.role_id
 AND project_role.project_id = held.project_id
WHERE held.role_id IS NOT NULL
  AND held.permission = 'models.social.feedbacks.create'
  AND project_role.name IN ('admin', 'editor', 'viewer')
ON CONFLICT (project_id, role_id, permission) DO NOTHING;

END
$$;
