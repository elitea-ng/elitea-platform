-- 0216_platform_preamble_revisions.sql — the revision history of the
-- platform preamble (ADR-0043 decision 3, issue #1500).
--
--   elitea_runtime.platform_preamble_revisions
--       One row per save of the platform configuration section
--       `platform_preamble`: instruction text that the worker puts before
--       every agent's system prompt at run time, outside the agent author's
--       control. ADR-0043 makes the preamble versioned like any other
--       governed configuration (ADR-0040 change control): each save that
--       changes the text is a new revision, numbered from 1, with its
--       author, its time and the sha256 of its text. The same transaction
--       writes the revision and one security audit event
--       (ai.elitea.security.v1.config.changed, action
--       config.platform_preamble.revised, the revision and digest as
--       reasons). Main sends the current text and its revision on each agent
--       command (WorkerCommandV1.content_controls, field 71), so the command
--       names the revision a run used. The rows are append-only: a revision
--       is never changed or deleted, so an earlier preamble stays
--       readable as evidence.
--
-- The preamble text is platform configuration that a platform
-- administrator writes. It holds no user content and no secret.

CREATE SCHEMA IF NOT EXISTS elitea_runtime;

CREATE TABLE IF NOT EXISTS elitea_runtime.platform_preamble_revisions (
    revision    integer PRIMARY KEY CHECK (revision > 0),
    text        text NOT NULL CHECK (octet_length(text) <= 8192 AND text !~ '\x00'),
    sha256      text NOT NULL CHECK (sha256 ~ '^[0-9a-f]{64}$'),
    saved_by    text NOT NULL CHECK (length(saved_by) BETWEEN 1 AND 320),
    saved_at    timestamptz NOT NULL DEFAULT clock_timestamp(),
    -- The mirrored security audit event (0183 outbox event_id).
    audit_event_id uuid NOT NULL UNIQUE
);

CREATE OR REPLACE FUNCTION elitea_runtime.platform_preamble_revisions_append_only()
RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'elitea_runtime.platform_preamble_revisions is append-only';
END
$$;

DROP TRIGGER IF EXISTS platform_preamble_revisions_append_only
    ON elitea_runtime.platform_preamble_revisions;
CREATE TRIGGER platform_preamble_revisions_append_only
    BEFORE UPDATE OR DELETE ON elitea_runtime.platform_preamble_revisions
    FOR EACH ROW EXECUTE FUNCTION elitea_runtime.platform_preamble_revisions_append_only();
