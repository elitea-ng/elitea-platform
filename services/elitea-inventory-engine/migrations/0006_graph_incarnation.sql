-- Which incarnation of a graph a row is (issue #1244 review, ADR-0031 C0).
--
-- `revision` is bumped by every write of a graph, and a reader (the
-- retrieval `ViewCache`) reuses its cached copy while the stored revision is
-- the one it loaded. But a graph that is deleted and created again starts at
-- revision 1 again, so a replica that cached the DELETED graph's revision 1
-- would keep serving it for the new graph until the second write. The
-- incarnation tells them apart: it is drawn once when the `graphs` row is
-- inserted, kept by every update (`save` upserts the row), and gone with the
-- row. A cache compares (incarnation, revision), and equal stamps mean an
-- unchanged graph on this replica or another.
--
-- Existing rows get an incarnation of their own, as a new row would.
--
-- 0001 to 0005 are applied and checksummed; they are not edited.

ALTER TABLE inventory_graph.graphs
    ADD COLUMN graph_id uuid NOT NULL DEFAULT gen_random_uuid();
