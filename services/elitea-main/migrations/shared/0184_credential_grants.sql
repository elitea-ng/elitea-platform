-- 0184_credential_grants.sql — the credential grant model behind the
-- AND-composed operation decision (ADR-0039 decisions 7-8, issue #1116).
--
--   target_bindings         the canonical provider target a credential is
--                           for (scheme + host + base path, or a provider
--                           tenant/site id).
--   credential_references   where a governed credential lives: backend and a
--                           backend-relative locator. Never a value.
--   agent_toolkit_bindings  the toolkit (and its digest) an agent binding
--                           may run.
--   credential_use_grants   the exact grant: agent binding x toolkit x
--                           credential reference x target, with its
--                           capabilities, operation and resource scopes and
--                           obligations.
--
-- A grant's target is the target of its credential reference: the composite
-- foreign key below refuses a grant that names one target and a credential
-- bound to another. Every grant state change bumps epoch by one under
-- SELECT ... FOR UPDATE, records its actor and appends a security audit event
-- (0183 outbox) in the same transaction. The approver differs from the
-- proposer, in code and in the CHECK below.
--
-- The five permissions models.credentials.governed.{use,reveal,manage,probe,
-- approve} are declared by the service (the Roles matrix lists them) and
-- granted to NO role here: an operator grants them per deployment. So this
-- file writes no auth_core row.
--
-- With ELITEA_ENTERPRISE_CREDENTIAL_GOVERNANCE_ENABLED off nothing writes or
-- reads these tables. Additive only; no change to existing tables.
-- Idempotent throughout. No BEGIN/COMMIT: the ledgered runner wraps the file.

CREATE SCHEMA IF NOT EXISTS elitea_identity;

CREATE TABLE IF NOT EXISTS elitea_identity.target_bindings (
    id                 uuid PRIMARY KEY,
    tenant_project_id  bigint NOT NULL CHECK (tenant_project_id > 0),
    provider_type      text NOT NULL CHECK (provider_type ~ '^[a-z0-9][a-z0-9_-]{0,63}$'),
    target_identity    text NOT NULL CHECK (length(target_identity) BETWEEN 1 AND 512),
    audience           text NOT NULL DEFAULT '' CHECK (length(audience) <= 512),
    created_at         timestamptz NOT NULL DEFAULT clock_timestamp(),
    CONSTRAINT target_bindings_tenant_key UNIQUE (id, tenant_project_id)
);

-- One target row per tenant project, provider and canonical identity.
CREATE UNIQUE INDEX IF NOT EXISTS target_bindings_one_per_identity
    ON elitea_identity.target_bindings (tenant_project_id, provider_type, target_identity, audience);

CREATE TABLE IF NOT EXISTS elitea_identity.credential_references (
    id                 uuid PRIMARY KEY,
    tenant_project_id  bigint NOT NULL CHECK (tenant_project_id > 0),
    class              text NOT NULL
                       CHECK (class IN ('static_secret', 'oauth_delegated', 'oauth_service', 'dynamic_lease')),
    backend            text NOT NULL CHECK (backend IN ('pgvault', 'vault')),
    -- Backend-relative path. Never a value.
    locator            text NOT NULL CHECK (length(locator) BETWEEN 1 AND 512),
    provider_type      text NOT NULL CHECK (provider_type ~ '^[a-z0-9][a-z0-9_-]{0,63}$'),
    target_binding_id  uuid NOT NULL,
    generation         bigint NOT NULL DEFAULT 0 CHECK (generation >= 0),
    state              text NOT NULL DEFAULT 'staged'
                       CHECK (state IN ('staged', 'active', 'retiring', 'retired')),
    created_at         timestamptz NOT NULL DEFAULT clock_timestamp(),
    updated_at         timestamptz NOT NULL DEFAULT clock_timestamp(),
    CONSTRAINT credential_references_scope_key UNIQUE (id, tenant_project_id, target_binding_id),
    CONSTRAINT credential_references_target_same_tenant
        FOREIGN KEY (target_binding_id, tenant_project_id)
        REFERENCES elitea_identity.target_bindings (id, tenant_project_id)
);

CREATE TABLE IF NOT EXISTS elitea_identity.agent_toolkit_bindings (
    agent_binding_id   uuid NOT NULL,
    toolkit_id         bigint NOT NULL CHECK (toolkit_id > 0),
    tenant_project_id  bigint NOT NULL CHECK (tenant_project_id > 0),
    toolkit_digest     bytea NOT NULL CHECK (octet_length(toolkit_digest) = 32),
    created_at         timestamptz NOT NULL DEFAULT clock_timestamp(),
    PRIMARY KEY (agent_binding_id, toolkit_id),
    CONSTRAINT agent_toolkit_bindings_binding_same_tenant
        FOREIGN KEY (agent_binding_id, tenant_project_id)
        REFERENCES elitea_identity.agent_identity_bindings (id, tenant_project_id)
);

CREATE TABLE IF NOT EXISTS elitea_identity.credential_use_grants (
    id                       uuid PRIMARY KEY,
    tenant_project_id        bigint NOT NULL CHECK (tenant_project_id > 0),
    agent_binding_id         uuid NOT NULL,
    toolkit_id               bigint NOT NULL CHECK (toolkit_id > 0),
    credential_reference_id  uuid NOT NULL,
    target_binding_id        uuid NOT NULL,
    capabilities             text[] NOT NULL CHECK (
                                 cardinality(capabilities) BETWEEN 1 AND 5
                                 AND capabilities <@ ARRAY['use', 'reveal', 'manage', 'probe', 'approve']::text[]),
    operation_scopes         text[] NOT NULL CHECK (cardinality(operation_scopes) BETWEEN 1 AND 64),
    resource_scopes          text[] NOT NULL DEFAULT '{}' CHECK (cardinality(resource_scopes) <= 64),
    obligations              text[] NOT NULL DEFAULT '{}' CHECK (cardinality(obligations) <= 16),
    state                    text NOT NULL DEFAULT 'proposed'
                             CHECK (state IN ('proposed', 'active', 'suspended', 'revoked')),
    epoch                    bigint NOT NULL DEFAULT 0 CHECK (epoch >= 0),
    expires_at               timestamptz,
    proposed_by              text NOT NULL CHECK (length(proposed_by) BETWEEN 1 AND 64),
    approved_by              text CHECK (approved_by IS NULL OR length(approved_by) BETWEEN 1 AND 64),
    last_transition_by       text NOT NULL CHECK (length(last_transition_by) BETWEEN 1 AND 64),
    created_at               timestamptz NOT NULL DEFAULT clock_timestamp(),
    updated_at               timestamptz NOT NULL DEFAULT clock_timestamp(),
    CONSTRAINT credential_use_grants_binding_same_tenant
        FOREIGN KEY (agent_binding_id, tenant_project_id)
        REFERENCES elitea_identity.agent_identity_bindings (id, tenant_project_id),
    -- The grant's target is its credential reference's target.
    CONSTRAINT credential_use_grants_reference_same_scope
        FOREIGN KEY (credential_reference_id, tenant_project_id, target_binding_id)
        REFERENCES elitea_identity.credential_references (id, tenant_project_id, target_binding_id),
    CONSTRAINT credential_use_grants_approver_not_proposer
        CHECK (approved_by IS NULL OR approved_by <> proposed_by),
    CONSTRAINT credential_use_grants_active_is_approved
        CHECK (state <> 'active' OR approved_by IS NOT NULL)
);

-- One non-revoked grant per agent binding, toolkit, credential and target.
CREATE UNIQUE INDEX IF NOT EXISTS credential_use_grants_one_live_per_key
    ON elitea_identity.credential_use_grants
       (agent_binding_id, toolkit_id, credential_reference_id, target_binding_id)
    WHERE state IN ('proposed', 'active', 'suspended');

CREATE INDEX IF NOT EXISTS credential_use_grants_tenant_created
    ON elitea_identity.credential_use_grants (tenant_project_id, created_at DESC);
