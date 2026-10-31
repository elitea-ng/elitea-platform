-- 0214_guardrail_pipeline.sql — the gateway guardrail pipeline (ADR-0043
-- decision 1, issue #1500).
--
--   gateway.governance_config
--       The type `guardrail` joins governance_config_type_known. A guardrail
--       row names a stage (input or output), an ordered list of detectors
--       (regex, secrets, http) with an action each (block, mask, flag), a
--       timeout and a fail mode. elitea-main validates the row on write
--       (internal/api/gateway/governance_guardrail.go); the LLM gateway
--       compiles it on load (internal/guardrail).
--
--   gateway.guardrail_verdicts
--       One row per detector per request: the request's project, user,
--       execution id and route, then the row, the detector, its type, the
--       action, the outcome (pass, flagged, masked, blocked, error_open,
--       error_closed), the fail mode, the labels, the number of findings,
--       the highest score, the latency and the number of runs merged into the
--       row (a streamed answer is checked once per window).
--
--       NO CONTENT, BY CONSTRUCTION, as in 0099. No column can hold the
--       inspected text or the matched text. A label is a short identifier
--       (^[a-z0-9][a-z0-9_.:-]{0,63}$) that the row author or the adapter
--       assigns.
--
--       The gateway writes the rows with its request log batch
--       (internal/requestlog/verdicts.go). For a request made from a runtime
--       execution the rows are step evidence of that run (ADR-0042): the run
--       evidence API reads them by execution_id, which is the run id of
--       elitea_runtime.run_step_heads. The table is the gateway's, not part of
--       the run_steps hash chain: the gateway has no internal channel to
--       elitea-main, and the chain is elitea-main's to write.
--
--       Retention: the gateway prunes the rows with its request log, on the
--       window of the `gateway_request_log` retention class.
--
-- Every statement is guarded or idempotent. No BEGIN/COMMIT: the ledgered
-- runner wraps the file.

DO $$
DECLARE
    current_definition text;
BEGIN
    IF to_regclass('gateway.governance_config') IS NULL THEN
        RETURN;
    END IF;

    SELECT pg_get_constraintdef(oid)
      INTO current_definition
      FROM pg_constraint
     WHERE conrelid = 'gateway.governance_config'::regclass
       AND conname  = 'governance_config_type_known';

    IF current_definition IS NOT NULL AND current_definition LIKE '%''guardrail''%' THEN
        RETURN;
    END IF;

    IF current_definition IS NOT NULL THEN
        ALTER TABLE gateway.governance_config
            DROP CONSTRAINT governance_config_type_known;
    END IF;

    -- Same name and still NOT VALID, for the reasons 0112 gives.
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
            'egress_allowlist',
            'guardrail'
        ))
        NOT VALID;
END
$$;

CREATE SCHEMA IF NOT EXISTS gateway;

CREATE TABLE IF NOT EXISTS gateway.guardrail_verdicts (
    id             BIGSERIAL PRIMARY KEY,
    occurred_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    project_id     INTEGER,
    user_id        INTEGER,
    execution_id   VARCHAR(128),
    route          VARCHAR(128) NOT NULL DEFAULT '',
    stage          VARCHAR(16)  NOT NULL CHECK (stage IN ('input', 'output')),
    row_name       VARCHAR(255) NOT NULL DEFAULT '',
    detector       VARCHAR(64)  NOT NULL DEFAULT '',
    detector_type  VARCHAR(16)  NOT NULL DEFAULT '',
    action         VARCHAR(16)  NOT NULL CHECK (action IN ('block', 'mask', 'flag')),
    outcome        VARCHAR(16)  NOT NULL CHECK (outcome IN (
                       'pass', 'flagged', 'masked', 'blocked', 'error_open', 'error_closed')),
    fail_mode      VARCHAR(8)   NOT NULL CHECK (fail_mode IN ('open', 'closed')),
    labels         TEXT[]       NOT NULL DEFAULT '{}',
    findings       INTEGER      NOT NULL DEFAULT 0 CHECK (findings >= 0),
    max_score      REAL         NOT NULL DEFAULT 0,
    latency_ms     BIGINT       NOT NULL DEFAULT 0 CHECK (latency_ms >= 0),
    runs           INTEGER      NOT NULL DEFAULT 1 CHECK (runs >= 1)
);

-- The run evidence read: the verdicts of one execution, in time order.
CREATE INDEX IF NOT EXISTS idx_guardrail_verdicts_execution
    ON gateway.guardrail_verdicts (execution_id, occurred_at)
    WHERE execution_id IS NOT NULL;

-- The retention prune.
CREATE INDEX IF NOT EXISTS idx_guardrail_verdicts_time
    ON gateway.guardrail_verdicts (occurred_at);
