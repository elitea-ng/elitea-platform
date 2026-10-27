-- 0188_credential_redemptions.sql — governed credential redemption (ADR-0039
-- decision 10, issue #1118).
--
--   bound_tool_invocations    one row per tool invocation a governed worker
--                             redeems a credential for, unique per (execution,
--                             generation, invocation id). A replay of the same
--                             id with any other field is refused.
--   credential_redemptions    one row per released credential: the decision
--                             that allowed it, its kind and its expiry. The
--                             material is never stored.
--   execution_agent_sessions  the run-scoped agent session the broker opened
--                             at the run's first redemption. The execution
--                             security context is immutable and digested, so
--                             the session is linked here, not in the context.
--                             The reference is not a credential on its own:
--                             the agent identity broker also checks the
--                             execution, generation and worker claim.
--
-- Written by the credential broker only, which exists only with
-- ELITEA_ENTERPRISE_CREDENTIAL_GOVERNANCE_ENABLED on, so a flag-off
-- deployment writes and reads nothing here. No row is ever updated. Additive
-- only; idempotent; no BEGIN/COMMIT: the ledgered runner wraps the file.

CREATE TABLE IF NOT EXISTS elitea_runtime.bound_tool_invocations (
    execution_id             text NOT NULL CHECK (length(execution_id) BETWEEN 1 AND 128),
    generation               bigint NOT NULL CHECK (generation > 0),
    invocation_id            uuid NOT NULL,
    tool_call_id             text NOT NULL CHECK (length(tool_call_id) BETWEEN 1 AND 256),
    toolkit_id               bigint NOT NULL CHECK (toolkit_id > 0),
    operation_id             text NOT NULL CHECK (length(operation_id) BETWEEN 5 AND 194),
    resource_ref             text NOT NULL DEFAULT '' CHECK (length(resource_ref) <= 512),
    arguments_digest         bytea NOT NULL CHECK (octet_length(arguments_digest) = 32),
    credential_reference_id  uuid NOT NULL,
    tenant_project_id        bigint NOT NULL CHECK (tenant_project_id > 0),
    created_at               timestamptz NOT NULL DEFAULT clock_timestamp(),
    PRIMARY KEY (execution_id, generation, invocation_id)
);

CREATE TABLE IF NOT EXISTS elitea_runtime.credential_redemptions (
    redemption_id     uuid PRIMARY KEY,
    execution_id      text NOT NULL,
    generation        bigint NOT NULL,
    invocation_id     uuid NOT NULL,
    decision_id       uuid NOT NULL,
    kind              text NOT NULL CHECK (kind IN ('static', 'oauth_access', 'dynamic_lease')),
    not_after         timestamptz NOT NULL,
    created_at        timestamptz NOT NULL DEFAULT clock_timestamp(),
    CONSTRAINT credential_redemptions_invocation
        FOREIGN KEY (execution_id, generation, invocation_id)
        REFERENCES elitea_runtime.bound_tool_invocations (execution_id, generation, invocation_id)
        ON DELETE CASCADE
);

CREATE INDEX IF NOT EXISTS credential_redemptions_by_invocation
    ON elitea_runtime.credential_redemptions (execution_id, generation, invocation_id);

CREATE TABLE IF NOT EXISTS elitea_runtime.execution_agent_sessions (
    execution_id       text NOT NULL CHECK (length(execution_id) BETWEEN 1 AND 128),
    generation         bigint NOT NULL CHECK (generation > 0),
    agent_session_ref  text NOT NULL CHECK (length(agent_session_ref) BETWEEN 1 AND 128),
    created_at         timestamptz NOT NULL DEFAULT clock_timestamp(),
    PRIMARY KEY (execution_id, generation)
);

CREATE OR REPLACE FUNCTION elitea_runtime.credential_redemption_rows_immutable()
RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'credential redemption rows are immutable';
END
$$;

DROP TRIGGER IF EXISTS bound_tool_invocations_immutable
    ON elitea_runtime.bound_tool_invocations;
CREATE TRIGGER bound_tool_invocations_immutable
    BEFORE UPDATE ON elitea_runtime.bound_tool_invocations
    FOR EACH ROW EXECUTE FUNCTION elitea_runtime.credential_redemption_rows_immutable();

DROP TRIGGER IF EXISTS credential_redemptions_immutable
    ON elitea_runtime.credential_redemptions;
CREATE TRIGGER credential_redemptions_immutable
    BEFORE UPDATE ON elitea_runtime.credential_redemptions
    FOR EACH ROW EXECUTE FUNCTION elitea_runtime.credential_redemption_rows_immutable();

DROP TRIGGER IF EXISTS execution_agent_sessions_immutable
    ON elitea_runtime.execution_agent_sessions;
CREATE TRIGGER execution_agent_sessions_immutable
    BEFORE UPDATE ON elitea_runtime.execution_agent_sessions
    FOR EACH ROW EXECUTE FUNCTION elitea_runtime.credential_redemption_rows_immutable();
