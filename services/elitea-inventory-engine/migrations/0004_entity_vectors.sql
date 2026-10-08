-- Similarity search in PostgreSQL (pgvector).
--
-- 0001 kept the embedding as float8[] so an export reads back the numbers
-- that were written, and left the choice of search to retrieval. The
-- engine ranked every entity of a graph in Rust, brute force. Here
-- PostgreSQL ranks them: `embedding_vector` is the same vector as pgvector's
-- `vector` (float4, as numpy's float32 compared them), generated from
-- `embedding` so the two can never disagree, and `semantic_search` orders
-- by cosine distance (`<=>`).
--
-- NO DIMENSION AND NO INDEX YET. A graph's width is its embedding model's
-- (`_metadata.embeddings_dimension`), so the column takes any width and the
-- scan is exact, per (project, toolkit). An HNSW index needs one width per
-- column (`vector(n)`, at most 2000; `halfvec(n)` up to 4000): a deployment
-- that settles on one model adds it as a partial expression index, e.g.
--
--   CREATE INDEX ON inventory_graph.entities
--       USING hnsw ((embedding_vector::halfvec(2560)) halfvec_cosine_ops)
--       WHERE vector_dims(embedding_vector) = 2560;
--
-- An empty array has no vector (pgvector refuses zero dimensions).

CREATE EXTENSION IF NOT EXISTS vector;

ALTER TABLE inventory_graph.entities
    ADD COLUMN embedding_vector vector GENERATED ALWAYS AS (
        CASE WHEN cardinality(embedding) > 0 THEN embedding::vector END
    ) STORED;
