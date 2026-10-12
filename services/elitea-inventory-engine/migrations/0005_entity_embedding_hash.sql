-- The hash of the text each entity's vector was embedded from.
--
-- ADR-0031 decision 7 (phase C0). `embed_graph` embedded only the entities
-- without a vector, so an entity whose name or description changed kept the
-- vector of its old text for good. The engine now records, beside the
-- vector, a SHA-256 (lowercase hex) of the composed embedding text
-- (`elitea_inventory_core::embed::compose_text`), and embeds an entity again
-- when the text it would be embedded from today hashes differently.
--
-- NULL means "no hash recorded": an entity embedded before this column
-- existed, or one without a vector. Such an entity with a vector is embedded
-- once more by the next ingestion, which records its hash. Nothing reads the
-- column but the ingestion's embedding step, and `load_view` never selects
-- it.
--
-- 0001 to 0004 are applied and checksummed; they are not edited.

ALTER TABLE inventory_graph.entities ADD COLUMN embedding_text_hash text;
