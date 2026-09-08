-- 0112_governance_config_egress_allowlist.sql (gap G6)
--
-- NUMBERED 0112, NOT 0111. 0111 belongs to
-- shared/0111_mcp_prebuilt_parameter_schema.sql, which is in review on its own
-- branch. Both files correctly claimed a free number against a main whose head
-- was 0110; the collision is only real at merge, and this one yields.
-- scripts/database/check-migration-version.sh requires a version ABOVE the base
-- head, not the next one, and Head() reads the last entry rather than counting,
-- so a gap costs nothing if the other file lands first.
--
-- ---------------------------------------------------------------------------
-- WHY
-- ---------------------------------------------------------------------------
--
-- The LLM gateway's egress allowlist had ONE authoring surface: the environment
-- variable GATEWAY_EGRESS_ALLOWLIST, set in the Helm chart. An empty value
-- meant two different things at the same time:
--
--   * every public host is an acceptable credential `api_base`, and
--   * no private host is reachable, because bifrost's SSRF-safe dialer stays
--     armed for every provider.
--
-- So an on-premise model endpoint — a vLLM at http://192.168.29.60:8000/v1 —
-- could not be reached without a chart edit and a pod restart. There was no
-- admin route for it at all, while every other gateway control (budgets, rate
-- limits, model allowlists, MCP allowlists, routing rules) is authored in
-- gateway.governance_config and polled by the gateway.
--
-- This file adds `egress_allowlist` to that table's type set. The gateway
-- UNIONS the authored rows with the environment variable; the environment
-- variable stays a FLOOR that no authored row can withdraw, because the chart
-- and the admin console are different authorities.
--
-- ---------------------------------------------------------------------------
-- WHAT THE ROW CARRIES
-- ---------------------------------------------------------------------------
--
--   type    = 'egress_allowlist'
--   data    = {"egress": {"allowlist": ["host", "host:port", "*.domain",
--                                        "192.168.29.0/24"]}}
--
-- The grammar is the one GATEWAY_EGRESS_ALLOWLIST already parses, extended with
-- CIDR blocks. ONE parser serves all three readers — the environment variable,
-- this table, and elitea-main's write validation — and it lives in
-- libs/go/egresslib. Three copies of one grammar drift, and a host the admin
-- form accepts while the gateway ignores it is the exact failure this row type
-- exists to end.
--
-- A CIDR entry matches an api_base whose host is an IP LITERAL only. A hostname
-- that resolves inside the block is not matched: resolving a name here and
-- dialing it later is the DNS-rebinding race that a name allowlist avoids.
--
-- An `egress_allowlist` row is GLOBAL and carries no scope. Half of what it
-- governs — whether the SSRF-safe dialer is relaxed for the self-hosted
-- provider classes — is decided by bifrost with no project in hand, so a scoped
-- row could only be half-honoured. elitea-main refuses a scoped row on write and
-- the gateway's compiler rejects one that reaches it anyway.
--
-- ---------------------------------------------------------------------------
-- THE CONSTRAINT
-- ---------------------------------------------------------------------------
--
-- 0093 added governance_config_type_known, a NOT VALID CHECK over the seven
-- types that existed then. 0093 is checksum-immutable
-- (internal/infra/db/migrate/manifest.go), so the value set is widened by
-- REPLACING the constraint here rather than by editing that file.
--
-- The replacement keeps 0093's two properties and its reasons:
--
--   * SAME NAME. An operator who reads 0093's header and runs
--     `ALTER TABLE gateway.governance_config VALIDATE CONSTRAINT
--     governance_config_type_known` still finds the constraint it names.
--   * STILL NOT VALID. An existing deployment may already hold a row with some
--     other type — the table has been writable through a generic CRUD since
--     0067 — and a validating constraint would fail the migration and block the
--     whole release. NOT VALID checks every future INSERT and UPDATE and leaves
--     existing rows alone. The gateway names any such row in its logs and on
--     GET /governance/status.
--
-- Idempotent, like every file in this corpus. The DO block replaces the
-- constraint only when the one in place does not already admit the new value,
-- so re-applying over an up-to-date database is inert.

DO $$
DECLARE
    current_definition text;
BEGIN
    IF to_regclass('gateway.governance_config') IS NULL THEN
        -- 0067 has not run on this database. Nothing to constrain; the CREATE
        -- TABLE there does not carry this constraint, and a later run of 0093
        -- and this file over the created table adds it.
        RETURN;
    END IF;

    SELECT pg_get_constraintdef(oid)
      INTO current_definition
      FROM pg_constraint
     WHERE conrelid = 'gateway.governance_config'::regclass
       AND conname  = 'governance_config_type_known';

    IF current_definition IS NOT NULL AND current_definition LIKE '%egress_allowlist%' THEN
        -- Already widened. Re-applying must not churn the constraint.
        RETURN;
    END IF;

    IF current_definition IS NOT NULL THEN
        ALTER TABLE gateway.governance_config
            DROP CONSTRAINT governance_config_type_known;
    END IF;

    ALTER TABLE gateway.governance_config
        ADD CONSTRAINT governance_config_type_known
        CHECK (type IN (
            'budget',
            'rate_limit',
            'model_config',
            'routing_rule',
            'mcp_allowlist',
            'credential_policy',
            'budget_alert',
            'egress_allowlist'
        ))
        NOT VALID;
END
$$;
