-- Issue #965: shrink the agent admission start-path critical section.
--
-- The agent start path used to hold the capability's
-- execution_admission_policies row lock across the whole multi-insert
-- admission transaction (input bundle, execution job, binding, current turn,
-- command outbox) plus a synchronous commit. The commit's WAL fsync dominated
-- the lock hold, so a 100-start burst on 3 replicas measured p95 > 700 ms.
--
-- This split reserves the durable slot in a short single-statement transaction
-- and materializes the heavy durable writes afterward, without holding the
-- policy row lock. agent_admission_reservations holds one row per in-flight
-- (or just-admitted) agent start, keyed by the same idempotency anchor as
-- execution_jobs. The admission cap stays a live re-computation of durable
-- active executions plus unmaterialized reservations, so a slot is neither
-- lost nor double-counted across the two-phase window, and a terminal
-- execution frees its slot by leaving the active set.

CREATE TABLE IF NOT EXISTS elitea_runtime.agent_admission_reservations (
    capability_id text NOT NULL,
    idempotency_scope text NOT NULL,
    idempotency_key text NOT NULL,
    execution_id text NOT NULL,
    configured_max bigint NOT NULL,
    reserved_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    materialized_at timestamptz,
    PRIMARY KEY (capability_id, idempotency_scope, idempotency_key),
    CONSTRAINT agent_admission_reservation_capability_length
        CHECK (octet_length(capability_id) BETWEEN 1 AND 256),
    CONSTRAINT agent_admission_reservation_configured_max
        CHECK (configured_max BETWEEN 1 AND 1024)
);

-- Backs the per-capability unmaterialized count the cap guard reads while the
-- policy row is locked, and the reaper's stale scan.
CREATE INDEX IF NOT EXISTS agent_admission_reservations_unmaterialized_idx
    ON elitea_runtime.agent_admission_reservations (capability_id, reserved_at)
    WHERE materialized_at IS NULL;

-- Backs the reaper's garbage-collect scan of materialized reservations.
CREATE INDEX IF NOT EXISTS agent_admission_reservations_materialized_idx
    ON elitea_runtime.agent_admission_reservations (materialized_at)
    WHERE materialized_at IS NOT NULL;

CREATE OR REPLACE FUNCTION elitea_runtime.guard_agent_admission_reservation()
RETURNS trigger
LANGUAGE plpgsql
AS $$
DECLARE
    persisted_max  bigint;
    active_count   bigint;
    reserved_count bigint;
BEGIN
    -- Serialize on the capability policy row. Take the lock BEFORE the replay
    -- check so a same-key retry sees every reservation that committed while it
    -- waited; without this a retry racing the original at a full cap would fall
    -- through to the cap check and surface as a false E9652.
    SELECT max_outstanding
    INTO persisted_max
    FROM elitea_runtime.execution_admission_policies
    WHERE capability_id = NEW.capability_id
    FOR UPDATE;

    IF persisted_max IS NULL THEN
        RAISE EXCEPTION 'agent admission policy missing for capability %', NEW.capability_id
            USING ERRCODE = 'E9650';
    END IF;

    -- Replay fast path: an identical (capability, scope, key) reservation is
    -- already present. Skip the guard; the caller's ON CONFLICT DO NOTHING
    -- drops this row and the start re-enters the materialize path.
    IF EXISTS (
        SELECT 1
        FROM elitea_runtime.agent_admission_reservations
        WHERE capability_id = NEW.capability_id
          AND idempotency_scope = NEW.idempotency_scope
          AND idempotency_key = NEW.idempotency_key
    ) THEN
        RETURN NEW;
    END IF;

    -- Fail closed when the persisted ceiling differs from the configured one.
    IF persisted_max <> NEW.configured_max THEN
        RAISE EXCEPTION 'agent admission policy mismatch: persisted % configured %',
            persisted_max, NEW.configured_max
            USING ERRCODE = 'E9651';
    END IF;

    -- Cap: durable active executions plus unmaterialized reservations. A
    -- terminal execution leaves the active set and frees its slot; a
    -- materialized reservation stops counting here.
    SELECT count(*)
    INTO active_count
    FROM elitea_runtime.execution_jobs
    WHERE capability_id = NEW.capability_id
      AND state IN ('PENDING', 'DISPATCHED', 'CLAIMED', 'RUNNING', 'SETTLING');

    SELECT count(*)
    INTO reserved_count
    FROM elitea_runtime.agent_admission_reservations
    WHERE capability_id = NEW.capability_id
      AND materialized_at IS NULL;

    IF active_count + reserved_count >= persisted_max THEN
        RAISE EXCEPTION 'agent admission capacity exhausted for capability %', NEW.capability_id
            USING ERRCODE = 'E9652';
    END IF;

    RETURN NEW;
END;
$$;

CREATE TRIGGER agent_admission_reservation_guard
BEFORE INSERT ON elitea_runtime.agent_admission_reservations
FOR EACH ROW
EXECUTE FUNCTION elitea_runtime.guard_agent_admission_reservation();
