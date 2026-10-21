-- 0170_execution_admission_cap_4096.sql — raise the durable admission hard
-- ceiling from 1024 to 4096 outstanding executions per capability (#966).
--
-- WHY. The 5k-user / 1k-flow load test (2026-09-20) put 1000 concurrent
-- executions under a 1024 ceiling with no headroom. The ceiling lives in two
-- CHECK constraints — the persisted policy row (0033) and the agent start
-- path's reservation row, which records the configured cap it was admitted
-- under (0126) — and in elitea-main's Go bound (runtimecomposition
-- maxRuntimeOutstanding, repos maxSupportedOutstandingJobs). All four move
-- together; a Go bound above a CHECK would only turn a raised cap into a
-- constraint violation on the first start.
--
-- The command bus's per-stream live-entry ceiling (commandbus
-- MaxStreamMessages, 1024) is NOT raised here. It bounds commands dispatched
-- but not yet claimed; a stream that fills answers CONTROL_STREAM_SATURATED,
-- which the dispatcher treats as backpressure, not as a failed admission.
--
-- No data changes: every persisted value already satisfies the wider check.
ALTER TABLE elitea_runtime.execution_admission_policies
    DROP CONSTRAINT execution_admission_policy_max_outstanding;

ALTER TABLE elitea_runtime.execution_admission_policies
    ADD CONSTRAINT execution_admission_policy_max_outstanding
        CHECK (max_outstanding BETWEEN 1 AND 4096);

ALTER TABLE elitea_runtime.agent_admission_reservations
    DROP CONSTRAINT agent_admission_reservation_configured_max;

ALTER TABLE elitea_runtime.agent_admission_reservations
    ADD CONSTRAINT agent_admission_reservation_configured_max
        CHECK (configured_max BETWEEN 1 AND 4096);
