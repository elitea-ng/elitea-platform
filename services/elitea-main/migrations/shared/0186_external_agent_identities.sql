-- 0186_external_agent_identities.sql — external agent identities and
-- run-scoped agent sessions (ADR-0039 decisions 1 and 11, issue #1151).
--
--   external_agent_identities  one per logical agent, application and
--                              provider tenant. Every version of the agent
--                              binds to it; a fork (another application)
--                              gets a new one.
--   agent_sessions             one per Broker.OpenSession: the references
--                              and epochs a later Credential call re-checks.
--                              Keyed on sha256 of the opaque session ref;
--                              the ref itself is never stored.
--
-- agent_identity_bindings.external_agent_identity_id (0181) gets its
-- foreign key here, together with tenant project and application, so a
-- binding can only reference an external identity of its own application.
--
-- No column holds a token, an assertion, a secret or a key.
-- provider_config_key names operator configuration (for example an Entra
-- blueprint), never a credential. Every state change bumps
-- revocation_epoch by one and appends a security audit event (0183 outbox)
-- in the same transaction.
--
-- With ELITEA_ENTERPRISE_CREDENTIAL_GOVERNANCE_ENABLED off nothing writes or
-- reads these tables. Additive only. Idempotent throughout. No BEGIN/COMMIT:
-- the ledgered runner wraps the file.

CREATE SCHEMA IF NOT EXISTS elitea_identity;

CREATE TABLE IF NOT EXISTS elitea_identity.external_agent_identities (
    id                   uuid PRIMARY KEY,
    tenant_project_id    bigint NOT NULL CHECK (tenant_project_id > 0),
    application_id       bigint NOT NULL CHECK (application_id > 0),
    mode                 text NOT NULL
                         CHECK (mode IN ('dedicated_agent', 'runtime_domain_binding', 'elitea_native')),
    provider             text NOT NULL CHECK (provider ~ '^[a-z][a-z0-9_-]{0,31}$'),
    provider_tenant      text NOT NULL CHECK (provider_tenant ~ '^[A-Za-z0-9._:/-]{1,128}$'),
    -- Empty until the provider object exists.
    kind                 text NOT NULL DEFAULT '' CHECK (kind = '' OR kind ~ '^[a-z][a-z0-9._]{0,63}$'),
    external_id          text NOT NULL DEFAULT '' CHECK (length(external_id) <= 256),
    generation           bigint NOT NULL DEFAULT 0 CHECK (generation >= 0),
    provider_config_key  text NOT NULL DEFAULT '' CHECK (provider_config_key ~ '^[A-Za-z0-9._:/-]{0,128}$'),
    agent_user_ref       text NOT NULL DEFAULT '' CHECK (length(agent_user_ref) <= 256),
    owner_ref            text NOT NULL DEFAULT '' CHECK (length(owner_ref) <= 128),
    state                text NOT NULL DEFAULT 'provisioning'
                         CHECK (state IN ('provisioning', 'pending_activation', 'active', 'suspended',
                                          'retiring', 'retired', 'replaced')),
    revocation_epoch     bigint NOT NULL DEFAULT 0 CHECK (revocation_epoch >= 0),
    created_by           text NOT NULL CHECK (length(created_by) BETWEEN 1 AND 64),
    last_transition_by   text NOT NULL CHECK (length(last_transition_by) BETWEEN 1 AND 64),
    created_at           timestamptz NOT NULL DEFAULT clock_timestamp(),
    updated_at           timestamptz NOT NULL DEFAULT clock_timestamp(),
    -- The target of the bindings' composite foreign key.
    CONSTRAINT external_agent_identities_scope_key UNIQUE (id, tenant_project_id, application_id),
    CONSTRAINT external_agent_identities_tenant_key UNIQUE (id, tenant_project_id),
    -- Past provisioning a record names its provider object.
    CONSTRAINT external_agent_identities_provisioned_has_ref
        CHECK (state IN ('provisioning', 'retiring', 'retired', 'replaced') OR (kind <> '' AND external_id <> ''))
);

-- One live external identity per tenant project, application, provider and
-- provider tenant for the per-agent modes. Retired and replaced records do
-- not count, so a replacement can be created.
CREATE UNIQUE INDEX IF NOT EXISTS external_agent_identities_one_live_per_agent
    ON elitea_identity.external_agent_identities (tenant_project_id, application_id, provider, provider_tenant)
    WHERE state NOT IN ('retired', 'replaced') AND mode IN ('dedicated_agent', 'elitea_native');

-- One live record per provider object.
CREATE UNIQUE INDEX IF NOT EXISTS external_agent_identities_one_live_per_object
    ON elitea_identity.external_agent_identities (provider, provider_tenant, external_id)
    WHERE external_id <> '' AND state NOT IN ('retired', 'replaced');

CREATE INDEX IF NOT EXISTS external_agent_identities_tenant_created
    ON elitea_identity.external_agent_identities (tenant_project_id, created_at DESC);

-- The binding's reference, with its tenant project and application. MATCH
-- SIMPLE: a NULL external_agent_identity_id skips the check. NOT VALID: a
-- governed deployment may hold 0181 bindings whose bare uuid names no row
-- (none can be active: FailClosedExternalIdentities refused every
-- approval). Every new or re-pointed reference is checked.
DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint
        WHERE conname = 'agent_identity_bindings_external_same_application'
          AND conrelid = 'elitea_identity.agent_identity_bindings'::regclass
    ) THEN
        ALTER TABLE elitea_identity.agent_identity_bindings
            ADD CONSTRAINT agent_identity_bindings_external_same_application
            FOREIGN KEY (external_agent_identity_id, tenant_project_id, application_id)
            REFERENCES elitea_identity.external_agent_identities (id, tenant_project_id, application_id)
            NOT VALID;
    END IF;
END
$$;

CREATE INDEX IF NOT EXISTS agent_identity_bindings_external
    ON elitea_identity.agent_identity_bindings (external_agent_identity_id)
    WHERE external_agent_identity_id IS NOT NULL;

CREATE TABLE IF NOT EXISTS elitea_identity.agent_sessions (
    id                          uuid PRIMARY KEY,
    -- sha256 of the opaque session ref.
    ref_digest                  bytea NOT NULL UNIQUE CHECK (octet_length(ref_digest) = 32),
    tenant_project_id           bigint NOT NULL CHECK (tenant_project_id > 0),
    execution_id                text NOT NULL CHECK (length(execution_id) BETWEEN 1 AND 128),
    generation                  bigint NOT NULL CHECK (generation > 0),
    agent_binding_id            uuid NOT NULL,
    agent_binding_epoch         bigint NOT NULL CHECK (agent_binding_epoch >= 0),
    external_agent_identity_id  uuid NOT NULL,
    external_generation         bigint NOT NULL CHECK (external_generation >= 0),
    external_epoch              bigint NOT NULL CHECK (external_epoch >= 0),
    security_profile            text NOT NULL
                                CHECK (security_profile IN ('autonomous_attributed', 'delegated_verified')),
    user_evidence_kind          text NOT NULL CHECK (user_evidence_kind IN ('none', 'verified', 'asserted')),
    delegation_id               text NOT NULL DEFAULT '' CHECK (length(delegation_id) <= 256),
    mandate_id                  text NOT NULL DEFAULT '' CHECK (length(mandate_id) <= 256),
    -- References of the user evidence. Never an assertion.
    evidence_issuer             text NOT NULL DEFAULT '' CHECK (length(evidence_issuer) <= 256),
    evidence_tenant_id          text NOT NULL DEFAULT '' CHECK (length(evidence_tenant_id) <= 256),
    evidence_subject            text NOT NULL DEFAULT '' CHECK (length(evidence_subject) <= 256),
    evidence_audience           text NOT NULL DEFAULT '' CHECK (length(evidence_audience) <= 256),
    ceiling_audiences           text[] NOT NULL CHECK (cardinality(ceiling_audiences) BETWEEN 1 AND 64),
    ceiling_scopes              text[] NOT NULL DEFAULT '{}' CHECK (cardinality(ceiling_scopes) <= 64),
    -- Bound by the first Credential call.
    worker_claim_ref            text NOT NULL DEFAULT '' CHECK (length(worker_claim_ref) <= 256),
    policy_revision_id          text NOT NULL DEFAULT '' CHECK (length(policy_revision_id) <= 256),
    state                       text NOT NULL DEFAULT 'open' CHECK (state IN ('open', 'closed')),
    expires_at                  timestamptz NOT NULL,
    created_at                  timestamptz NOT NULL DEFAULT clock_timestamp(),
    updated_at                  timestamptz NOT NULL DEFAULT clock_timestamp(),
    CONSTRAINT agent_sessions_binding_same_tenant
        FOREIGN KEY (agent_binding_id, tenant_project_id)
        REFERENCES elitea_identity.agent_identity_bindings (id, tenant_project_id),
    CONSTRAINT agent_sessions_external_same_tenant
        FOREIGN KEY (external_agent_identity_id, tenant_project_id)
        REFERENCES elitea_identity.external_agent_identities (id, tenant_project_id),
    -- The profile fixes the evidence kind (ADR-0039 decision 3).
    CONSTRAINT agent_sessions_profile_matches_evidence CHECK (
        (security_profile = 'delegated_verified' AND user_evidence_kind = 'verified')
        OR (security_profile = 'autonomous_attributed' AND user_evidence_kind IN ('none', 'asserted'))
    ),
    CONSTRAINT agent_sessions_expiry_after_creation CHECK (expires_at > created_at)
);

CREATE INDEX IF NOT EXISTS agent_sessions_execution
    ON elitea_identity.agent_sessions (execution_id, generation);
CREATE INDEX IF NOT EXISTS agent_sessions_external_identity
    ON elitea_identity.agent_sessions (external_agent_identity_id);

-- The ADMINISTRATION-mode grant behind the external identity admin API:
--
--   models.agent_identity.external.manage  list, create, provision, activate,
--                                          suspend, resume, retire
--
-- Activation also needs models.agent_identity.bindings.approve (0181).
-- Held by `super_admin` and `admin` only, like 0181. The to_regclass guard
-- and ON CONFLICT DO NOTHING make the block inert on a database without
-- auth_core tables and on a repeat run.
DO $$
BEGIN
    IF to_regclass('public.auth_core__role') IS NULL
       OR to_regclass('public.auth_core__role_permission') IS NULL THEN
        RAISE NOTICE '0186: auth_core tables absent, nothing to grant';
        RETURN;
    END IF;

    INSERT INTO public.auth_core__role_permission (role_id, permission)
    SELECT role.id, 'models.agent_identity.external.manage'
    FROM public.auth_core__role AS role
    WHERE role.mode = 'administration' AND role.name IN ('super_admin', 'admin')
    ON CONFLICT (role_id, permission) DO NOTHING;
END
$$;
