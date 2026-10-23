-- 0080_social_permissions.sql — the DEFAULT-mode grants for the social surface
-- (#386).
--
--   models.social.authors.get
--   models.social.avatar.get
--   models.social.avatar.update
--   models.social.feedbacks.create
--
-- WHY THIS FILE EXISTS. Each permission above gates a mounted route. No
-- migration in this corpus grants any of them. So every author name, every
-- avatar and every feedback form answers 403 on a clean database.
--
-- WHY THE JOURNEYS DID NOT CATCH IT. The end-to-end seed grants these strings
-- per project. A journey that passes on the seeded database proves nothing about
-- a clean one. #161 records the same trap on the same surface.
--
-- THE ROUTES. internal/api/v2/social/authors.go, avatar.go and feedback.go
-- register them. Every route resolves `auth.PermissionModeDefault`, so this file
-- grants in the `default` mode only.
--
-- WHERE THE SPLIT COMES FROM. testdata/postgres/legacy-rbac-matrix.json gives
-- `models.social.authors.get` and `models.social.feedbacks.create` to `admin`,
-- `editor` and `viewer` in `default` mode. It gives `models.social.avatar.get`
-- to `admin` only. This file copies that split. It makes no new policy.
--
-- ONE STRING HAS NO MATRIX ENTRY: `models.social.avatar.update`. The matrix
-- names the legacy avatar write `models.social.avatar.post`, and gives it to
-- `admin` only, exactly as it gives the read. The Go route renamed the same
-- operation from `.post` to `.update`; see avatar.go:25. So this file gives
-- `.update` the holders the matrix gives `.post`. That is a transcription of the
-- nearest legacy entry, not an invented split.
--
-- A CONSEQUENCE TO READ BEFORE YOU APPROVE. The two avatar routes are
-- PER-USER: avatar.go scopes them by `user_id`, so they read and write the
-- CALLER's own avatar. The matrix still withholds both from the default-mode
-- editor and viewer. So an editor or a viewer cannot see or change their own
-- avatar after this file applies. That is legacy parity, and it is very probably
-- a legacy defect. This file keeps parity, because widening a grant is a policy
-- change and #386 forbids an invented split. Correcting it needs its own issue.
--
-- The matrix also gives all four to `system` and to `super_admin`. This file
-- omits both, as every other file in this corpus does. Go seeds neither role in
-- the default mode.
--
-- BLAST RADIUS. legacyrbac's projectPermissions() reads the CENTRAL default-mode
-- grants by role NAME. It reads them only for a project that carries NO
-- per-project auth_core__project_role_permission rows. That shape is the fresh
-- Go database. It is never the shape of a legacy-backed database, of a legacy
-- dump, or of the end-to-end stack. Each one seeds per-project rows, and those
-- rows suppress the central fallback completely. So no existing deployment's
-- members gain anything here.
--
-- The fallback also joins THROUGH the caller's assigned project roles. So a
-- non-member has no row to fall back from, and gains nothing at all.
--
-- 0060's VIRGIN-mode guard is NOT reproduced here, for the reason 0061 and 0072
-- state. These permissions never existed on a Go deployment, so no operator can
-- have revoked them.
DO $$
BEGIN

IF to_regclass('public.auth_core__role') IS NULL
   OR to_regclass('public.auth_core__role_permission') IS NULL THEN
    RAISE NOTICE '0080: auth_core tables absent, nothing to grant';
    RETURN;
END IF;

-- Granted to admin, editor and viewer. A viewer may read an author name, and
-- may leave feedback.
INSERT INTO public.auth_core__role_permission (role_id, permission)
SELECT role.id, grant_row.permission
FROM public.auth_core__role AS role
CROSS JOIN (VALUES
    ('models.social.authors.get'),
    ('models.social.feedbacks.create')
) AS grant_row(permission)
WHERE role.mode = 'default' AND role.name IN ('admin', 'editor', 'viewer')
ON CONFLICT (role_id, permission) DO NOTHING;

-- Granted to admin only. The legacy matrix withholds the avatar read from the
-- default-mode editor and viewer, and gives the avatar write the same holders.
INSERT INTO public.auth_core__role_permission (role_id, permission)
SELECT role.id, grant_row.permission
FROM public.auth_core__role AS role
CROSS JOIN (VALUES
    ('models.social.avatar.get'),
    ('models.social.avatar.update')
) AS grant_row(permission)
WHERE role.mode = 'default' AND role.name = 'admin'
ON CONFLICT (role_id, permission) DO NOTHING;

END
$$;
