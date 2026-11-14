-- 0248_agent_memory_vector_journal.sql — the journal of an agent's memory
-- vectors (ADR-0050 decision 1b, issue 1657).
--
-- WHY. An agent's memory entries (tenant/0163) are embedded into elitea-vector
-- (source agent_memory) so recall can rank by meaning. elitea-vector and the
-- embedding model are other processes, so one transaction cannot span them, and
-- a memory write must never fail or wait because they are down. The entry row
-- is the authority and the vector is a copy, so the two are reconciled:
--
--   1. THE ROW DECIDES. A trigger on the tenant table (tenant/0248) writes one
--      row here in the same transaction that inserts, edits, archives or
--      deletes an entry: every path (the agent's tools, the Memory routes,
--      the entry limit, clear, opt-out, the retention purge) is covered with
--      no path to remember.
--   2. A RECONCILER WORKS THE ROW. It reads the entry as it is NOW: active
--      and not a cursor, the entry is embedded and its point written; any
--      other state (archived, pending, deleted, gone) removes its point. The
--      journal says "look at this entry", never what to do, so a lost or
--      repeated row is harmless and an old row never resurrects a vector.
--
-- (project_id, entry_id) is the key: an entry id is a tenant-schema serial,
-- so a second change of the same entry REOPENS the row. `generation` is 1 at
-- insert and +1 on every reopen; the reconciler closes a row only at the
-- generation it read, so a change made while it worked keeps the row open.
--
-- attempts / next_attempt_at / claimed_until / last_error are the journal
-- mechanics of shared/0161: runs so far, the backoff, the lease of the run
-- working the row (a short FOR UPDATE SKIP LOCKED claim), and the last
-- failure by class only, never a raw error.
--
-- No foreign key to centry.project: a project delete drops the project's
-- vectors (DropProject) and its tenant schema; rows left here find no entry
-- and the reconciler's removal is then a no-op that closes them.
-- Idempotent. No BEGIN/COMMIT: the ledgered runner wraps the file.

CREATE TABLE IF NOT EXISTS centry.agent_memory_vector_journal (
    project_id      BIGINT      NOT NULL CHECK (project_id > 0),
    entry_id        BIGINT      NOT NULL CHECK (entry_id > 0),
    application_id  BIGINT      NOT NULL CHECK (application_id > 0),
    generation      BIGINT      NOT NULL DEFAULT 1,
    attempts        INTEGER     NOT NULL DEFAULT 0,
    last_error      TEXT,
    next_attempt_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    claimed_until   TIMESTAMPTZ,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (project_id, entry_id)
);

-- The reconciler's read: due rows, oldest retry first.
CREATE INDEX IF NOT EXISTS agent_memory_vector_journal_due_idx
    ON centry.agent_memory_vector_journal (next_attempt_at);

COMMENT ON TABLE centry.agent_memory_vector_journal IS
    'Journal of agent memory vectors (issue 1657, shared/0248). A trigger on each tenant agent_memory_entries writes a row when an entry changes; a reconciler in elitea-main embeds or removes the entry''s point from the entry row''s current state and closes the row.';
