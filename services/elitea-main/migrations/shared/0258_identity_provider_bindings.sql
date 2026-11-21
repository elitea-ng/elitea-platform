-- 0258_identity_provider_bindings.sql — bind every external identity to the
-- provider and issuer that asserted it (issue #2558, ADR-0059 review R3B-1).
--
-- THE DEFECT. An account's federated link is one row of
-- public.auth_core__user_provider: `oidc:<sub>` or `saml:<NameID>`. The row names
-- no provider and no issuer. So whoever could repoint the enabled provider at an
-- identity provider they control (a new issuer, metadata URL or signing
-- certificate) could assert the subject of any account, a super_admin's
-- included, and sign in as it.
--
-- THE FIX. A sign-in now matches (provider key, issuer, subject), and this
-- table holds the first two for each link. A link with no binding for the
-- provider that is signing in is refused (409), so a provider whose issuer
-- changed no longer matches the links made under the old issuer. A super_admin
-- moves them on purpose with
-- POST /api/v2/admin/identity_providers/administration/{key}/rebind.
--
--   provider_ref  the link this binds: auth_core__user_provider.provider_ref
--   provider_key  elitea_auth.identity_providers.provider_key, or
--                 '@environment' for the provider configured by OIDC_ISSUER_URL
--                 (no stored key can contain '@')
--   issuer        the provider's configured trust identifier: the OIDC issuer,
--                 or the SAML IdP entity id
--   user_id       the account; the same as the link's
--
-- The same subject may be bound to more than one (provider, issuer): every
-- binding names its own provider, and each one is checked on its own.
--
-- THE BACKFILL (upgrade safety). Each existing `oidc:` link is bound to the
-- ENABLED stored OIDC provider and its current issuer, and each `saml:` link to
-- the enabled stored SAML provider and its IdP entity id. Those are the
-- providers the links sign in through today, so every user keeps signing in. An
-- `oidc:` subject and a `saml:` subject with the same text are two links, and
-- each is bound to its own provider. A link whose protocol has no enabled
-- stored provider is left unbound:
--   * an OIDC link on a deployment configured by OIDC_ISSUER_URL is bound by
--     its next sign-in through that environment provider, which only an
--     operator with workload access can configure;
--   * a link that a replica of the previous release writes while the upgrade
--     rolls out is bound at its next sign-in, but only to a stored provider
--     that has not been saved since this migration was applied
--     (identityrepo.BindUnboundLinkToUnchangedProvider), so never to one
--     repointed after the upgrade;
--   * any other unbound link is refused until a super_admin binds it with the
--     rebind route's `from_unbound`.
-- Bare refs (the operator-configured Form logins) and invite receipts carry no
-- protocol prefix and are not bound. The Form plane (identityrepo Provision)
-- resolves only those; it refuses an `oidc:` or `saml:` reference outright, so
-- the bound resolution in api/v2/auth is the only way to a federated link.
--
-- NO UNCONDITIONAL FOREIGN KEY, for the reason 0096 states: the shared corpus
-- must apply to a database that holds nothing else. The key to the link (ON
-- DELETE and ON UPDATE CASCADE, so a binding never outlives its link) is added
-- only when public.auth_core__user_provider exists, as 0141 does for its anchor.
-- The sign-in reads also join the link, so a binding without one is inert.
--
-- IDEMPOTENT throughout. No BEGIN/COMMIT: the ledgered runner executes each file
-- inside one transaction with its ledger row (migrate/runner.go apply).

CREATE SCHEMA IF NOT EXISTS elitea_auth;

CREATE TABLE IF NOT EXISTS elitea_auth.identity_bindings (
    provider_ref text        NOT NULL,
    provider_key text        NOT NULL,
    issuer       text        NOT NULL,
    user_id      integer     NOT NULL,
    created_at   timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (provider_ref, provider_key, issuer),
    CONSTRAINT identity_bindings_key_present CHECK (length(btrim(provider_key)) > 0),
    CONSTRAINT identity_bindings_issuer_present CHECK (length(btrim(issuer)) > 0)
);

-- The rebind route selects by (provider_key, issuer).
CREATE INDEX IF NOT EXISTS identity_bindings_provider_idx
    ON elitea_auth.identity_bindings (provider_key, issuer);

DO $$
BEGIN
    IF to_regclass('public.auth_core__user_provider') IS NULL THEN
        RAISE NOTICE '0258: auth_core absent, identity binding foreign key and backfill skipped';
        RETURN;
    END IF;

    IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'identity_bindings_link_fkey') THEN
        ALTER TABLE elitea_auth.identity_bindings
            ADD CONSTRAINT identity_bindings_link_fkey
            FOREIGN KEY (user_id, provider_ref)
            REFERENCES public.auth_core__user_provider (user_id, provider_ref)
            ON DELETE CASCADE ON UPDATE CASCADE;
    END IF;

    IF to_regclass('elitea_auth.identity_providers') IS NULL THEN
        RETURN;
    END IF;

    -- The enabled provider of each kind. identity_providers_enabled_kind_uniq
    -- holds at most one per kind, so each link gets at most one binding here.
    INSERT INTO elitea_auth.identity_bindings (provider_ref, provider_key, issuer, user_id)
    SELECT link.provider_ref, provider.provider_key, provider.issuer, link.user_id
    FROM public.auth_core__user_provider AS link
    JOIN (
        SELECT provider_key,
               kind,
               btrim(CASE kind
                         WHEN 'oidc' THEN config ->> 'issuer'
                         WHEN 'saml' THEN config ->> 'idp_entity_id'
                     END) AS issuer
        FROM elitea_auth.identity_providers
        WHERE enabled
    ) AS provider
      ON left(link.provider_ref, length(provider.kind) + 1) = provider.kind || ':'
    WHERE coalesce(provider.issuer, '') <> ''
    ON CONFLICT (provider_ref, provider_key, issuer) DO NOTHING;
END
$$;
