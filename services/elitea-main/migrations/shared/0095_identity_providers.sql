-- 0095_identity_providers.sql — the TYPED identity provider revision: the
-- authored definition of how this deployment federates a login.
--
-- WHAT THIS REPLACES. The admin Configuration page's "Authentication" section
-- describes a legacy runtime plugin's YAML. `auth_provider: form | oidc`
-- selects which of the legacy auth runtime's plugins registers the login
-- routes, and the OIDC values (`metadata_endpoint`, `client_id`,
-- `client_secret`) are keys inside `auth_oidc/config.yml`.
-- `auth_saml/config.yml` holds the parallel set (`sp_key`, `sp_cert`,
-- `idp_cert`, `authn_destination`, `saml_issuer`). The reference page edits
-- them by shipping patched plugin YAML over the Arbiter bus, and a legacy
-- runtime reloads its descriptor.
--
-- This service loads no plugins. Until now the only federation configuration it
-- had was four environment variables read once at boot
-- (`internal/api/v2/auth/oidc.go:OIDCConfigFromEnv`), so an operator could not
-- change an identity provider without a redeploy, and the admin section that
-- exists to change one refused with a sentence about the Arbiter bus.
--
-- WHY ONE ROW PER PROVIDER AND NOT A SETTINGS DOCUMENT. The configuration
-- provenance specification requires "one typed OIDC provider revision owns all
-- values" and "one typed SAML provider revision", because the legacy runtime
-- spreads response type, scopes, endpoints, client authentication and
-- verification behaviour through code at three separate call sites, and a flat
-- settings document reproduces exactly that scattering. A row is the revision:
-- `config` carries the whole typed document for one provider, `revision` counts
-- its authored versions, and no value that the login path reads lives anywhere
-- else.
--
-- WHY NOT `centry.platform_config`. Two reasons, and either alone is decisive.
-- An OIDC client secret and a SAML service-provider private key are
-- credentials, and `internal/api/v2/admin/config_values.go:rejectCredentialField`
-- refuses a credential into those rows — correctly, because every holder of
-- `runtime.plugins` can read them. And the plugin-config write path stores a
-- section as untyped field values, which cannot express "this document is an
-- OIDC provider and these are the invariants it must satisfy".
--
-- SECRETS ARE NOT HERE. `secret_ref` names an entry in the GLOBAL vault's
-- hidden bucket (`centry.secrets_key`/`secrets_data`, vault id `admin`, see
-- `internal/api/v2/secrets/admin_hidden.go`). The hidden bucket is excluded
-- from the merge that project workloads interpolate `{{secret.<name>}}`
-- against, so a client secret stored there cannot be read back by an agent.
-- This table holds the reference, never the value. `config` is non-secret by
-- construction: issuer URLs, client identifiers, scopes, certificates of the
-- identity provider (a certificate is public material), and the algorithm and
-- clock-skew policy.
--
-- AT MOST ONE ENABLED PROVIDER PER KIND. The router mounts one route set per
-- protocol — `/forward-auth/auth_oidc/*` and `/forward-auth/auth_saml/*`. Two
-- enabled OIDC rows would mean two definitions competing for one callback URL,
-- and the login path would have to pick one with no rule for which. The partial
-- unique index makes that unrepresentable rather than resolved arbitrarily at
-- request time. An operator who prepares a replacement provider leaves it
-- disabled and flips the pair in one transaction.
--
-- A DISABLED ROW KEEPS ITS SECRET REFERENCE. That is why `enabled` exists at
-- all instead of the operator deleting the row: withdrawing a provider must not
-- destroy the sealed credential that restoring it would need.
--
-- REVISION IS MONOTONIC PER ROW, and it is what lets a reader cache a
-- constructed provider. OIDC discovery is a network round trip, and the login
-- path must not make one per request. The resolver caches by
-- (provider_key, revision), so an authored change invalidates the cache exactly
-- when the document changed and never otherwise.
--
-- NO FOREIGN KEYS. The table references nothing. A provider is a deployment
-- fact, not a tenant's.
--
-- WHY SHARED, NOT TENANT. Federation is platform-wide: the browser reaches
-- `/forward-auth/auth_oidc/login` before any project is selected, so there is
-- no tenant in scope when the definition is read.
--
-- IDEMPOTENT throughout. No BEGIN/COMMIT: the ledgered runner executes each
-- file inside one transaction with its ledger row (migrate/runner.go apply).

CREATE SCHEMA IF NOT EXISTS elitea_auth;

CREATE TABLE IF NOT EXISTS elitea_auth.identity_providers (
    id            bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    -- provider_key is the stable slug an operator names the definition by. It
    -- is the URL segment of the admin surface, so it is normalised to lower
    -- case with underscores before it is stored.
    provider_key  text        NOT NULL,
    -- kind selects the protocol AND the shape of `config`. The check constraint
    -- lists exactly the kinds a reader in this service switches on; a row of an
    -- unknown kind would be a definition no login path can honour.
    kind          text        NOT NULL,
    -- display_name is what the admin page and the login chooser show.
    display_name  text        NOT NULL,
    -- enabled is whether the login path mounts this definition. New rows arrive
    -- disabled: an operator authors the document first and turns it on after,
    -- rather than replacing a working provider halfway through typing one.
    enabled       boolean     NOT NULL DEFAULT false,
    -- revision counts authored versions of this row's document. See the cache
    -- note above; it is read, not decorative.
    revision      integer     NOT NULL DEFAULT 1,
    -- config is the whole typed document for this provider, minus its secret.
    -- Go validates it against the kind before it is written; the constraint
    -- below is the last line, not the first.
    config        jsonb       NOT NULL DEFAULT '{}'::jsonb,
    -- secret_ref names the vault entry holding this provider's one secret: the
    -- OIDC client secret, or the SAML service-provider private key. Empty means
    -- the definition has no secret, which is distinct from "the secret is
    -- empty" — a public OIDC client using PKCE alone is the first case.
    secret_ref    text        NOT NULL DEFAULT '',
    created_at    timestamptz NOT NULL DEFAULT now(),
    updated_at    timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT identity_providers_key_present
        CHECK (length(btrim(provider_key)) > 0),
    CONSTRAINT identity_providers_display_name_present
        CHECK (length(btrim(display_name)) > 0),
    CONSTRAINT identity_providers_kind_known
        CHECK (kind IN ('oidc', 'saml')),
    CONSTRAINT identity_providers_revision_positive
        CHECK (revision > 0),
    -- config is an object, not an array or a scalar. A malformed value would
    -- reach the typed decoder as something it cannot open.
    CONSTRAINT identity_providers_config_object
        CHECK (jsonb_typeof(config) = 'object')
);

-- One definition per slug. A second save of the same name replaces the first.
CREATE UNIQUE INDEX IF NOT EXISTS identity_providers_key_uniq
    ON elitea_auth.identity_providers (provider_key);

-- At most one ENABLED definition per protocol. See the header: the router
-- mounts one route set per protocol, so a second enabled row of the same kind
-- is a definition that could never be reached.
CREATE UNIQUE INDEX IF NOT EXISTS identity_providers_enabled_kind_uniq
    ON elitea_auth.identity_providers (kind)
    WHERE enabled;
