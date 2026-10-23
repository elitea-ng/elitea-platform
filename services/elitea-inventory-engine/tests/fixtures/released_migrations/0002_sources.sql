-- Per-source ingestion state: the replacement for sources_status.json and
-- the .ingestion-checkpoint-{source}.json files beside graph.json.
--
-- sources: one row per source toolkit of a graph, keyed by the toolkit id
-- as sources_status.json was (`toolkit_id` is text there too). The columns
-- are that document's fields, so `get_sources_status` can be answered from
-- the rows; `commit_sha` is new: the commit the last run read.
--
-- source_files: the content hash of every file the last completed run of a
-- source read, keyed by the source NAME — the value every citation carries
-- as `source_toolkit`. The next run skips a file whose hash is unchanged
-- and removes the citations of a changed or deleted one before reading it
-- again. (The Python checkpoint kept the same hashes; its removal step read
-- a citation key the graph no longer had, so a changed file's old
-- entities were never removed, and a deleted file's never at all.)
--
-- Neither table references inventory_graph.graphs: the status of a first
-- ingestion exists before its graph does. A graph delete removes both
-- explicitly (store::delete).

CREATE TABLE inventory_graph.sources (
    project_id          bigint      NOT NULL,
    application_id      bigint      NOT NULL,
    toolkit_id          text        NOT NULL,
    toolkit_name        text        NOT NULL,
    toolkit_type        text        NOT NULL,
    status              text        NOT NULL
        CHECK (status IN ('pending', 'in_progress', 'completed', 'error')),
    started_at          timestamptz,
    last_updated        timestamptz NOT NULL DEFAULT clock_timestamp(),
    entities_count      bigint      NOT NULL DEFAULT 0,
    relations_count     bigint      NOT NULL DEFAULT 0,
    documents_processed bigint      NOT NULL DEFAULT 0,
    error_message       text,
    progress_message    text,
    branch              text,
    commit_sha          text,
    PRIMARY KEY (project_id, application_id, toolkit_id)
);

CREATE TABLE inventory_graph.source_files (
    project_id      bigint NOT NULL,
    application_id  bigint NOT NULL,
    source_name     text   NOT NULL,
    file_path       text   NOT NULL,
    content_hash    text   NOT NULL,
    PRIMARY KEY (project_id, application_id, source_name, file_path)
);
