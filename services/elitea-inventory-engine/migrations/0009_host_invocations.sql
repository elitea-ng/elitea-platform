-- The sub-application host's durable invocation store, for Inventory
-- (multi-replica host, #scaling).
--
-- WHY THESE TABLES ARE HERE. The Inventory host (services/elitea-subapp-host,
-- ELITEA_SUBAPP=inventory) kept its invocations in process memory. A poll or
-- a cancel that a load balancer sent to another replica answered "unknown
-- invocation", and a toolkit deletion stopped only the ingests of the replica
-- it landed on. So the host was pinned to one replica. With these tables the
-- host keeps the same durable store DeepWiki's host keeps
-- (services/elitea-deepwiki-engine/migrations/0002_invocations.sql and
-- 0009_host_fleet.sql): every replica reads, cancels and stops every
-- invocation of the fleet, and a replica that stops reconciles the in-flight
-- rows of a replica that is gone.
--
-- WHO OWNS THEM. This engine owns the database schema and applies the
-- migration (ledger `inventory_graph.schema_migrations`); the HOST reads and
-- writes the rows (spi.PostgresStore with WithSchema("inventory_graph")).
-- The engine does not read them. The columns are the DeepWiki host's
-- columns, so one store implementation serves both applications.
--
-- THE COLUMNS THE FLEET ADDS.
--   labels      the facts a deletion finds a run by (project, toolkit),
--               written when the invocation is accepted, so a deletion on
--               any replica finds a run on any other replica.
--   holds_slot  true for an invocation that takes one of the fleet's
--               limited slots. Inventory has no slot limit, so the host
--               writes false; the column is here so that the store is one
--               shape for every application.
--   host_replicas
--               one row for each live host replica: its slot count, whether
--               it accepts new work, and when it last proved it is alive.
--
-- 0001 to 0008 are applied and checksummed; they are not edited.

CREATE TABLE inventory_graph.invocations (
    invocation_id  text        PRIMARY KEY,
    toolkit_name   text        NOT NULL,
    tool_name      text        NOT NULL,
    -- pending | running | stopped.
    status         text        NOT NULL,
    stop_requested boolean     NOT NULL DEFAULT false,
    -- The terminal body, verbatim, as the poll returns it.
    result         jsonb,
    created_at     timestamptz NOT NULL DEFAULT now(),
    finished_at    timestamptz,
    -- The host process that accepted the invocation (behind its liveness
    -- lock); a sibling ends the in-flight rows of an owner that is gone.
    owner          text,
    labels         jsonb       NOT NULL DEFAULT '{}'::jsonb,
    holds_slot     boolean     NOT NULL DEFAULT false
);

-- A poll is keyed by all three segments of the wire path.
CREATE INDEX idx_invocations_lookup
    ON inventory_graph.invocations (toolkit_name, tool_name, invocation_id);

-- Housekeeping scans terminal rows by age.
CREATE INDEX idx_invocations_finished
    ON inventory_graph.invocations (finished_at) WHERE finished_at IS NOT NULL;

-- Reconciliation scans in-flight rows by owner.
CREATE INDEX idx_invocations_owner
    ON inventory_graph.invocations (owner) WHERE finished_at IS NULL;

CREATE TABLE inventory_graph.invocation_events (
    id            bigint      GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    invocation_id text        NOT NULL
        REFERENCES inventory_graph.invocations (invocation_id) ON DELETE CASCADE,
    message       text        NOT NULL,
    created_at    timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX idx_invocation_events_drain
    ON inventory_graph.invocation_events (invocation_id, id);

CREATE TABLE inventory_graph.host_replicas (
    owner     text        PRIMARY KEY,
    slots     integer     NOT NULL,
    admitting boolean     NOT NULL DEFAULT true,
    seen_at   timestamptz NOT NULL DEFAULT now()
);
