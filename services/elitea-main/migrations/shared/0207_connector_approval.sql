-- 0207_connector_approval.sql — default-deny catalogue mode and connector
-- approval (ADR-0044 decision 1, issue #1501).
--
--   centry.connector_requests
--       A request to make one connector usable: a toolkit type, a pre-built
--       MCP catalogue entry or a custom MCP server URL (target_kind,
--       target), for a list of projects or platform-wide, with the tools it
--       declares (name, ADR-0041 category and, for a custom MCP server, the
--       digest of each tool's description and input schema), the egress
--       hosts it needs, the data classes it touches and a justification.
--       States: draft, submitted, approved, rejected, withdrawn. While the
--       platform's catalogue mode is approved_only (the `catalogue`
--       Configuration section; forced by the governed credential profile),
--       a connector is usable only by the projects an approved request
--       names. The approver differs from the requester, in code and in the
--       CHECK below. The approval also writes the toolkit-type grants
--       (centry.toolkit_type_policy / toolkit_type_project_grant, 0114) and
--       the worker egress allowlist entries (the platform section or each
--       project's worker_egress setting, 0197's ADR) in the same
--       transaction. A custom MCP server's approved tools are pinned: a tool
--       whose name or digest no approved request names stays hidden.
--
--   elitea_runtime.command_outbox.refusal_code
--       Admits CONNECTOR_NOT_APPROVED beside EGRESS_POLICY_INVALID: a worker
--       command naming a connector the run's project may not use (configured
--       before the mode switched) fails at once instead of running it.
--
--   elitea_lifecycle.attestations.subject_type
--       Admits `connector`: the optional mandatory architecture attestation
--       of a connector request (subject_id is the request id) is recorded
--       in the requesting project, as every other attestation is.
--
-- Permission: connector.approve goes to the platform administrators
-- (administration mode: super_admin, admin). 0060 returns early on an
-- existing database, so the grant is here and in 001_initial.sql.
--
-- Every statement is guarded: the file tolerates a database without the
-- runtime outbox, the lifecycle schema or the auth_core tables, and one
-- that already holds its objects. No BEGIN/COMMIT: the ledgered runner
-- wraps the file.

CREATE SCHEMA IF NOT EXISTS centry;

CREATE TABLE IF NOT EXISTS centry.connector_requests (
    id              bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    project_id      bigint NOT NULL CHECK (project_id > 0),
    target_kind     text NOT NULL
                    CHECK (target_kind IN ('toolkit_type', 'prebuilt_mcp', 'custom_mcp')),
    target          text NOT NULL CHECK (length(target) BETWEEN 1 AND 2048),
    toolkit_type    text NOT NULL CHECK (length(toolkit_type) BETWEEN 1 AND 256),
    platform_wide   boolean NOT NULL DEFAULT false,
    project_ids     bigint[] NOT NULL DEFAULT '{}' CHECK (cardinality(project_ids) <= 64),
    declared_tools  jsonb NOT NULL DEFAULT '[]'::jsonb
                    CHECK (jsonb_typeof(declared_tools) = 'array' AND jsonb_array_length(declared_tools) <= 256),
    egress_hosts    text[] NOT NULL DEFAULT '{}' CHECK (cardinality(egress_hosts) <= 64),
    data_classes    text[] NOT NULL DEFAULT '{}' CHECK (cardinality(data_classes) <= 32),
    justification   text NOT NULL DEFAULT '' CHECK (length(justification) <= 4000),
    state           text NOT NULL DEFAULT 'draft'
                    CHECK (state IN ('draft', 'submitted', 'approved', 'rejected', 'withdrawn')),
    requested_by    bigint NOT NULL CHECK (requested_by > 0),
    decided_by      bigint CHECK (decided_by IS NULL OR decided_by > 0),
    decision_reason text NOT NULL DEFAULT '' CHECK (length(decision_reason) <= 1000),
    attestation_id  bigint CHECK (attestation_id IS NULL OR attestation_id > 0),
    created_at      timestamptz NOT NULL DEFAULT clock_timestamp(),
    updated_at      timestamptz NOT NULL DEFAULT clock_timestamp(),
    submitted_at    timestamptz,
    decided_at      timestamptz,
    CONSTRAINT connector_requests_scope
        CHECK (platform_wide = (cardinality(project_ids) = 0)),
    CONSTRAINT connector_requests_decider_not_requester
        CHECK (decided_by IS NULL OR decided_by <> requested_by),
    CONSTRAINT connector_requests_decision_complete
        CHECK ((state IN ('approved', 'rejected')) = (decided_by IS NOT NULL AND decided_at IS NOT NULL)),
    CONSTRAINT connector_requests_submitted_stamped
        CHECK (state = 'draft' OR state = 'withdrawn' OR submitted_at IS NOT NULL)
);

CREATE INDEX IF NOT EXISTS connector_requests_project
    ON centry.connector_requests (project_id, id DESC);

CREATE INDEX IF NOT EXISTS connector_requests_state
    ON centry.connector_requests (state, id DESC);

CREATE INDEX IF NOT EXISTS connector_requests_approved_target
    ON centry.connector_requests (target_kind, target)
    WHERE state = 'approved';

DO $$
BEGIN
    IF to_regclass('elitea_runtime.command_outbox') IS NULL THEN
        RAISE NOTICE '0207: elitea_runtime.command_outbox absent, refusal codes unchanged';
        RETURN;
    END IF;
    -- Re-added NOT VALID, as 0197 added it: no scan under the lock, every
    -- new and updated row checked.
    ALTER TABLE elitea_runtime.command_outbox DROP CONSTRAINT IF EXISTS command_outbox_refusal;
    ALTER TABLE elitea_runtime.command_outbox
        ADD CONSTRAINT command_outbox_refusal CHECK (
            (refusal_code IS NULL AND refusal_message IS NULL)
            OR (
                refusal_code IN ('EGRESS_POLICY_INVALID', 'CONNECTOR_NOT_APPROVED')
                AND octet_length(refusal_message) BETWEEN 1 AND 1024
                AND prepared_at IS NULL
                AND authority_granted_at IS NULL
            )
        ) NOT VALID;
END $$;

DO $$
BEGIN
    IF to_regclass('elitea_lifecycle.attestations') IS NULL THEN
        RAISE NOTICE '0207: elitea_lifecycle.attestations absent, subject types unchanged';
        RETURN;
    END IF;
    ALTER TABLE elitea_lifecycle.attestations DROP CONSTRAINT IF EXISTS attestations_subject_type_check;
    ALTER TABLE elitea_lifecycle.attestations
        ADD CONSTRAINT attestations_subject_type_check
        CHECK (subject_type IN ('agent', 'release', 'toolkit', 'index', 'connector'));
END $$;

DO $$
BEGIN
    IF to_regclass('public.auth_core__role') IS NULL
       OR to_regclass('public.auth_core__role_permission') IS NULL THEN
        RAISE NOTICE '0207: auth_core tables absent, nothing to grant';
        RETURN;
    END IF;

    -- The mode spelled as a literal: the router's grant gate
    -- (router_permission_grant_gate_test.go) reads the grants of this
    -- directory per `mode = '...'` block.
    INSERT INTO public.auth_core__role_permission (role_id, permission)
    SELECT role.id, 'connector.approve'
    FROM public.auth_core__role AS role
    WHERE role.mode = 'administration' AND role.name IN ('super_admin', 'admin')
    ON CONFLICT (role_id, permission) DO NOTHING;
END
$$;
