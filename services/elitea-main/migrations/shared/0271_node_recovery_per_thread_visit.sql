-- 0271_node_recovery_per_thread_visit.sql — per-thread pending operator
-- node-recovery visits (FU-071, T-5B 5b-5).
--
-- Replaces 0146's execution-wide unique pending index on
-- (execution_id, generation) with a per-thread unique pending index on
-- (execution_id, generation, graph_thread) so independent fan-out child
-- threads (p1:... / m1:...) can each hold at most one pending
-- SUSPENDED/AUTHORIZED visit without colliding across sibling branches or
-- items, while preserving the single-pending-visit invariant within each
-- graph thread. Adds a covering per-thread latest-visit index for
-- thread-scoped latest-visit selection and locking.
DROP INDEX elitea_runtime.node_recovery_one_pending_visit;

CREATE UNIQUE INDEX node_recovery_one_pending_visit
    ON elitea_runtime.node_recovery_visits(execution_id, generation, graph_thread)
    WHERE status IN ('SUSPENDED', 'AUTHORIZED');

CREATE INDEX node_recovery_latest_visit_by_thread
    ON elitea_runtime.node_recovery_visits(execution_id, generation, graph_thread, created_at DESC, journal_revision DESC);
