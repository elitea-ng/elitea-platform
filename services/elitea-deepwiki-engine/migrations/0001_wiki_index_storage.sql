-- DeepWiki index storage: the PostgreSQL replacement for the per-wiki files.
--
-- ADR-0022 decision 3. The legacy engine kept every index as files on a PVC:
-- a SQLite `.wiki.db` (repo_nodes, repo_edges, an FTS5 virtual table, a
-- sqlite-vec virtual table), a separate `.bm25.sqlite` postings index, an mmap
-- docstore pair, FAISS binaries, and a gzipped code graph. A query could only
-- be answered by the pod that mounted the file, cache loss meant re-indexing,
-- and the global registry was a mutable JSON blob in an artifact bucket with
-- no locking. This file replaces that layer.
--
-- WHICH DATABASE THIS IS.
--
-- A dedicated `deepwiki` database on the platform PostgreSQL cluster — not the
-- product database, not a tenant `p_{id}` schema. Service-operational data
-- belongs to the service, and claiming product-side migrations for it is the
-- ownership crossing that has repeatedly caused migration collisions. These
-- migrations are owned, versioned and checksummed here (the elitea-llm-gateway
-- precedent) and are applied by this service, not by elitea-main.
--
-- WHY THE DENSE COLUMN HAS NO DIMENSION, AND NO HNSW INDEX YET.
--
-- `vector` without a typmod accepts any dimension, which the engine needs:
-- the legacy code sizes its vector table from the configured embedding model
-- (1536 for platform embeddings, 384 for the local MiniLM fallback). pgvector
-- cannot build an HNSW index on an unconstrained column, so this migration
-- creates none and dense search runs as an exact sequential scan.
--
-- That is deliberate for the parity phase, not an oversight. HNSW is
-- approximate; adding it in the same change as the storage port would mix two
-- sources of ranking difference and make the P0 retrieval fixtures unable to
-- tell them apart. Exact scan first, parity proven against the fixtures, then
-- HNSW with its own recall measurement against the now-trusted exact backend.
-- The follow-up migration adds `vector(n)` per deployment plus the index.

CREATE EXTENSION IF NOT EXISTS vector;

-- ---------------------------------------------------------------------------
-- Text search configuration
-- ---------------------------------------------------------------------------
--
-- The legacy FTS5 table used `tokenize='porter unicode61'`: Porter stemming,
-- unicode folding, and NO stopword removal. PostgreSQL's built-in `english`
-- configuration stems with Snowball AND strips stopwords, so it would drop
-- terms FTS5 keeps and change which rows match at all.
--
-- This configuration is the closest available equivalent: Snowball English
-- stemming with the stopword list omitted. It is not identical — Snowball
-- implements Porter2 where FTS5 implements Porter1 — and that residual
-- difference is measured against the P0 fixtures rather than assumed away.
--
-- THE TOKENIZER DIFFERENCE THAT ACTUALLY MATTERS, AND HOW IT IS CLOSED.
--
-- PostgreSQL's text-search parser is built for prose: it recognises hosts,
-- URLs, emails, file paths and version numbers and keeps them WHOLE. Over
-- source code that is catastrophic — `self.connection.execute` lexes as a
-- single `host` token, so `connection` is not indexed at all, and a query for
-- it finds nothing. The P0 fixture query `connection` matched 5 rows under
-- FTS5 and 2 under an unmodified `to_tsvector`; that is what caught it.
--
-- FTS5's `unicode61` tokenizer instead splits on every non-alphanumeric
-- character. `wiki_nodes.fts` therefore folds each such character to a space
-- *before* `to_tsvector` sees the text, which reproduces that rule. The query
-- side must apply the identical transform — see
-- `PostgresBackend.search_fts`, which uses the same expression. If the two
-- ever diverge, terms will be indexed that no query can name.
DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_ts_dict WHERE dictname = 'deepwiki_stem'
    ) THEN
        CREATE TEXT SEARCH DICTIONARY deepwiki_stem (
            TEMPLATE = snowball,
            Language = english
            -- StopWords deliberately omitted: FTS5 removed none.
        );
    END IF;

    IF NOT EXISTS (
        SELECT 1 FROM pg_ts_config WHERE cfgname = 'deepwiki_porter'
    ) THEN
        CREATE TEXT SEARCH CONFIGURATION deepwiki_porter (COPY = simple);
        ALTER TEXT SEARCH CONFIGURATION deepwiki_porter
            ALTER MAPPING FOR asciiword, asciihword, hword_asciipart,
                              word, hword, hword_part, numword, hword_numpart
            WITH deepwiki_stem;
    END IF;
END
$$;

-- ---------------------------------------------------------------------------
-- The wiki registry
-- ---------------------------------------------------------------------------
--
-- Replaces `_registry/wikis.json`, which `WikiRegistryManager.register_wiki`
-- rewrote whole under no lock: two concurrent generations read the same
-- document and the second write erased the first one's entry. A row with a
-- primary key cannot lose a neighbour.
--
-- `artifact_status` is carried over as a jsonb column but note what the legacy
-- code put in it: all-true, unconditionally, on every registration. It
-- recorded intent, never the presence of the artifacts. It is stored here for
-- shape compatibility with the existing UI and is not to be trusted as truth.
CREATE TABLE IF NOT EXISTS wikis (
    wiki_id                   TEXT PRIMARY KEY,
    repo                      TEXT        NOT NULL,
    branch                    TEXT        NOT NULL,
    provider                  TEXT        NOT NULL DEFAULT 'github',
    host                      TEXT        NOT NULL DEFAULT 'github.com',
    display_name              TEXT        NOT NULL DEFAULT '',
    description               TEXT        NOT NULL DEFAULT '',
    topics                    JSONB       NOT NULL DEFAULT '[]'::jsonb,
    folder_path               TEXT        NOT NULL DEFAULT '',
    commit_hash               TEXT,
    canonical_repo_identifier TEXT,
    analysis_key              TEXT,
    wiki_version_id           TEXT,
    artifact_status           JSONB       NOT NULL DEFAULT '{}'::jsonb,
    stats                     JSONB       NOT NULL DEFAULT '{}'::jsonb,
    created_at                TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at                TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS idx_wikis_repo_branch ON wikis (repo, branch);

-- ---------------------------------------------------------------------------
-- Nodes
-- ---------------------------------------------------------------------------
--
-- The legacy `repo_nodes` columns that any retrieval or rendering path reads.
-- Columns the legacy schema carried but nothing consulted (`analysis_level`,
-- `hub_assignment`, `return_type`, `parameters`) are omitted; adding them
-- later is additive and cheap, whereas carrying dead columns forward makes
-- them look load-bearing.
--
-- `fts` is a stored generated column so the index can never drift from the
-- text. The legacy code rebuilt its whole FTS5 table on every write
-- (`_populate_fts5` does DELETE then INSERT SELECT) precisely because it had
-- no such guarantee.
CREATE TABLE IF NOT EXISTS wiki_nodes (
    wiki_id        TEXT    NOT NULL REFERENCES wikis (wiki_id) ON DELETE CASCADE,
    node_id        TEXT    NOT NULL,
    rel_path       TEXT    NOT NULL DEFAULT '',
    file_name      TEXT    NOT NULL DEFAULT '',
    language       TEXT    NOT NULL DEFAULT '',
    start_line     INTEGER NOT NULL DEFAULT 0,
    end_line       INTEGER NOT NULL DEFAULT 0,
    symbol_name    TEXT    NOT NULL DEFAULT '',
    symbol_type    TEXT    NOT NULL DEFAULT '',
    parent_symbol  TEXT,
    source_text    TEXT    NOT NULL DEFAULT '',
    docstring      TEXT    NOT NULL DEFAULT '',
    signature      TEXT    NOT NULL DEFAULT '',
    chunk_type     TEXT,
    macro_cluster  INTEGER,
    micro_cluster  INTEGER,
    is_architectural BOOLEAN NOT NULL DEFAULT FALSE,
    is_doc           BOOLEAN NOT NULL DEFAULT FALSE,
    is_test          BOOLEAN NOT NULL DEFAULT FALSE,
    fts            TSVECTOR GENERATED ALWAYS AS (
        to_tsvector(
            'deepwiki_porter',
            regexp_replace(
                symbol_name || ' ' || signature || ' ' || docstring
                    || ' ' || source_text,
                '[^[:alnum:]]+', ' ', 'g'
            )
        )
    ) STORED,
    PRIMARY KEY (wiki_id, node_id)
);

CREATE INDEX IF NOT EXISTS idx_wiki_nodes_fts   ON wiki_nodes USING GIN (fts);
CREATE INDEX IF NOT EXISTS idx_wiki_nodes_path  ON wiki_nodes (wiki_id, rel_path);
CREATE INDEX IF NOT EXISTS idx_wiki_nodes_name  ON wiki_nodes (wiki_id, symbol_name);
CREATE INDEX IF NOT EXISTS idx_wiki_nodes_type  ON wiki_nodes (wiki_id, symbol_type);
CREATE INDEX IF NOT EXISTS idx_wiki_nodes_macro ON wiki_nodes (wiki_id, macro_cluster);

-- ---------------------------------------------------------------------------
-- Edges
-- ---------------------------------------------------------------------------
--
-- The legacy `repo_edges`, used by graph expansion. No retrieval branch in the
-- P0 fixtures reads it, so nothing here is parity-gated yet; it exists so the
-- engine's expansion path has somewhere to land in the next slice rather than
-- being tempted back onto a file.
CREATE TABLE IF NOT EXISTS wiki_edges (
    wiki_id    TEXT    NOT NULL REFERENCES wikis (wiki_id) ON DELETE CASCADE,
    source_id  TEXT    NOT NULL,
    target_id  TEXT    NOT NULL,
    rel_type   TEXT    NOT NULL,
    edge_class TEXT,
    weight     REAL    NOT NULL DEFAULT 1.0,
    metadata   JSONB   NOT NULL DEFAULT '{}'::jsonb,
    PRIMARY KEY (wiki_id, source_id, target_id, rel_type)
);

CREATE INDEX IF NOT EXISTS idx_wiki_edges_source ON wiki_edges (wiki_id, source_id);
CREATE INDEX IF NOT EXISTS idx_wiki_edges_target ON wiki_edges (wiki_id, target_id);

-- ---------------------------------------------------------------------------
-- Dense vectors
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS wiki_node_embeddings (
    wiki_id   TEXT   NOT NULL,
    node_id   TEXT   NOT NULL,
    embedding VECTOR NOT NULL,
    PRIMARY KEY (wiki_id, node_id),
    FOREIGN KEY (wiki_id, node_id)
        REFERENCES wiki_nodes (wiki_id, node_id) ON DELETE CASCADE
);

-- ---------------------------------------------------------------------------
-- BM25 term statistics
-- ---------------------------------------------------------------------------
--
-- ADR-0022's Consequences name this as the branch that must be re-expressed
-- rather than moved: "BM25 ranking must be re-expressed over PostgreSQL (term
-- statistics tables or `ts_rank`-based substitution)". These tables are the
-- first option, chosen because it is the only one that can be *exactly*
-- equal to the legacy scores rather than merely correlated with them.
--
-- They are a direct translation of the legacy `.bm25.sqlite` schema
-- (`meta`, `docs`, `terms`, `postings`), and the tokenizer is the same
-- whitespace split the legacy `_default_tokenizer` falls back to, so the
-- scores this produces are the scores the P0 fixtures recorded.
--
-- Two statistic sets exist per wiki, distinguished by `branch`:
--
--   'bm25' — the standalone index. Whitespace tokens over docstore text,
--            k1=1.5, b=0.75. Exact parity with the legacy index.
--   'fts'  — the lexical branch. `deepwiki_porter` lexemes over the node text,
--            k1=1.2, b=0.75. NOT numerically equal to SQLite FTS5's `bm25()`,
--            which is a different implementation over a different tokenizer.
--            Only the match set and the ordering are compared; see the parity
--            report for the measured divergence.
CREATE TABLE IF NOT EXISTS wiki_bm25_meta (
    wiki_id   TEXT             NOT NULL,
    branch    TEXT             NOT NULL,
    doc_count INTEGER          NOT NULL,
    avgdl     DOUBLE PRECISION NOT NULL,
    k1        DOUBLE PRECISION NOT NULL,
    b         DOUBLE PRECISION NOT NULL,
    PRIMARY KEY (wiki_id, branch)
);

CREATE TABLE IF NOT EXISTS wiki_bm25_docs (
    wiki_id TEXT    NOT NULL,
    branch  TEXT    NOT NULL,
    doc_idx INTEGER NOT NULL,
    node_id TEXT    NOT NULL,
    length  INTEGER NOT NULL,
    PRIMARY KEY (wiki_id, branch, doc_idx)
);

CREATE INDEX IF NOT EXISTS idx_wiki_bm25_docs_node
    ON wiki_bm25_docs (wiki_id, branch, node_id);

CREATE TABLE IF NOT EXISTS wiki_bm25_terms (
    wiki_id TEXT    NOT NULL,
    branch  TEXT    NOT NULL,
    term    TEXT    NOT NULL,
    df      INTEGER NOT NULL,
    PRIMARY KEY (wiki_id, branch, term)
);

CREATE TABLE IF NOT EXISTS wiki_bm25_postings (
    wiki_id TEXT    NOT NULL,
    branch  TEXT    NOT NULL,
    term    TEXT    NOT NULL,
    doc_idx INTEGER NOT NULL,
    tf      INTEGER NOT NULL,
    PRIMARY KEY (wiki_id, branch, term, doc_idx)
);

CREATE INDEX IF NOT EXISTS idx_wiki_bm25_postings_term
    ON wiki_bm25_postings (wiki_id, branch, term);
