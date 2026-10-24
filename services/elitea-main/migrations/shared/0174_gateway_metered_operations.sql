-- 0174_gateway_metered_operations.sql — the price catalog and the ledger
-- columns for metered tool operations (#1238): web search, web fetch and
-- internet host intelligence calls that the worker makes through the gateway's
-- /llm/v1/metered/admit and /llm/v1/metered/settle routes.
--
-- gateway.metered_operation_prices holds ONE price per (provider, operation):
--
--   unit                the unit the provider bills: request, credit or page
--   price_per_unit_usd  USD for ONE unit. This is NOT the per-1M denomination
--                       of gateway.gateway_models: a tool provider quotes a
--                       price per call, per credit or per page. The gateway
--                       reads it as exact nano-USD with
--                       (price_per_unit_usd * 1e9)::bigint, so the column
--                       keeps 8 decimal places and never goes through a float.
--   source              who wrote the row: `seed` for the rows below, `admin`
--                       for an operator edit
--   price_overridden    true when an operator changed the price; a later seed
--                       refresh must not overwrite that row
--
-- An ABSENT row means UNPRICED: the gateway refuses the admission with 422
-- metered_operation_unpriced. A zero price is a published price (ddg_instant
-- and the direct fetch cost nothing) and still writes a ledger row.
--
-- The seed prices come from the providers' public price sheets on 2026-10-24.
-- Verify them before a release; an operator can change each one in the admin
-- Tools pricing tab. ON CONFLICT DO NOTHING keeps every operator edit.
--
-- gateway.llm_usage_events and gateway.llm_request_logs get:
--
--   operation  `llm` for a model call (the default, so every old row and every
--              old writer stays valid), `tool` for a metered operation
--   units      the number of billed units of a tool row, NULL for an llm row
--   unit       the unit of `units`, NULL for an llm row
--   cost_usd   (request logs only) the exact cost of a tool row in USD
--
-- The writers send the new columns only for tool rows, so a gateway or a
-- scheduler that runs before this migration keeps writing llm rows.
--
-- Idempotent: CREATE ... IF NOT EXISTS and ADD COLUMN IF NOT EXISTS. No
-- BEGIN/COMMIT: the ledgered runner wraps the file.

CREATE TABLE IF NOT EXISTS gateway.metered_operation_prices (
    id                  BIGSERIAL     PRIMARY KEY,
    provider            VARCHAR(64)   NOT NULL,
    operation           VARCHAR(64)   NOT NULL,
    unit                VARCHAR(16)   NOT NULL,
    price_per_unit_usd  NUMERIC(20,8) NOT NULL,
    source              VARCHAR(16)   NOT NULL DEFAULT 'seed',
    price_overridden    BOOLEAN       NOT NULL DEFAULT false,
    price_overridden_at TIMESTAMPTZ,
    price_overridden_by INTEGER,
    updated_at          TIMESTAMPTZ   NOT NULL DEFAULT now(),
    CONSTRAINT metered_operation_prices_provider_operation UNIQUE (provider, operation),
    CONSTRAINT metered_operation_prices_unit CHECK (unit IN ('request', 'credit', 'page')),
    CONSTRAINT metered_operation_prices_price CHECK (price_per_unit_usd >= 0),
    CONSTRAINT metered_operation_prices_names
        CHECK (provider ~ '^[a-z0-9_]{1,64}$' AND operation ~ '^[a-z0-9_]{1,64}$')
);

INSERT INTO gateway.metered_operation_prices (provider, operation, unit, price_per_unit_usd) VALUES
    ('tavily',      'web_search',          'credit',  0.008),
    ('tavily',      'web_search_advanced', 'credit',  0.008),
    ('tavily',      'web_fetch',           'credit',  0.008),
    ('firecrawl',   'web_search',          'credit',  0.0038),
    ('firecrawl',   'web_fetch',           'page',    0.0038),
    ('brave',       'web_search',          'request', 0.005),
    ('ddg_instant', 'web_search',          'request', 0),
    ('direct',      'web_fetch',           'page',    0),
    ('shodan',      'host_search',         'credit',  0.0069),
    ('shodan',      'host_lookup',         'credit',  0.0069)
ON CONFLICT (provider, operation) DO NOTHING;

ALTER TABLE gateway.llm_usage_events ADD COLUMN IF NOT EXISTS operation VARCHAR(16) NOT NULL DEFAULT 'llm' CHECK (operation IN ('llm', 'tool'));
ALTER TABLE gateway.llm_usage_events ADD COLUMN IF NOT EXISTS units BIGINT;
ALTER TABLE gateway.llm_usage_events ADD COLUMN IF NOT EXISTS unit VARCHAR(16);

CREATE INDEX IF NOT EXISTS idx_llm_usage_events_project_operation_time
    ON gateway.llm_usage_events (project_id, operation, occurred_at);

ALTER TABLE gateway.llm_request_logs ADD COLUMN IF NOT EXISTS operation VARCHAR(16) NOT NULL DEFAULT 'llm' CHECK (operation IN ('llm', 'tool'));
ALTER TABLE gateway.llm_request_logs ADD COLUMN IF NOT EXISTS units BIGINT;
ALTER TABLE gateway.llm_request_logs ADD COLUMN IF NOT EXISTS unit VARCHAR(16);
ALTER TABLE gateway.llm_request_logs ADD COLUMN IF NOT EXISTS cost_usd NUMERIC(20,8);

COMMENT ON TABLE gateway.metered_operation_prices IS
    'USD per ONE unit (request, credit or page) of a metered tool operation. An absent row is unpriced.';
COMMENT ON COLUMN gateway.llm_usage_events.operation IS
    'llm for a model call, tool for a metered tool operation.';
COMMENT ON COLUMN gateway.llm_request_logs.cost_usd IS
    'The exact cost of a metered tool operation in USD. NULL for a model call.';

-- elitea_runtime.execution_jobs.attribution_execution_id is the desktop local
-- turn a remote toolkit call (toolkit.call_tool.v1) was admitted for. Main
-- writes it at admission, after it has verified the turn is the caller's and
-- live. The /llm edge accepts that turn's id as the execution of a metered
-- call only while this call execution is live and runs as the same actor in
-- the same project, so a tool's spend lands on the turn without the worker's
-- word for it. NULL for every other execution.
ALTER TABLE elitea_runtime.execution_jobs ADD COLUMN IF NOT EXISTS attribution_execution_id TEXT;

CREATE INDEX IF NOT EXISTS idx_execution_jobs_attribution_execution_id
    ON elitea_runtime.execution_jobs (attribution_execution_id)
    WHERE attribution_execution_id IS NOT NULL;
