-- 0255_microsoft_oauth_apps.sql — the Default app registry for Microsoft
-- delegated credentials (ADR-0057 decisions 4, 5 and 11; epic 2254 story S1b,
-- issue 2267).
--
-- An administrator registers an Entra application once (client id, secret,
-- authority, the credential types it serves, the Graph permissions it may
-- ask for, the projects that may use it). A credential then names only the
-- app's id (`oauth_app_id`, "Default app"); Main resolves the client at every
-- use, so an admin change needs no user action and the client secret never
-- leaves Main.
--
--   elitea_identity.microsoft_oauth_apps         the registry
--   elitea_identity.microsoft_oauth_app_projects the explicit project set of an
--                                                app whose project_scope is
--                                                'selected'
--
-- THE SECRET IS SEALED, WRITE-ONLY. client_secret_ciphertext is AES-256-GCM
-- under a key derived with HKDF-SHA256 from the deployment master key under the
-- label `elitea/microsoft-oauth-apps/v1` (recorded in key_label with its
-- key_version). The AAD binds the ciphertext to the app id, client id and key
-- version, so a ciphertext copied to another row or kept after the client id
-- changes does not open. No API response, log or export carries it.
--
-- STATES. `draft` has no secret yet, `active` is usable, `disabled` was switched
-- off by an administrator, `needs_attention` was set by the platform (a secret
-- that does not open, a failed Test); state_reason carries the stable code. An
-- app that is not active is not offered for new credentials, and a credential
-- that already names it fails with a stable error instead of falling back to
-- another client.
--
-- NOTHING SHIPS ENABLED. An empty registry means a credential form shows only
-- Custom app. seed_key names a row that Helm (`microsoftOAuthApps`) manages, so a
-- re-run updates it instead of adding a second one. seed_hash is the keyed digest
-- of the Helm entry last applied: a restart writes the row only when the chart's
-- values changed, so an administrator's console edits are not reverted by it.
--
-- The permission that guards the admin surface is granted at the end, to
-- super_admin and admin in administration mode (the trap in 0060: a new central
-- permission needs its own migration). Additive and idempotent. No BEGIN/COMMIT:
-- the ledgered runner wraps the file.

CREATE SCHEMA IF NOT EXISTS elitea_identity;

CREATE TABLE IF NOT EXISTS elitea_identity.microsoft_oauth_apps (
    id                       uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    seed_key                 text        UNIQUE
                                         CHECK (seed_key IS NULL OR seed_key ~ '^[a-z0-9][a-z0-9-]{0,39}$'),
    seed_hash                text        CHECK (seed_hash IS NULL OR seed_hash ~ '^[0-9a-f]{64}$'),
    name                     text        NOT NULL CHECK (length(btrim(name)) BETWEEN 1 AND 80),
    description              text        NOT NULL DEFAULT '' CHECK (length(description) <= 300),
    provider                 text        NOT NULL DEFAULT 'entra' CHECK (provider = 'entra'),
    client_id                text        NOT NULL
                                         CHECK (client_id ~ '^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$'),
    client_secret_ciphertext bytea,
    key_label                text,
    key_version              integer     CHECK (key_version IS NULL OR key_version >= 1),
    secret_expires_at        timestamptz,
    authority                text        NOT NULL CHECK (authority ~ '^https://login\.microsoftonline\.com/[A-Za-z0-9.-]{1,255}$'),
    credential_types         text[]      NOT NULL
                                         CHECK (cardinality(credential_types) >= 1
                                                AND credential_types <@ ARRAY['outlook', 'teams', 'ms_graph', 'dataverse', 'sharepoint']::text[]),
    scope_ceiling            jsonb       NOT NULL DEFAULT '{}'::jsonb CHECK (jsonb_typeof(scope_ceiling) = 'object'),
    auto_refresh_token       boolean     NOT NULL DEFAULT true,
    project_scope            text        NOT NULL DEFAULT 'all' CHECK (project_scope IN ('all', 'selected')),
    state                    text        NOT NULL DEFAULT 'draft'
                                         CHECK (state IN ('draft', 'active', 'disabled', 'needs_attention')),
    state_reason             text        NOT NULL DEFAULT '' CHECK (length(state_reason) <= 64),
    created_by               integer,
    updated_by               integer,
    created_at               timestamptz NOT NULL DEFAULT now(),
    updated_at               timestamptz NOT NULL DEFAULT now(),
    revision                 integer     NOT NULL DEFAULT 1 CHECK (revision >= 1),
    CONSTRAINT microsoft_oauth_apps_secret_custody
        CHECK ((client_secret_ciphertext IS NULL) = (key_label IS NULL)
               AND (client_secret_ciphertext IS NULL) = (key_version IS NULL)),
    -- `draft` means no secret yet; a usable app has one.
    CONSTRAINT microsoft_oauth_apps_state_has_secret
        CHECK (state NOT IN ('active', 'needs_attention') OR client_secret_ciphertext IS NOT NULL)
);

CREATE UNIQUE INDEX IF NOT EXISTS microsoft_oauth_apps_name_key
    ON elitea_identity.microsoft_oauth_apps (lower(btrim(name)));

COMMENT ON COLUMN elitea_identity.microsoft_oauth_apps.client_secret_ciphertext IS
    'AES-256-GCM, write-only: no API response, log or export carries the secret (ADR-0057 decision 5).';

CREATE TABLE IF NOT EXISTS elitea_identity.microsoft_oauth_app_projects (
    app_id            uuid   NOT NULL REFERENCES elitea_identity.microsoft_oauth_apps (id) ON DELETE CASCADE,
    tenant_project_id bigint NOT NULL CHECK (tenant_project_id > 0),
    PRIMARY KEY (app_id, tenant_project_id)
);

CREATE INDEX IF NOT EXISTS microsoft_oauth_app_projects_project_idx
    ON elitea_identity.microsoft_oauth_app_projects (tenant_project_id);

-- The ADMINISTRATION-mode grant behind the Microsoft apps admin surface.
--
--   configuration.microsoft_apps  <- /admin/microsoft_oauth_apps (list, create,
--                                    update, disable, test, settings)
--
-- WHO HOLDS IT. super_admin and admin, like every central configuration grant:
-- registering an app decides which Entra client signs users in to Microsoft for
-- every project. Guarded and conflict-safe: elitea-migrate can run this corpus
-- against a database whose auth_core tables do not exist yet.
DO $$
BEGIN
    IF to_regclass('public.auth_core__role') IS NULL
       OR to_regclass('public.auth_core__role_permission') IS NULL THEN
        RAISE NOTICE '0255: auth_core tables absent, nothing to grant';
        RETURN;
    END IF;

    INSERT INTO public.auth_core__role_permission (role_id, permission)
    SELECT role.id, grant_row.permission
    FROM public.auth_core__role AS role
    CROSS JOIN (VALUES
        ('configuration.microsoft_apps')
    ) AS grant_row(permission)
    WHERE role.mode = 'administration' AND role.name IN ('super_admin', 'admin')
    ON CONFLICT (role_id, permission) DO NOTHING;
END
$$;
