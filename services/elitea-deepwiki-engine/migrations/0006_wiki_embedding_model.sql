-- The embedding model a wiki's vectors were made with (ADR-0031 phase D0,
-- decision 6: `ask` reads the model from the wiki row, not from call
-- arguments).
--
-- Both columns are nullable and have no default: a wiki published before
-- this migration has no recorded model, and `ask` keeps using the model its
-- caller names for those. A publish writes the model name it embedded with
-- and the dimension the model client's first response established; a publish
-- that embedded nothing writes NULL, because the publish replaces the wiki's
-- embeddings wholesale and a stale model would describe vectors that are gone.
ALTER TABLE wikis ADD COLUMN IF NOT EXISTS embedding_model TEXT;
ALTER TABLE wikis ADD COLUMN IF NOT EXISTS embedding_dim INTEGER
    CHECK (embedding_dim IS NULL OR embedding_dim > 0);
