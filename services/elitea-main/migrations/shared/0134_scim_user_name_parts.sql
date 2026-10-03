-- 0134_scim_user_name_parts.sql — the SCIM `name` sub-attributes, stored.
--
-- WHY. Microsoft Entra ID maps three name attributes by default, and it maps
-- them INDEPENDENTLY of `displayName`:
--
--   givenName                     → name.givenName
--   surname                       → name.familyName
--   Join(" ", givenName, surname) → name.formatted
--   displayName                   → displayName
--
-- and it sends only the attributes that changed. This service stored none of
-- the three: it read them, folded them into `auth_core__user.name`, and
-- answered every GET with `name.formatted` = the display name and no given or
-- family name. Entra then saw a difference on every user whose display name is
-- not exactly "Given Family" ("Smith, John (Contractor)") and PATCHed the name
-- again on every cycle, and each such PATCH overwrote the display name the
-- operator had chosen. A lone `name.familyName` change was accepted with 200
-- and dropped. Storing the parts is what lets a GET answer with what the
-- client sent, so the client converges instead of re-sending forever.
--
-- WHERE. On the SCIM side table 0096 created, keyed by user id, for the reason
-- 0096's header gives: `auth_core__user` is pylon-owned and is read and
-- updated, never reshaped. The display name stays `auth_core__user.name`.
--
-- WHAT THEY DO NOT DO. The parts never overwrite a stored display name; they
-- fill an EMPTY one only (internal/api/scim/patch_user.go). `displayName` is the
-- attribute the platform shows, and the client manages it directly.
--
-- GUARDED on to_regclass. 0096 creates the table in this same corpus and always
-- runs first, so the guard does not fire on any ledgered database; it keeps the
-- file applicable on its own against a database that holds nothing else, which
-- is the rule 0096's header states for this corpus.
--
-- IDEMPOTENT. No BEGIN/COMMIT: the ledgered runner executes each file inside one
-- transaction with its ledger row (migrate/runner.go apply).

DO $$
BEGIN
    IF to_regclass('elitea_auth.scim_users') IS NULL THEN
        RAISE NOTICE '0134: elitea_auth.scim_users does not exist, nothing to extend';
        RETURN;
    END IF;

    ALTER TABLE elitea_auth.scim_users
        ADD COLUMN IF NOT EXISTS given_name     text NOT NULL DEFAULT '',
        ADD COLUMN IF NOT EXISTS family_name    text NOT NULL DEFAULT '',
        ADD COLUMN IF NOT EXISTS formatted_name text NOT NULL DEFAULT '';
END
$$;
