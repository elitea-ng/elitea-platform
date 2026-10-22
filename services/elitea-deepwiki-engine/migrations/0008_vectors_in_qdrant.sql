-- The dense vectors move to Qdrant behind elitea-vector (#1359; ADR-0031
-- DeepWiki D1-D3). This database holds no vector any more.
--
-- WHAT A WIKI RECORDS. `wikis.active_generation`: the elitea-vector
-- generation (the id of the build that wrote it) a reader searches. The
-- publish writes it in the same transaction as the nodes and edges, so a
-- reader that reads it in its snapshot searches the vectors of the nodes it
-- answers with. NULL: the wiki's last publish wrote no vector (it answers
-- lexically). A wiki published before this migration has its vectors only
-- in the table dropped below, so it starts with NULL and answers lexically
-- until it is generated again.
--
-- `wiki_stale_generations`: generations no reader names any more whose
-- points are still to be deleted, one row per (project, wiki, generation).
-- A table of its own, not a column of `wikis`: a build of a wiki that was
-- never published has no `wikis` row, and its generation must still be
-- recorded. A publish adds the generation it replaces (in its transaction,
-- under the per-wiki publish lock); a build removed without a bearer (the
-- startup reconciliation, the heartbeat sweep, the worker parent's
-- clean-up) or whose own delete failed is added too. Every holder of a
-- bearer for the project (a publish, an abandoned build, the parent's
-- clean-up with the invocation's bearer) then deletes the project's listed
-- generations, never one a wiki names as active, and removes the rows it
-- deleted; a failure leaves them listed for the next one. Never "every
-- generation but the active one": builds of one wiki run concurrently, and
-- an in-flight build's generation is not stale. A project deletion removes
-- its rows (elitea-main drops the project's points from elitea-vector).
--
-- WHAT GOES. `wiki_node_embeddings` and `deepwiki_build.wiki_node_embeddings`
-- (the rewritten 0001, 0003 and 0005 no longer create them; a database that
-- applied the released texts has them), and then the `vector` extension:
--
-- * only when it exists (a fresh install never created it);
-- * only when nothing else depends on it (`RESTRICT`): an object of another
--   service in this database that uses the type keeps the extension, and
--   the migration still succeeds;
-- * only when this role may drop it: an extension created by a superuser
--   cannot be dropped by the engine's role, and the migration still
--   succeeds. A database administrator can drop it later.
--
-- A replica of the previous release still running during a rolling update
-- reads and writes the dropped tables and fails its dense searches and its
-- publishes until it is replaced.

ALTER TABLE wikis ADD COLUMN IF NOT EXISTS active_generation TEXT
    CHECK (active_generation IS NULL OR active_generation ~ '^[A-Za-z0-9._:-]{1,128}$');
CREATE TABLE IF NOT EXISTS wiki_stale_generations (
    project_id  INTEGER     NOT NULL,
    wiki_id     TEXT        NOT NULL,
    generation  TEXT        NOT NULL CHECK (generation ~ '^[A-Za-z0-9._:-]{1,128}$'),
    recorded_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (project_id, wiki_id, generation)
);

DROP TABLE IF EXISTS wiki_node_embeddings;
DROP TABLE IF EXISTS deepwiki_build.wiki_node_embeddings;

DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM pg_extension WHERE extname = 'vector') THEN
        BEGIN
            DROP EXTENSION vector RESTRICT;
        EXCEPTION
            WHEN dependent_objects_still_exist THEN
                RAISE NOTICE 'the vector extension is still used by other objects; it is kept';
            WHEN insufficient_privilege THEN
                RAISE NOTICE 'this role may not drop the vector extension; it is kept';
        END;
    END IF;
END
$$;
