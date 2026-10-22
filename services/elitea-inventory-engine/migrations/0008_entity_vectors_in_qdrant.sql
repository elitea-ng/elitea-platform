-- Entity vectors move to Qdrant behind elitea-vector (#1359; ADR-0031
-- decision 7 reversed by the owner on 2026-10-21: one vector solution).
--
-- WHAT A DATABASE THAT APPLIED THE RELEASED 0004 STILL HOLDS, AND WHAT GOES.
-- The released 0004 created the `vector` extension and the generated column
-- `inventory_graph.entities.embedding_vector`. The column goes. The extension
-- goes only when nothing else uses it: another engine may share this
-- database and keep pgvector columns of its own, so the drop is RESTRICT and
-- a refusal (dependent objects, or a role that does not own the extension)
-- is a NOTICE, not a failure. On a fresh install (the rewritten 0004) there
-- is neither, and every statement here is a no-op for them.
--
-- WHAT THE GRAPH ROW RECORDS NOW. Every write of a graph upserts its
-- non-zero vectors into its namespace (`store::vectors::namespace_id`) under
-- a NEW generation BEFORE its transaction opens (no lock is held across the
-- call to elitea-vector), and records here, in its transaction:
--
--   vector_generation      the generation a ranking searches; NULL means the
--                          graph's vectors are not in Qdrant (a database
--                          upgraded from pgvector, or a write made without
--                          an invocation bearer, e.g. the `import-graph`
--                          command). The first ranking with a bearer then
--                          upserts them from `entities.embedding` (the
--                          backfill, under the graph's write lock) and sets
--                          it, so no graph needs rebuilding.
--   vector_model_slug      the embedding space's model slug, from the
--                          `_metadata.embeddings_model` stamp
--                          (`store::vectors::model_slug`)
--   vector_dimension       the width of the graph's vectors (the first
--                          non-empty one, by ordinal); NULL: no vector
--   vector_other_dimension another width some entity has, when they differ;
--                          every ranking of such a graph is numpy's
--                          "shapes ... not aligned" refusal, as it was
--   vector_stale_generations the generations a commit superseded and that
--                          are not deleted from elitea-vector yet. Each
--                          commit appends the one it replaced; the sweep
--                          after a commit deletes exactly these (never
--                          "every generation but the current one", which
--                          would delete a concurrent writer's points before
--                          it commits) and removes them from the list
--
-- 0001 to 0007 are applied and checksummed; 0004 was rewritten (see it).

ALTER TABLE inventory_graph.entities DROP COLUMN IF EXISTS embedding_vector;

DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM pg_extension WHERE extname = 'vector') THEN
        BEGIN
            DROP EXTENSION vector RESTRICT;
        EXCEPTION
            WHEN dependent_objects_still_exist THEN
                RAISE NOTICE 'the vector extension is kept: other objects in this database still use it';
            WHEN insufficient_privilege THEN
                RAISE NOTICE 'the vector extension is kept: this role does not own it';
        END;
    END IF;
END
$$;

ALTER TABLE inventory_graph.graphs
    ADD COLUMN vector_generation      text,
    ADD COLUMN vector_model_slug      text,
    ADD COLUMN vector_dimension       integer,
    ADD COLUMN vector_other_dimension integer,
    ADD COLUMN vector_stale_generations text[] NOT NULL DEFAULT '{}';
