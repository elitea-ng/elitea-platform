-- The build space: where a generation stages its index before publishing it.
--
-- ADR-0026 decision 5. The owner decision is "no SQLite anywhere": the
-- Rust-native engine has no local index file (no `.wiki.db`, no sqlite-vec,
-- no FAISS, no docstore). The Python engine built a `.wiki.db` on scratch and
-- `storage/publish.py` copied it into the 0001 tables row by row, with no
-- transaction around the whole: a reader during a publish could see the new
-- nodes with the old edges, or half of the new nodes. The native engine
-- writes into the tables below instead, and publishes in ONE transaction:
-- delete the wiki's live rows, `INSERT ... SELECT` from here, write the
-- `wiki_bm25_*` statistics and the `wikis` row, delete the build's rows.
-- Readers see the old index or the new one, never a part of one.
--
-- ADDITIVE ONLY. Nothing in 0001 or 0002 changes, and the Python migrator
-- applies this file as it applies the others (the same ledger, the same
-- checksum). The Python engine never writes here.
--
-- WHY A SEPARATE SCHEMA. The staging tables have the same names and columns
-- as the live ones (keyed by `build_id` instead of `wiki_id`), so the build
-- uses the same retrieval SQL as the query path. A schema keeps the two
-- apart without a prefix on every name, and nothing on the query path can
-- read a staged row by accident: every reader names `public` tables
-- unqualified, every writer names `deepwiki_build` tables qualified.
--
-- WHY UNLOGGED. A crashed build is rebuilt, never recovered. Unlogged tables
-- skip the write-ahead log (the bulk of a staging COPY's cost) and are
-- truncated after a crash, which is the right outcome for them. `builds` is
-- an ordinary (logged) table on purpose: after a crash its rows survive,
-- and the reconciliation finds and deletes them. Each staging table
-- references `builds` with ON DELETE CASCADE (an unlogged table may
-- reference a logged one), so deleting a build row removes all its rows.

CREATE SCHEMA IF NOT EXISTS deepwiki_build;

-- One row per build in progress. `owner` identifies the process that runs
-- the build (the pod name in Kubernetes). At startup an engine deletes the
-- builds of its own owner, which can only be its predecessor's abandoned
-- ones; it never touches another replica's. A periodic sweep deletes builds
-- whose `heartbeat_at` is older than a setting (default 2 h), which is how a
-- replica that went away for good is cleaned up.
CREATE TABLE IF NOT EXISTS deepwiki_build.builds (
    build_id     TEXT        PRIMARY KEY,
    wiki_id      TEXT        NOT NULL,
    owner        TEXT        NOT NULL,
    started_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    heartbeat_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS idx_builds_owner ON deepwiki_build.builds (owner);
CREATE INDEX IF NOT EXISTS idx_builds_heartbeat ON deepwiki_build.builds (heartbeat_at);

-- The staged copy of `wiki_nodes`. The `fts` expression is 0001's, character
-- for character: the build's lexical lookups must match exactly what the
-- published index will match.
CREATE UNLOGGED TABLE IF NOT EXISTS deepwiki_build.wiki_nodes (
    build_id       TEXT    NOT NULL
        REFERENCES deepwiki_build.builds (build_id) ON DELETE CASCADE,
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
    PRIMARY KEY (build_id, node_id)
);

CREATE INDEX IF NOT EXISTS idx_build_nodes_fts
    ON deepwiki_build.wiki_nodes USING GIN (fts);

-- The staged copy of `wiki_edges`. Same primary key: parallel graph edges
-- collapse onto (source, target, rel_type) before they are staged, exactly as
-- the Python publisher's ON CONFLICT collapsed them.
CREATE UNLOGGED TABLE IF NOT EXISTS deepwiki_build.wiki_edges (
    build_id   TEXT    NOT NULL
        REFERENCES deepwiki_build.builds (build_id) ON DELETE CASCADE,
    source_id  TEXT    NOT NULL,
    target_id  TEXT    NOT NULL,
    rel_type   TEXT    NOT NULL,
    edge_class TEXT,
    weight     REAL    NOT NULL DEFAULT 1.0,
    metadata   JSONB   NOT NULL DEFAULT '{}'::jsonb,
    PRIMARY KEY (build_id, source_id, target_id, rel_type)
);

CREATE INDEX IF NOT EXISTS idx_build_edges_target
    ON deepwiki_build.wiki_edges (build_id, target_id);

-- The staged copy of `wiki_node_embeddings`. No dimension, as in 0001.
CREATE UNLOGGED TABLE IF NOT EXISTS deepwiki_build.wiki_node_embeddings (
    build_id  TEXT   NOT NULL,
    node_id   TEXT   NOT NULL,
    embedding VECTOR NOT NULL,
    PRIMARY KEY (build_id, node_id),
    FOREIGN KEY (build_id, node_id)
        REFERENCES deepwiki_build.wiki_nodes (build_id, node_id) ON DELETE CASCADE
);

-- The standalone BM25 index's input, per staged node.
--
-- Not in ADR-0026's list of staging tables, and added for one reason: the
-- 'bm25' statistics branch tokenises with Python's `str.split()` (0001,
-- "BM25 term statistics"), which the engine reproduces exactly in Rust and
-- PostgreSQL's regular expressions cannot be trusted to. The engine therefore
-- tokenises each node while it stages it and writes the result here; the
-- publish transaction turns these rows into `wiki_bm25_*` rows with SQL
-- alone. A node whose text has no token has no row (the legacy skip rule).
-- `ord` is the node's position in the graph, which is the legacy `doc_idx`
-- order.
CREATE UNLOGGED TABLE IF NOT EXISTS deepwiki_build.bm25_docs (
    build_id TEXT    NOT NULL
        REFERENCES deepwiki_build.builds (build_id) ON DELETE CASCADE,
    node_id  TEXT    NOT NULL,
    ord      BIGINT  NOT NULL,
    length   INTEGER NOT NULL,
    PRIMARY KEY (build_id, node_id)
);

CREATE UNLOGGED TABLE IF NOT EXISTS deepwiki_build.bm25_postings (
    build_id TEXT    NOT NULL,
    node_id  TEXT    NOT NULL,
    term     TEXT    NOT NULL,
    tf       INTEGER NOT NULL,
    PRIMARY KEY (build_id, node_id, term),
    FOREIGN KEY (build_id, node_id)
        REFERENCES deepwiki_build.bm25_docs (build_id, node_id) ON DELETE CASCADE
);
