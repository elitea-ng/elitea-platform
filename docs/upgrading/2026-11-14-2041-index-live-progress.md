# Indexing: live progress frames from the Rust worker

Issue #2041.

**What changed.** The Rust worker now publishes nonterminal progress frames
(`NodeEvent` `agent_tool_start` / `agent_tool_end`) for `index.ingest.v1` runs:
one step for reading the source and the index, one for writing documents, and a
bounded number of document-count updates in between (at least five documents and
one second apart, at most twenty per run). Main already projects them to the
index activity and the execution stream, and the web index view shows them as
steps. The terminal frame is unchanged and takes the sequence after the last
acknowledged progress frame. A progress frame that Main rejects (Stop, the
deadline) ends the run with the matching failure terminal at that sequence. A
progress frame whose delivery stays uncertain after the bounded output attempts
leaves the command unacknowledged; its redelivery settles the spool from Main's
handoff watermark and reports the run failed.

**Who is affected.** Operators of deployments that run indexes on the Rust
worker. There is no migration and no setting.

**What to do.** Upgrade the worker with Main (Main must already ingest
`NodeEvent` frames for index runs, as in the previous release). An older worker
publishes no progress and behaves as before.
