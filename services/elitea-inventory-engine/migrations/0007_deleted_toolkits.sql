-- Tombstones of deleted toolkits (issue #1244 review round 2, ADR-0031 C0).
--
-- A deletion takes the graph's ingestion lease, so no run that holds it can
-- interleave; but a run that STARTS after the lease is released (it was
-- registered as the deletion began, or runs on another replica) would create
-- the graph again, and the platform no longer has the toolkit to delete it
-- with. The deleting transaction writes the toolkit's tombstone here, and
-- every write that takes the lease (an ingestion, an import, a source
-- removal, a type normalisation) checks for one under that same lease and
-- refuses when it finds it: a deleted toolkit's graph is never resurrected.
--
-- The platform never reuses a toolkit id (ids are a serial), so a tombstone
-- can never refuse a legitimately new toolkit; it is purged after 30 days,
-- long after any in-flight write has ended (`store::delete::purge_tombstones`).
--
-- 0001 to 0006 are applied and checksummed; they are not edited.

CREATE TABLE inventory_graph.deleted_toolkits (
    project_id     bigint      NOT NULL,
    application_id bigint      NOT NULL,
    deleted_at     timestamptz NOT NULL DEFAULT clock_timestamp(),
    PRIMARY KEY (project_id, application_id)
);

CREATE INDEX deleted_toolkits_deleted_at_idx
    ON inventory_graph.deleted_toolkits (deleted_at);
