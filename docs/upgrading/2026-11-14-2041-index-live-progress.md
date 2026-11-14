# Indexing: live progress frames from the Rust worker

Issue #2041.

**What changed.** The Rust worker now publishes nonterminal progress frames
(`NodeEvent` `agent_tool_start` / `agent_tool_end`) for `index.ingest.v1` runs:
one step for reading the source and the index, and one running step for writing
documents whose text is updated with the document count (one frame per update, at
least five documents and one second apart, at most twenty updates per run).
Main already projects them to the index activity and the execution stream, and
the web index view shows them as steps. The run never waits for the output
stream: frames are queued and the newest document count wins.

**Progress is best effort.** A broken or slow output stream no longer fails the
index: if a progress frame cannot be delivered, progress stops and the run's real
result is still written as the terminal. When the run ends the worker makes one
last bounded attempt to settle the frame; only if that also fails is the command
left unacknowledged, and its redelivery settles the spool from Main's handoff
watermark. In that last case the redelivery cannot know the run's result and
reports the run failed. A frame Main rejects (Stop, the deadline) ends progress
and stops the run at the next document boundary; the real terminal then takes
that frame's sequence. Any step still open when a run halts is closed with the
reason before the terminal.

**Who is affected.** Operators of deployments that run indexes on the Rust
worker. There is no migration and no setting.

**What to do.** Upgrade the worker with Main and the web app (Main must already
ingest `NodeEvent` frames for index runs). An older worker publishes no progress
and behaves as before; an older web app shows the document step under its first
name only.
