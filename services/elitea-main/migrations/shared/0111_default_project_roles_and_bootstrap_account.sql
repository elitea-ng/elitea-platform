-- 0111_default_project_roles_and_bootstrap_account.sql — two fresh-install
-- repairs on the rows internal/infra/db/migrations/001_initial.sql seeds (F4).
--
-- Both are about the SAME thing: the bootstrap schema hand-writes rows that
-- every later project and account gets from code, and it writes fewer of them.
-- The bootstrap runs only when centry.project is absent (migrate.Bootstrap), so
-- both defects exist on a fresh install and on nothing else — which is why they
-- survived so long.
--
--
-- ## 1. "Default Project" (id 1) had no project roles
--
-- Every project the application creates gets four roles from
-- projectprovisioning's `project_permissions` step
-- (internal/application/projectprovisioning/steps.go): the central default-mode
-- role NAMES plus `system`, written to auth_core__project_role and nothing
-- else. Project 1 is written by hand in 001_initial.sql and skips that step, so
-- it has none.
--
-- With no rows there, nobody can be made a member of it: the member write
-- resolves the role names the client sends against auth_core__project_role for
-- that project (eliteacore/users_write.go resolveProjectRoleIDs) and rejects
-- every name. A project with no members never appears in any user's project
-- switcher, so the shared/AI project is invisible to the whole product.
--
-- ROLES ONLY, exactly as the provisioning step writes them. No
-- auth_core__project_role_permission row is created here. That is not an
-- omission: legacyrbac.projectPermissions() falls back to the CENTRAL
-- default-mode grants by role name, and that fallback is suppressed for any
-- project that carries per-project rows — so seeding permissions here would cut
-- project 1 off from every grant migration in this corpus.
--
-- The role list is derived from auth_core__role rather than spelled out, for
-- the same reason the Go step derives it: a deployment that added a
-- default-mode role would otherwise get a project 1 that disagrees with every
-- other project.
--
--
-- ## 2. The `dev@elitea.ai` bootstrap account held implicit global admin
--
-- 001_initial.sql creates user 1 and grants it `default|admin` and
-- `administration|admin`. It has no auth_core__user_provider row and no
-- password, so it cannot log in: the credential that would have used it,
-- AUTH_DEV_MODE, was removed by ADR-0017 and now fails startup
-- (cmd/elitea-main/main.go). The roles therefore grant nobody anything today.
--
-- They are not inert, though. The OIDC provisioning path resolves an identity
-- to an EXISTING account by e-mail and links the provider on first login
-- (GetAuthUserByEmailForProvisioning → LinkAuthProviderIfMissing). Anyone who
-- can obtain an identity-provider account at that address therefore becomes a
-- global administrator of a fresh install, silently, on first sign-in. That is
-- implicit admin, which AGENTS.md names as a correction compatibility does not
-- get to preserve.
--
-- THE ROW IS KEPT. Integration fixtures reference user 1 as an owner
-- (auth_core__project_user_role, centry.project.owner_id, centry.social_users),
-- and deleting it would cascade. Only the two central role assignments go, and
-- only while the account is still the untouched bootstrap seed:
--
--   * the e-mail is still `dev@elitea.ai`   — an operator who renamed it owns it
--   * no auth_core__user_provider row       — nobody has ever signed in as it
--   * last_login IS NULL                    — nor by any other credential
--
-- A deployment that deliberately adopted the account fails all three tests and
-- keeps everything. On such a deployment this migration writes nothing at all.
DO $$
DECLARE
    bootstrap_account_id CONSTANT INTEGER := 1;
    shared_project_id CONSTANT INTEGER := 1;
BEGIN

/* ── 1. the shared project's roles ─────────────────────────────────────── */

IF to_regclass('public.auth_core__project_role') IS NULL
   OR to_regclass('public.auth_core__role') IS NULL
   OR to_regclass('centry.project') IS NULL THEN
    RAISE NOTICE '0111: the project tables are absent, nothing to seed';
ELSIF EXISTS (SELECT 1 FROM centry.project WHERE id = shared_project_id) THEN
    INSERT INTO public.auth_core__project_role (project_id, name)
    SELECT shared_project_id, role_name
    FROM (
        SELECT name AS role_name FROM public.auth_core__role WHERE mode = 'default'
        UNION
        SELECT 'system'::text
    ) AS project_roles
    ON CONFLICT (project_id, name) DO NOTHING;
END IF;

/* ── 2. the bootstrap account's central roles ──────────────────────────── */

IF to_regclass('public.auth_core__user') IS NULL
   OR to_regclass('public.auth_core__user_role') IS NULL
   OR to_regclass('public.auth_core__user_provider') IS NULL THEN
    RAISE NOTICE '0111: the account tables are absent, nothing to revoke';
ELSE
    DELETE FROM public.auth_core__user_role AS assignment
    WHERE assignment.user_id = bootstrap_account_id
      AND EXISTS (
          SELECT 1
          FROM public.auth_core__user AS account
          WHERE account.id = bootstrap_account_id
            AND account.email = 'dev@elitea.ai'
            AND account.last_login IS NULL
      )
      AND NOT EXISTS (
          SELECT 1
          FROM public.auth_core__user_provider AS link
          WHERE link.user_id = bootstrap_account_id
      );
END IF;

END
$$;
