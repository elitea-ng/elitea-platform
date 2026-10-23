-- Documents, not files (ADR-0028).
--
-- A source is no longer only a git repository: a connector lists documents
-- — a page, a ticket, a PDF in a bucket — each with a version, a media type
-- and an ACL. source_files becomes documents:
--
--   file_path    → document_key  (a repository path is one kind of key)
--   content_hash → version       (a content hash is one kind of version; a
--                                 connector may use an etag or a revision,
--                                 and a git document's version is still the
--                                 SHA-256 of its bytes, so no stored row
--                                 changes meaning)
--   + mime  — the document's media type (existing rows: unknown)
--   + acl   — who may read it: {"scope": "project"} (everyone with the
--             project: every row so far, git sources) or
--             {"scope": "restricted", "principals": [{"kind": "user" |
--             "group" | "email", "id": "…"}]}. Retrieval filters what a
--             caller sees by it.
--
-- 0001 and 0002 are applied and checksummed; they are not edited.

ALTER TABLE inventory_graph.source_files RENAME TO documents;
ALTER TABLE inventory_graph.documents RENAME COLUMN file_path TO document_key;
ALTER TABLE inventory_graph.documents RENAME COLUMN content_hash TO version;
ALTER TABLE inventory_graph.documents
    ADD COLUMN mime text  NOT NULL DEFAULT 'application/octet-stream',
    ADD COLUMN acl  jsonb NOT NULL DEFAULT '{"scope": "project"}'::jsonb,
    ADD CONSTRAINT documents_acl_scope
        CHECK (acl ->> 'scope' IN ('project', 'restricted'));

-- The restricted documents of a graph: what a read must filter. Most
-- graphs have none, and the index stays empty for them.
CREATE INDEX documents_restricted_idx
    ON inventory_graph.documents (project_id, application_id)
    WHERE acl ->> 'scope' = 'restricted';
