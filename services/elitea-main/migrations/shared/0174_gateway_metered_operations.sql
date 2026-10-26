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
-- NO SCAN OF THE LEDGER TABLES. gateway.llm_usage_events and
-- gateway.llm_request_logs are the two hottest write paths of the gateway,
-- and the ledgered runner applies EVERY pending migration in ONE transaction
-- (migrate/runner.go apply), so each ALTER below holds ACCESS EXCLUSIVE on
-- its table until the whole upgrade commits. Nothing here may scan them under
-- that lock:
--
--   - `operation` has a CONSTANT default ('llm'), so adding it NOT NULL is a
--     catalog change: PostgreSQL stores the default once and rewrites and
--     scans nothing.
--   - Its CHECK is added NOT VALID, so the ALTER does not scan for violators.
--     PostgreSQL still enforces it for every new and updated row. Every
--     existing row reads the constant default 'llm', so no row can violate
--     it. It is not VALIDATEd here: inside the one migration transaction the
--     validation scan would run under the ALTER's lock (shared 0140 has the
--     same rule and test).
--   - No index is built on them. CREATE INDEX CONCURRENTLY cannot run inside
--     the runner's transaction, and nothing needs a (project, operation,
--     time) index: the Usage dimension read and the run analytics bound
--     their scans by project and time with 0084's
--     idx_llm_usage_events_project_time and 0100's request-log index, and
--     filter on operation within that range.
--
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

ALTER TABLE gateway.llm_usage_events ADD COLUMN IF NOT EXISTS operation VARCHAR(16) NOT NULL DEFAULT 'llm';
ALTER TABLE gateway.llm_usage_events ADD COLUMN IF NOT EXISTS units BIGINT;
ALTER TABLE gateway.llm_usage_events ADD COLUMN IF NOT EXISTS unit VARCHAR(16);

ALTER TABLE gateway.llm_request_logs ADD COLUMN IF NOT EXISTS operation VARCHAR(16) NOT NULL DEFAULT 'llm';
ALTER TABLE gateway.llm_request_logs ADD COLUMN IF NOT EXISTS units BIGINT;
ALTER TABLE gateway.llm_request_logs ADD COLUMN IF NOT EXISTS unit VARCHAR(16);
ALTER TABLE gateway.llm_request_logs ADD COLUMN IF NOT EXISTS cost_usd NUMERIC(20,8);

DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint
         WHERE conname = 'llm_usage_events_operation'
           AND conrelid = 'gateway.llm_usage_events'::regclass
    ) THEN
        ALTER TABLE gateway.llm_usage_events
            ADD CONSTRAINT llm_usage_events_operation
            CHECK (operation IN ('llm', 'tool'))
            NOT VALID;
    END IF;
    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint
         WHERE conname = 'llm_request_logs_operation'
           AND conrelid = 'gateway.llm_request_logs'::regclass
    ) THEN
        ALTER TABLE gateway.llm_request_logs
            ADD CONSTRAINT llm_request_logs_operation
            CHECK (operation IN ('llm', 'tool'))
            NOT VALID;
    END IF;
END
$$;

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
--
-- The partial index below is the one index this file builds, and it is kept
-- on purpose: the edge's execution verifier looks the turn up by this column
-- on EVERY metered admit and settle (repos/execution_attribution_verifier.go),
-- and no other execution_jobs index bounds that lookup, because a call
-- execution settled within the attribution slack must still verify. It is
-- not a ledger table, the column is new and NULL in every row, so the index
-- holds no entry; its build is one read of execution_jobs, inside the same
-- transaction as the ALTER that already locks it.
--
-- It is guarded on the table existing because this file is ALSO part of
-- infradb.GatewayMigrationSQL(): the gateway-only schema the dev bootstrap
-- and the gateway's Postgres test suites build, which carries the ledger
-- tables above but no elitea_runtime schema. Every ledgered deploy has
-- elitea_runtime.execution_jobs long before 0174, so there it always runs.
DO $$
BEGIN
    IF to_regclass('elitea_runtime.execution_jobs') IS NOT NULL THEN
        ALTER TABLE elitea_runtime.execution_jobs
            ADD COLUMN IF NOT EXISTS attribution_execution_id TEXT;
        CREATE INDEX IF NOT EXISTS idx_execution_jobs_attribution_execution_id
            ON elitea_runtime.execution_jobs (attribution_execution_id)
            WHERE attribution_execution_id IS NOT NULL;
    END IF;
END
$$;
