-- 0169_egress_run_summaries.sql — the egress proxy's per-turn audit summaries,
-- held in the database instead of in each Main replica's memory (ADR-0034
-- decision 9, "Host allowlist: the egress proxy", Logging).
--
-- WHY. Each proxy replica reports a CUMULATIVE snapshot of its own view of a
-- run to Main over EgressAuditService. Main merges the replicas' snapshots
-- into ONE audit event per turn. Held in memory per Main replica, a turn
-- yielded one event per Main replica that heard a report, and a Main restart
-- lost the summaries it was holding. Here every Main replica writes the same
-- table and one of them claims the turn:
--
--   * Every report is an upsert of one (execution_id, replica) row; a repeated
--     or retried report replaces, never adds. A non-final report never
--     overwrites a final one of the same turn.
--   * A Main replica sweeps the table. A turn is due when every replica that
--     reported is final and the last report is older than the settle window, or
--     when the token's expiry plus the hold has passed (a crashed proxy replica
--     never sends its last report). The sweep CLAIMS a due turn with one
--     UPDATE ... SET audited_at RETURNING over the turn's unclaimed rows, so
--     exactly one Main replica receives the rows and writes the audit event.
--     A claimed row is kept for retention and is never claimed again.
--   * A report for a turn already claimed re-opens its row (audited_at back to
--     NULL, first_reported_at restarted): a resumed turn reuses the execution
--     id with a new run token and is a new turn. The other replicas' claimed
--     rows stay claimed and are not merged into it.
--   * Rows older than the retention are deleted by the sweep.
--
-- COLUMNS (hosts_reached / hosts_refused are the proxy's bounded lists, at most
-- 256 entries in all, as the gRPC service validates them).
--   final             the replica's last report for the run (its token expired
--                     there, or the replica is stopping).
--   token_expires_at  NULL when the report carried none.
--   audited_at        NULL until a Main replica claims the turn for its event.
CREATE TABLE IF NOT EXISTS elitea_runtime.egress_run_summaries (
    execution_id      TEXT        NOT NULL,
    replica           TEXT        NOT NULL,
    project_id        BIGINT      NOT NULL,
    final             BOOLEAN     NOT NULL,
    token_expires_at  TIMESTAMPTZ,
    connections       BIGINT      NOT NULL DEFAULT 0,
    bytes_up          BIGINT      NOT NULL DEFAULT 0,
    bytes_down        BIGINT      NOT NULL DEFAULT 0,
    other_hosts       BIGINT      NOT NULL DEFAULT 0,
    hosts_reached     JSONB       NOT NULL DEFAULT '[]'::jsonb,
    hosts_refused     JSONB       NOT NULL DEFAULT '[]'::jsonb,
    first_reported_at TIMESTAMPTZ NOT NULL,
    updated_at        TIMESTAMPTZ NOT NULL,
    audited_at        TIMESTAMPTZ,
    PRIMARY KEY (execution_id, replica),
    CONSTRAINT egress_run_summaries_counts_nonnegative CHECK (
        connections >= 0 AND bytes_up >= 0 AND bytes_down >= 0 AND other_hosts >= 0),
    CONSTRAINT egress_run_summaries_hosts_arrays CHECK (
        jsonb_typeof(hosts_reached) = 'array' AND jsonb_typeof(hosts_refused) = 'array')
);

-- The sweep reads only the turns nobody has claimed yet.
CREATE INDEX IF NOT EXISTS egress_run_summaries_unclaimed
    ON elitea_runtime.egress_run_summaries (execution_id)
    WHERE audited_at IS NULL;

-- The retention delete.
CREATE INDEX IF NOT EXISTS egress_run_summaries_updated
    ON elitea_runtime.egress_run_summaries (updated_at);
