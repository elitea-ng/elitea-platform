-- The host's stop fences (multi-replica host, #scaling).
--
-- A deletion on one host replica stops the matching runs of every replica
-- (the labels of 0009). The stop reads the running rows one time. Before
-- this table, a sibling replica could admit a new matching generation
-- right after that read, and the generation then published an index that
-- the deletion was removing.
--
--   host_stop_fences  one row for each deletion in progress: the tool it
--                     stops, the labels it matches (an empty value matches
--                     all values), and the time at which the row stops
--                     counting. The host writes the row BEFORE it reads the
--                     running rows, and deletes it when the deletion ends.
--                     The host refuses a new invocation of that tool whose
--                     labels match a live row. The fence and the admission
--                     take the same transaction lock, so no invocation can
--                     pass between the write and the read.
--
-- The expiry is a safety net for a host that stops before it deletes its
-- row. The host deletes expired rows.
--
-- 0001 to 0009 are applied and checksummed; they are not edited.

CREATE TABLE IF NOT EXISTS host_stop_fences (
    fence_id   text        PRIMARY KEY,
    tool_name  text        NOT NULL,
    labels     jsonb       NOT NULL DEFAULT '{}'::jsonb,
    expires_at timestamptz NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_host_stop_fences_tool
    ON host_stop_fences (tool_name, expires_at);
