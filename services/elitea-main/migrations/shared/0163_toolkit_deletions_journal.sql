-- 0163_toolkit_deletions_journal.sql — the cleanup journal of a toolkit delete
-- (#1244).
--
-- WHY. An Inventory toolkit's graph lives in the Inventory engine, not in the
-- tenant's elitea_tools row, so `DELETE FROM p_<id>.elitea_tools` leaves it
-- behind. The engine is another process reached over gRPC, which one
-- transaction cannot span, so the delete is split the way a project delete is
-- (shared/0161):
--
--   1. ONE TRANSACTION DECIDES. It deletes the toolkit row (RETURNING its type)
--      and, when the type belongs to an application whose engine keeps data per
--      toolkit, inserts a row here. The request never waits for the engine and
--      never depends on it being reachable: the toolkit is gone when the
--      transaction commits, and a database error deletes nothing.
--   2. THE ENGINE CALL RUNS FROM THIS ROW. A reconciler in elitea-main claims
--      incomplete rows past their backoff, asks the engine to delete the
--      toolkit's data, and closes the row with the outcome. The call is
--      idempotent on the engine's side, so a row may be worked any number of
--      times.
--
-- ONE ROW IS ONE ENGINE CALL. A toolkit type belongs to exactly one
-- application, so the row carries the application (`app`) and nothing else to
-- choose. `app` is the type's application (an exact allow-list of the types the
-- engines publish), recorded wherever the delete is taken, whether or not that
-- replica has a client for it; a replica that has no client for the row's
-- application leaves the row pending with a backoff (logged) rather than
-- closing it, and a replica that has one drains it.
--
-- OUTCOMES (`outcome`, set together with completed_at):
--   deleted           the engine deleted the toolkit's data
--   nothing_to_delete the engine held nothing for the toolkit
--   unsupported       the engine keeps nothing of this kind (UNIMPLEMENTED)
--   failed_permanent  the engine refused this process's certificate, or the
--                     request itself was invalid: retrying cannot fix it. The
--                     row is closed with the CLASS of the reason in `note`
--                     ("failed permanently: the engine refused this process's
--                     client certificate" / "...: the request was invalid")
--                     and the engine's own message is logged with the row's
--                     ids; an operator finds these with
--                     WHERE outcome = 'failed_permanent'.
--   superseded        a toolkit with this id exists again in the project
--                     (a restored database); nothing was touched
-- `note` is the engine's one-line account, or the class of a permanent
-- failure; never a raw error. `last_error` is the last failed run's cause by
-- class only ("engine unavailable; retry"), never a raw error either: it can
-- carry an address.
--
-- attempts / next_attempt_at / claimed_until are the journal mechanics of
-- shared/0161: runs so far, the backoff, and the lease of the run working the
-- row (claimed in a short FOR UPDATE SKIP LOCKED transaction, one row at a
-- time, released when the run ends).
--
-- (project_id, toolkit_id, app) is unique: a toolkit id is a tenant-schema
-- serial, so a second delete of the same id (the id came back) REOPENS the
-- row rather than adding one. The surrogate `id` is what the reconciler
-- claims by.
--
-- `generation` is the row's obligation counter: 1 at insert, +1 on every
-- reopen (which also clears the lease). A claim reads the generation with the
-- id, and every write a run makes (close, fail, release) requires it, so a run
-- still working the previous obligation — its engine call outlasted the
-- lease, or the row was reopened under it — matches no row and touches
-- nothing of the new one.
--
-- No foreign key to centry.project: the row outlives the toolkit, and may
-- outlive the project (whose own journal step then also deletes the data; the
-- engine answers "nothing to delete" to whichever comes second).
-- Idempotent. No BEGIN/COMMIT: the ledgered runner wraps the file.

CREATE TABLE IF NOT EXISTS centry.toolkit_deletions (
    id              BIGSERIAL   PRIMARY KEY,
    project_id      BIGINT      NOT NULL,
    toolkit_id      BIGINT      NOT NULL,
    app             TEXT        NOT NULL,
    toolkit_type    TEXT        NOT NULL,
    outcome         TEXT,
    note            TEXT,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    attempts        INTEGER     NOT NULL DEFAULT 0,
    last_error      TEXT,
    next_attempt_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    claimed_until   TIMESTAMPTZ,
    completed_at    TIMESTAMPTZ,
    generation      BIGINT      NOT NULL DEFAULT 1,
    CONSTRAINT toolkit_deletions_one_row_per_toolkit UNIQUE (project_id, toolkit_id, app),
    CONSTRAINT toolkit_deletions_outcome_check CHECK (
        outcome IS NULL OR outcome IN ('deleted', 'nothing_to_delete', 'unsupported', 'failed_permanent', 'superseded')
    )
);

-- The reconciler's read: incomplete rows, oldest retry first.
CREATE INDEX IF NOT EXISTS toolkit_deletions_pending_idx
    ON centry.toolkit_deletions (next_attempt_at, id)
    WHERE completed_at IS NULL;

COMMENT ON TABLE centry.toolkit_deletions IS
    'Cleanup journal of toolkit deletes (#1244, shared/0163). The toolkit row is deleted in the transaction that inserts this row; the engine-side deletion it records runs afterwards and is retried by a reconciler until completed_at is set.';
