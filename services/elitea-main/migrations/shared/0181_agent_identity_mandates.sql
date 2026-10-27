-- 0181_agent_identity_mandates.sql — agent identity bindings, mandates and
-- delegation metadata (ADR-0039 decisions 1-3, issue #1112).
--
--   agent_identity_bindings  one per agent version: application version +
--                            frozen definition digest + a reference to the
--                            external agent identity (#1151 adds that table
--                            and the foreign key; the column is a bare uuid
--                            until then).
--   delegations              interactive-delegation METADATA only. Token
--                            custody (ciphertext columns) arrives in #1117.
--   mandates                 stored, revocable authority for unattended runs.
--
-- No column holds a token, an assertion, a secret or a key. Every state
-- change bumps revocation_epoch by one (the repositories do it under
-- SELECT ... FOR UPDATE), records its actor in last_transition_by and
-- appends a security audit event (0183 outbox) in the same transaction. The
-- approver differs from the proposer/issuer, in code and in the CHECKs below.
--
-- A delegation and a mandate stay inside their binding's tenant project, and
-- a mandate's delegation belongs to the same binding: composite foreign keys
-- enforce both, behind the same checks in code.
--
-- With ELITEA_ENTERPRISE_CREDENTIAL_GOVERNANCE_ENABLED off nothing writes or
-- reads these tables. Additive only; no change to existing tables.
-- Idempotent throughout. No BEGIN/COMMIT: the ledgered runner wraps the file.

CREATE SCHEMA IF NOT EXISTS elitea_identity;

CREATE TABLE IF NOT EXISTS elitea_identity.agent_identity_bindings (
    id                          uuid PRIMARY KEY,
    tenant_project_id           bigint NOT NULL CHECK (tenant_project_id > 0),
    application_id              bigint NOT NULL CHECK (application_id > 0),
    application_version_id      bigint NOT NULL CHECK (application_version_id > 0),
    definition_digest           bytea NOT NULL CHECK (octet_length(definition_digest) = 32),
    mode                        text NOT NULL
                                CHECK (mode IN ('dedicated_agent', 'runtime_domain_binding', 'elitea_native')),
    -- References elitea_identity.external_agent_identities (#1151 adds the
    -- table and the constraint). Many bindings may share one.
    external_agent_identity_id  uuid,
    workload_identity           text NOT NULL DEFAULT ''
                                CHECK (length(workload_identity) <= 512),
    state                       text NOT NULL DEFAULT 'proposed'
                                CHECK (state IN ('proposed', 'active', 'suspended', 'revoked')),
    revocation_epoch            bigint NOT NULL DEFAULT 0 CHECK (revocation_epoch >= 0),
    policy_revision_id          text NOT NULL
                                REFERENCES elitea_identity.credential_policy_revisions (id),
    proposed_by                 text NOT NULL CHECK (length(proposed_by) BETWEEN 1 AND 64),
    approved_by                 text CHECK (approved_by IS NULL OR length(approved_by) BETWEEN 1 AND 64),
    -- Who made the latest change: the proposer at creation, then the actor of
    -- every approve, suspend and revoke.
    last_transition_by          text NOT NULL CHECK (length(last_transition_by) BETWEEN 1 AND 64),
    created_at                  timestamptz NOT NULL DEFAULT clock_timestamp(),
    updated_at                  timestamptz NOT NULL DEFAULT clock_timestamp(),
    CONSTRAINT agent_identity_bindings_tenant_key UNIQUE (id, tenant_project_id),
    CONSTRAINT agent_identity_bindings_approver_not_proposer
        CHECK (approved_by IS NULL OR approved_by <> proposed_by),
    CONSTRAINT agent_identity_bindings_active_is_approved
        CHECK (state <> 'active' OR approved_by IS NOT NULL),
    CONSTRAINT agent_identity_bindings_workload_for_runtime_domain
        CHECK ((mode = 'runtime_domain_binding') = (workload_identity <> ''))
);

-- One non-revoked binding per tenant project, version and digest.
CREATE UNIQUE INDEX IF NOT EXISTS agent_identity_bindings_one_live_per_definition
    ON elitea_identity.agent_identity_bindings (tenant_project_id, application_version_id, definition_digest)
    WHERE state IN ('proposed', 'active', 'suspended');

CREATE INDEX IF NOT EXISTS agent_identity_bindings_tenant_created
    ON elitea_identity.agent_identity_bindings (tenant_project_id, created_at DESC);

CREATE TABLE IF NOT EXISTS elitea_identity.delegations (
    id                   uuid PRIMARY KEY,
    tenant_project_id    bigint NOT NULL CHECK (tenant_project_id > 0),
    issuer               text NOT NULL CHECK (length(issuer) BETWEEN 1 AND 512),
    tenant_id            text NOT NULL CHECK (length(tenant_id) BETWEEN 1 AND 128),
    subject_object_id    text NOT NULL CHECK (length(subject_object_id) BETWEEN 1 AND 128),
    agent_binding_id     uuid NOT NULL,
    audiences            text[] NOT NULL DEFAULT '{}' CHECK (cardinality(audiences) <= 64),
    scopes               text[] NOT NULL DEFAULT '{}' CHECK (cardinality(scopes) <= 64),
    consent_recorded_at  timestamptz NOT NULL,
    expires_at           timestamptz NOT NULL,
    state                text NOT NULL DEFAULT 'active'
                         CHECK (state IN ('active', 'suspended', 'revoked', 'expired')),
    revocation_epoch     bigint NOT NULL DEFAULT 0 CHECK (revocation_epoch >= 0),
    created_at           timestamptz NOT NULL DEFAULT clock_timestamp(),
    updated_at           timestamptz NOT NULL DEFAULT clock_timestamp(),
    CONSTRAINT delegations_scope_key UNIQUE (id, tenant_project_id, agent_binding_id),
    CONSTRAINT delegations_binding_same_tenant
        FOREIGN KEY (agent_binding_id, tenant_project_id)
        REFERENCES elitea_identity.agent_identity_bindings (id, tenant_project_id)
);

CREATE TABLE IF NOT EXISTS elitea_identity.mandates (
    id                  uuid PRIMARY KEY,
    tenant_project_id   bigint NOT NULL CHECK (tenant_project_id > 0),
    kind                text NOT NULL CHECK (kind IN ('autonomous', 'delegated_background')),
    agent_binding_id    uuid NOT NULL,
    subject_user_id     bigint CHECK (subject_user_id IS NULL OR subject_user_id > 0),
    delegation_id       uuid,
    operation_scopes    text[] NOT NULL CHECK (cardinality(operation_scopes) BETWEEN 1 AND 64),
    resource_scopes     text[] NOT NULL DEFAULT '{}' CHECK (cardinality(resource_scopes) <= 64),
    not_before          timestamptz NOT NULL,
    expires_at          timestamptz NOT NULL,
    state               text NOT NULL DEFAULT 'proposed'
                        CHECK (state IN ('proposed', 'active', 'suspended', 'revoked', 'expired')),
    revocation_epoch    bigint NOT NULL DEFAULT 0 CHECK (revocation_epoch >= 0),
    policy_revision_id  text NOT NULL
                        REFERENCES elitea_identity.credential_policy_revisions (id),
    issued_by           text NOT NULL CHECK (length(issued_by) BETWEEN 1 AND 64),
    approved_by         text CHECK (approved_by IS NULL OR length(approved_by) BETWEEN 1 AND 64),
    -- Who made the latest change, including a cascade from the binding.
    last_transition_by  text NOT NULL CHECK (length(last_transition_by) BETWEEN 1 AND 64),
    created_at          timestamptz NOT NULL DEFAULT clock_timestamp(),
    updated_at          timestamptz NOT NULL DEFAULT clock_timestamp(),
    CONSTRAINT mandates_binding_same_tenant
        FOREIGN KEY (agent_binding_id, tenant_project_id)
        REFERENCES elitea_identity.agent_identity_bindings (id, tenant_project_id),
    -- MATCH SIMPLE: a NULL delegation_id skips the check.
    CONSTRAINT mandates_delegation_same_scope
        FOREIGN KEY (delegation_id, tenant_project_id, agent_binding_id)
        REFERENCES elitea_identity.delegations (id, tenant_project_id, agent_binding_id),
    CONSTRAINT mandates_expiry_after_start CHECK (expires_at > not_before),
    CONSTRAINT mandates_approver_not_issuer
        CHECK (approved_by IS NULL OR approved_by <> issued_by),
    CONSTRAINT mandates_active_is_approved
        CHECK (state <> 'active' OR approved_by IS NOT NULL),
    CONSTRAINT mandates_subject_matches_kind CHECK (
        (kind = 'autonomous' AND subject_user_id IS NULL AND delegation_id IS NULL)
        OR (kind = 'delegated_background' AND subject_user_id IS NOT NULL)
    )
);

CREATE INDEX IF NOT EXISTS mandates_binding
    ON elitea_identity.mandates (agent_binding_id);
CREATE INDEX IF NOT EXISTS mandates_tenant_created
    ON elitea_identity.mandates (tenant_project_id, created_at DESC);

-- The ADMINISTRATION-mode grants behind the admin API:
--
--   models.agent_identity.bindings.manage   list, propose, suspend, revoke
--   models.agent_identity.bindings.approve  list, approve (and reactivate)
--   models.agent_identity.mandates.manage   list, issue, suspend, revoke
--   models.agent_identity.mandates.approve  list, approve (and reactivate)
--
-- Held by `super_admin` and `admin` only, like every central configuration
-- grant. Not in 0060: it returns early on any deployment that already ran it.
-- The to_regclass guard and ON CONFLICT DO NOTHING make the block inert on a
-- database without auth_core tables and on a repeat run.
DO $$
BEGIN
    IF to_regclass('public.auth_core__role') IS NULL
       OR to_regclass('public.auth_core__role_permission') IS NULL THEN
        RAISE NOTICE '0181: auth_core tables absent, nothing to grant';
        RETURN;
    END IF;

    INSERT INTO public.auth_core__role_permission (role_id, permission)
    SELECT role.id, grant_row.permission
    FROM public.auth_core__role AS role
    CROSS JOIN (VALUES
        ('models.agent_identity.bindings.manage'),
        ('models.agent_identity.bindings.approve'),
        ('models.agent_identity.mandates.manage'),
        ('models.agent_identity.mandates.approve')
    ) AS grant_row(permission)
    WHERE role.mode = 'administration' AND role.name IN ('super_admin', 'admin')
    ON CONFLICT (role_id, permission) DO NOTHING;
END
$$;
