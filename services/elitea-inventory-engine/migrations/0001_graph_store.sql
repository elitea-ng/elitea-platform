-- Inventory knowledge-graph storage: the PostgreSQL replacement for graph.json.
--
-- ADR-0027. The Python engine keeps each Inventory toolkit's graph as ONE
-- `graph.json` object in the toolkit's artifact bucket: every tool call
-- downloads the whole document into a per-pod cache, rebuilds a networkx
-- graph in memory and, after an ingestion, uploads the whole document again.
-- Two runs on one toolkit overwrite each other, and a read on another pod
-- serves whatever its cache held. These tables hold the same graph as rows.
--
-- WHICH DATABASE THIS IS. The engine's own database (or a database it
-- shares with other engine sidecars), never the product database or a
-- tenant `p_{id}` schema. The migrations are owned, versioned and
-- checksummed here (`elitea-pg-migrate`, ledger
-- `inventory_graph.schema_migrations`) and applied by this service.
--
-- THE ADDRESS. A graph is addressed by (project_id, application_id): the
-- platform project and the Inventory toolkit, the pair the sub-application
-- host passes on every call. Both lead every key, so nothing is ever read
-- or deleted across projects.
--
-- WHY THE ATTRIBUTES ARE JSONB. Python nodes and edges are open attribute
-- maps: parser properties, LLM properties (a nested `properties` object),
-- fact fields and file statistics all spread onto the node. The columns the
-- engine filters by (name, type, layer, community, relation type) are
-- generated from the document, so they can never disagree with it.
--
-- WHY THE EMBEDDING IS float8[], NOT pgvector. Exported vectors must read
-- back as the numbers that were written, and `vector` stores float4.
-- Similarity search arrives with retrieval (P4), which decides between an
-- exact scan over this column and an indexed `vector(n)` copy.
--
-- ORDER. `ordinal` is insertion order: the node order of graph.json and the
-- tie order of the lexical search. An edge's ordinal orders it within its
-- source node, which is how networkx lists edges.

CREATE SCHEMA IF NOT EXISTS inventory_graph;

CREATE TABLE inventory_graph.graphs (
    project_id      bigint      NOT NULL,
    application_id  bigint      NOT NULL,
    -- node-link `graph`, `_metadata` (minus last_saved/version, which a
    -- document export stamps) and `_schema`
    attributes      jsonb       NOT NULL DEFAULT '{}'::jsonb,
    metadata        jsonb       NOT NULL DEFAULT '{}'::jsonb,
    schema_document jsonb,
    -- bumped by every write; a reader can tell a graph changed under it
    revision        bigint      NOT NULL DEFAULT 1,
    updated_at      timestamptz NOT NULL DEFAULT clock_timestamp(),
    PRIMARY KEY (project_id, application_id),
    CHECK (jsonb_typeof(attributes) = 'object'),
    CHECK (jsonb_typeof(metadata) = 'object')
);

CREATE TABLE inventory_graph.entities (
    project_id      bigint  NOT NULL,
    application_id  bigint  NOT NULL,
    entity_id       text    NOT NULL,
    ordinal         integer NOT NULL,
    -- every node attribute except citations and embedding
    attributes      jsonb   NOT NULL,
    -- the `citations` list; NULL when the node has no such attribute
    citations       jsonb,
    embedding       double precision[],
    name            text GENERATED ALWAYS AS (attributes ->> 'name') STORED,
    type            text GENERATED ALWAYS AS (attributes ->> 'type') STORED,
    layer           text GENERATED ALWAYS AS (attributes ->> 'layer') STORED,
    community_id    text GENERATED ALWAYS AS (attributes ->> 'community_id') STORED,
    PRIMARY KEY (project_id, application_id, entity_id),
    UNIQUE (project_id, application_id, ordinal),
    FOREIGN KEY (project_id, application_id)
        REFERENCES inventory_graph.graphs (project_id, application_id) ON DELETE CASCADE,
    CHECK (jsonb_typeof(attributes) = 'object')
);

CREATE INDEX entities_name_idx
    ON inventory_graph.entities (project_id, application_id, lower(name));
CREATE INDEX entities_type_idx
    ON inventory_graph.entities (project_id, application_id, type);
CREATE INDEX entities_community_idx
    ON inventory_graph.entities (project_id, application_id, community_id)
    WHERE community_id IS NOT NULL;
-- Containment lookups by file, document or source toolkit:
-- citations @> '[{"file_path": "src/a.py"}]'. Incremental re-ingestion
-- removes a changed file's citations through it.
CREATE INDEX entities_citations_idx
    ON inventory_graph.entities USING gin (citations jsonb_path_ops);

CREATE TABLE inventory_graph.relations (
    project_id      bigint  NOT NULL,
    application_id  bigint  NOT NULL,
    source_id       text    NOT NULL,
    target_id       text    NOT NULL,
    ordinal         integer NOT NULL,
    -- every edge attribute, `relation_type` included. An edge attribute
    -- named `source` (the provenance: parser, llm) is kept here although
    -- graph.json overwrites it with the source id.
    attributes      jsonb   NOT NULL,
    relation_type   text GENERATED ALWAYS AS (attributes ->> 'relation_type') STORED,
    -- one edge per ordered pair: the Python graph is not a multigraph
    PRIMARY KEY (project_id, application_id, source_id, target_id),
    FOREIGN KEY (project_id, application_id, source_id)
        REFERENCES inventory_graph.entities (project_id, application_id, entity_id) ON DELETE CASCADE,
    FOREIGN KEY (project_id, application_id, target_id)
        REFERENCES inventory_graph.entities (project_id, application_id, entity_id) ON DELETE CASCADE,
    CHECK (jsonb_typeof(attributes) = 'object')
);

CREATE INDEX relations_target_idx
    ON inventory_graph.relations (project_id, application_id, target_id);
CREATE INDEX relations_type_idx
    ON inventory_graph.relations (project_id, application_id, relation_type);
