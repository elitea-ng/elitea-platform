-- 0226_browser_engine_seconds.sql — the cloud browser's engine seconds as a
-- metered tool operation (ADR-0049 decision 8; issue #1596).
--
-- The worker's `browser` toolkit family admits and settles each cloud browser
-- call through the gateway's metered API, as the web families do (0174). A
-- call is billed by the engine seconds it held the session for: provider
-- `browser`, operation `engine_seconds`, unit `second`. The settle row in
-- gateway.llm_request_logs (operation `tool`, provider `browser`, model
-- `engine_seconds`, units the seconds) is what Main sums for the project's
-- monthly browser quota.
--
-- ## gateway.metered_operation_prices
--
-- The unit CHECK takes `second`. The seed price is 0: an administrator sets
-- the deployment's own price in the Tools pricing tab, and an absent row
-- would refuse every browser call as unpriced. A row an administrator
-- already wrote is kept (ON CONFLICT DO NOTHING).
--
-- No permission, no tenant change.

ALTER TABLE gateway.metered_operation_prices
    DROP CONSTRAINT IF EXISTS metered_operation_prices_unit;
ALTER TABLE gateway.metered_operation_prices
    ADD CONSTRAINT metered_operation_prices_unit
    CHECK (unit IN ('request', 'credit', 'page', 'second'));

INSERT INTO gateway.metered_operation_prices (provider, operation, unit, price_per_unit_usd) VALUES
    ('browser', 'engine_seconds', 'second', 0)
ON CONFLICT (provider, operation) DO NOTHING;

COMMENT ON TABLE gateway.metered_operation_prices IS
    'USD per ONE unit (request, credit, page or second) of a metered tool operation. An absent row is unpriced.';
