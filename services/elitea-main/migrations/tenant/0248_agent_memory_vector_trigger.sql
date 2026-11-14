-- 0248_agent_memory_vector_trigger.sql — tell the vector journal when an
-- agent's memory entry changes (issue 1657; the journal is shared/0248).
--
-- AFTER INSERT, UPDATE and DELETE on agent_memory_entries, one row per entry
-- goes to centry.agent_memory_vector_journal, keyed by this tenant's project id
-- (from the schema name, p_<id>) and the entry id, in the SAME transaction as
-- the change. That covers every path that changes an entry: the agent's
-- remember and forget tools, an edit, approve, archive or delete in the Memory
-- routes, the entry limit's archive, clear, a person's opt-out and the
-- retention purge. A reconciler reads the entry as it is when it works the row.
--
-- Only changes the index can see are journalled: an entry that is or was
-- `active` and is not a cursor (a cursor is the heartbeat's state, never
-- embedded), and an UPDATE that changed the content, the status or the kind.
-- A pin, a version bump or a touch of updated_at changes nothing the index
-- holds. An entry that becomes a cursor is journalled (its point goes).
--
-- The shared table may not exist yet when a tenant migrates first (the shared
-- and tenant histories apply independently), so the function looks for it each
-- time and does nothing without it: a memory write never fails because of the
-- journal. The existing active entries are enqueued once below, when the table
-- exists, so they are embedded without a write. When this tenant migrates
-- before the shared journal exists, shared/0248 enqueues them instead (it
-- visits every tenant schema that holds the table).
--
-- Idempotent. No BEGIN/COMMIT: the ledgered runner wraps the file.

DO $migration$
DECLARE
    tenant text := current_schema();
BEGIN
    IF tenant IS NULL THEN
        RAISE EXCEPTION 'agent memory vector trigger migration needs a tenant search_path';
    END IF;

    EXECUTE format($fn$
        CREATE OR REPLACE FUNCTION %1$I.agent_memory_vector_journal() RETURNS trigger
        LANGUAGE plpgsql AS $body$
        DECLARE
            v_project bigint;
            v_entry bigint;
            v_agent bigint;
        BEGIN
            IF TG_TABLE_SCHEMA !~ '^p_[1-9][0-9]{0,8}$'
               OR to_regclass('centry.agent_memory_vector_journal') IS NULL THEN
                RETURN NULL;
            END IF;
            IF TG_OP = 'DELETE' THEN
                IF OLD.status <> 'active' OR OLD.kind = 'cursor' THEN
                    RETURN NULL;
                END IF;
                v_entry := OLD.id;
                v_agent := OLD.application_id;
            ELSIF TG_OP = 'INSERT' THEN
                IF NEW.status <> 'active' OR NEW.kind = 'cursor' THEN
                    RETURN NULL;
                END IF;
                v_entry := NEW.id;
                v_agent := NEW.application_id;
            ELSE
                IF (NEW.kind = 'cursor' AND OLD.kind = 'cursor')
                   OR (NEW.status <> 'active' AND OLD.status <> 'active')
                   OR (NEW.status = OLD.status AND NEW.content = OLD.content AND NEW.kind = OLD.kind) THEN
                    RETURN NULL;
                END IF;
                v_entry := NEW.id;
                v_agent := NEW.application_id;
            END IF;
            v_project := substr(TG_TABLE_SCHEMA, 3)::bigint;
            INSERT INTO centry.agent_memory_vector_journal AS journal (project_id, entry_id, application_id)
            VALUES (v_project, v_entry, v_agent)
            ON CONFLICT (project_id, entry_id) DO UPDATE
            SET generation      = journal.generation + 1,
                attempts        = 0,
                last_error      = NULL,
                next_attempt_at = now(),
                claimed_until   = NULL;
            RETURN NULL;
        EXCEPTION WHEN undefined_table THEN
            RETURN NULL;
        END
        $body$ $fn$, tenant);

    IF to_regclass('agent_memory_entries') IS NOT NULL THEN
        DROP TRIGGER IF EXISTS agent_memory_vector_journal ON agent_memory_entries;
        CREATE TRIGGER agent_memory_vector_journal
            AFTER INSERT OR UPDATE OR DELETE ON agent_memory_entries
            FOR EACH ROW EXECUTE FUNCTION agent_memory_vector_journal();

        IF to_regclass('centry.agent_memory_vector_journal') IS NOT NULL
           AND tenant ~ '^p_[1-9][0-9]{0,8}$' THEN
            EXECUTE format($backfill$
                INSERT INTO centry.agent_memory_vector_journal (project_id, entry_id, application_id)
                SELECT %2$L::bigint, id, application_id
                FROM %1$I.agent_memory_entries
                WHERE status = 'active' AND kind <> 'cursor'
                ON CONFLICT (project_id, entry_id) DO NOTHING
            $backfill$, tenant, substr(tenant, 3));
        END IF;
    END IF;
END
$migration$;
