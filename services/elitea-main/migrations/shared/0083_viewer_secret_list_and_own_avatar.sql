-- 0083_viewer_secret_list_and_own_avatar.sql — two DEFAULT-mode grants that
-- DEVIATE from the legacy role matrix (#402).
--
--   configuration.secrets.secret.list   → add `viewer`
--   models.social.avatar.get            → add `editor` and `viewer`
--   models.social.avatar.update         → add `editor` and `viewer`
--
-- THIS FILE ADDS NO NEW PERMISSION NAME. Each of the three strings is already
-- in the catalogue: 0075 grants the first to `admin` and `editor`, and 0080
-- grants the other two to `admin`. This file only adds holders.
--
-- READ THIS BEFORE YOU COPY THE PATTERN. Every other grant file in this corpus
-- transcribes testdata/postgres/legacy-rbac-matrix.json exactly, and that is
-- still the rule. This file does not. It is a deliberate product decision on
-- two splits that #401 transcribed correctly and that #402 then asked about.
-- The reasons are below, one per split. Do not widen a third split by pointing
-- at this file.
--
-- ============================================================================
-- SPLIT 1 — `configuration.secrets.secret.list` goes to the viewer
-- ============================================================================
--
-- THE MATRIX. legacy-rbac-matrix.json gives all six project secret strings to
-- `admin` and `editor` in `default` mode, and gives the viewer none of them.
-- 0075 copied that. So a viewer gets 403 on the secrets surface.
--
-- WHY THAT IS WRONG FOR THE PRODUCT.
--
--  1. The LIST route discloses no secret value. internal/api/v2/secrets/
--     handler.go:317 `List` answers with SecretListItem, which carries `name`,
--     `secret_name` (the literal `{{secret.<name>}}` template reference) and
--     `is_default`. The VALUE is a different route on a different path,
--     GET /secret/{mode}/{projectID}/{name}, and it is gated on
--     `configuration.secrets.secret.unsecret`. That gate does not change here.
--     A viewer still cannot read a value, and still cannot create, edit, delete
--     or hide one.
--
--  2. A viewer must be able to see that a secret EXISTS. A toolkit and an agent
--     hold their credentials as the `{{secret.<name>}}` reference. A viewer
--     reads that configuration. With no list, a viewer cannot tell a missing
--     secret from a wrong value, so a viewer cannot say why a toolkit fails.
--
--  3. The product already models this exact split, and only the grant was
--     missing. apps/elitea-web/src/pages/settings/Secrets.tsx:124-125 computes
--     `canList` and `canUnsecret` as two separate checks: `canList` enables the
--     listing query and `canUnsecret` enables the reveal control. The screen was
--     written for a role that may list and may not reveal. No such role existed.
--
--  4. The Secrets page carries no permission gate in the settings navigation.
--     So a viewer reaches it today and finds a table that is empty for ever,
--     with no statement of why.
--
-- WHY THE MATRIX IS NOT AUTHORITY HERE. The matrix does not draw the name/value
-- line anywhere on this surface. It gives the viewer neither `list` nor
-- `unsecret`. So it records a blanket exclusion of the viewer from the secrets
-- plugin, not a judgement that a secret NAME is sensitive. This file keeps the
-- part of that exclusion that protects the value, and drops the part that
-- protects the name.
--
-- THE FIVE STRINGS THIS FILE DOES NOT TOUCH. `unsecret`, `create`, `edit`,
-- `delete` and `hide` keep the 0075 holders, `admin` and `editor`. A viewer that
-- could read a value would be a real widening, and it is refused.
--
-- ============================================================================
-- SPLIT 2 — every project role may act on its OWN avatar
-- ============================================================================
--
-- THE MATRIX. legacy-rbac-matrix.json gives `models.social.avatar.get` to
-- `admin` alone in `default` mode. It has no entry for
-- `models.social.avatar.update`; 0080 mapped that string onto the legacy name
-- for the same operation, `models.social.avatar.post`, which is also `admin`
-- alone. So an editor and a viewer cannot see or set their own picture.
--
-- WHY THAT IS WRONG FOR THE PRODUCT.
--
--  1. The two routes are PER-USER. internal/api/v2/social/avatar.go reads the
--     user id from the authenticated principal (`currentAvatarUserID`, which
--     calls `auth.User.OwningUserID`) and stores by `user_id` in
--     centry.social_users. No path parameter, no query parameter and no body
--     field names a user. So the permission protects no other person's data. It
--     only decides whether the caller may act on the caller.
--
--  2. A profile picture belongs to the person, not to the project. A project
--     role must not decide whether a person may set their own picture.
--
--  3. Every project member has a profile. The member list, the chat participant
--     list and the author badge all show it. An editor and a viewer therefore
--     need the same self-service an admin has.
--
-- WHY THE GATE STAYS. #402 asks whether a per-user route should carry a
-- project-role gate at all. It must, for three measured reasons.
--
--  1. The `{projectID}` in the path SELECTS A STORAGE NAMESPACE on the write.
--     avatar.go builds `storage.NewObjectRef(projectID, "avatars", filename)`.
--     Drop the gate and any authenticated user could write an object into any
--     project's storage. `DownloadAvatar` is unauthenticated by design, so the
--     object would then be public under another tenant's prefix.
--
--  2. The gate is the only step that resolves a TOKEN principal to its owning
--     user id on this route. RequireResolvedPermissionsForProject sets
--     `user.UserID` from the resolution, and `OwningUserID()` answers false for
--     a token principal whose `UserID` is empty. Drop the gate and every
--     personal-access-token caller gets 401 from the handler.
--
--  3. Go seeds exactly three default-mode roles: `admin`, `editor` and `viewer`
--     (001_initial.sql). legacyrbac's projectPermissions() joins the central
--     fallback THROUGH the caller's assigned project roles. So a grant to all
--     three IS the membership check, and a non-member is still refused. The
--     correct gate and the widened grant are the same thing here, and the grant
--     costs no new code and no new failure mode.
--
-- THE ROLE MATRIX NOTE, AFTER THIS FILE
--
--   configuration.secrets.secret.list      admin, editor, viewer  (deviates)
--   configuration.secrets.secret.unsecret  admin, editor          (matrix)
--   configuration.secrets.secret.create    admin, editor          (matrix)
--   configuration.secrets.secret.edit      admin, editor          (matrix)
--   configuration.secrets.secret.delete    admin, editor          (matrix)
--   configuration.secrets.secret.hide      admin, editor          (matrix)
--   models.social.avatar.get               admin, editor, viewer  (deviates)
--   models.social.avatar.update            admin, editor, viewer  (deviates)
--
-- BLAST RADIUS. legacyrbac's projectPermissions() reads the CENTRAL
-- default-mode grants by role NAME, and only for a project that carries NO
-- per-project auth_core__project_role_permission rows. That shape is the fresh
-- Go database. It is never the shape of a legacy-backed database, of a legacy
-- dump, or of the end-to-end stack: each one seeds per-project rows, and those
-- rows suppress the central fallback completely. So no existing deployment's
-- members gain anything here.
--
-- 0060's VIRGIN-mode guard is NOT reproduced here, for the reason 0061, 0072 and
-- 0075 state. These permissions never existed on a Go deployment before #386, so
-- no operator can have revoked them.
DO $$
BEGIN

IF to_regclass('public.auth_core__role') IS NULL
   OR to_regclass('public.auth_core__role_permission') IS NULL THEN
    RAISE NOTICE '0083: auth_core tables absent, nothing to grant';
    RETURN;
END IF;

-- Split 1. The secret NAME listing, and nothing else on that surface.
INSERT INTO public.auth_core__role_permission (role_id, permission)
SELECT role.id, 'configuration.secrets.secret.list'
FROM public.auth_core__role AS role
WHERE role.mode = 'default' AND role.name = 'viewer'
ON CONFLICT (role_id, permission) DO NOTHING;

-- Split 2. The caller's own avatar, for every default-mode role Go seeds.
INSERT INTO public.auth_core__role_permission (role_id, permission)
SELECT role.id, grant_row.permission
FROM public.auth_core__role AS role
CROSS JOIN (VALUES
    ('models.social.avatar.get'),
    ('models.social.avatar.update')
) AS grant_row(permission)
WHERE role.mode = 'default' AND role.name IN ('editor', 'viewer')
ON CONFLICT (role_id, permission) DO NOTHING;

END
$$;
