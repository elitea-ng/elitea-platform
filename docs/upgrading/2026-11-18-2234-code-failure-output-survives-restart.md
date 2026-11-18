# Pipelines: a failed Code node's output survives a worker restart (issue 2234)

**What changed.** The program output a failed pipeline **Code** node ends its run with (issue 2227) was
kept only in the worker's memory until the run's terminal frame was built, so a worker restart in between
left the error with the registered sentence alone. The failing node now also writes the bounded text
(at most 1,500 characters, control characters removed, as before) to a new table of the agent-state
database, `elitea_runtime.sandbox_run_failure_details`, one row per run and generation. The terminal path
reads it back when its in-memory copy is empty. No protocol field changes and the shape Main admits is
the same.

**Who is affected.** Operators. Agent-state migration 0016 creates the table; Main's migrate job applies
it before the worker rolls out (the chart orders them), like every agent-state migration.

**What to do.** Nothing beyond the normal upgrade order, Main first. A worker that starts before the
migration ran still works: it cannot write or read the row, logs `code_failure_detail_not_kept` once per
failed node, and the error carries the sentence alone, exactly as before this change. The rows are small
(at most 8 KiB) and live with the run's other sandbox rows; the same text is already visible to everyone
who can read the run, so the table adds no new exposure beyond the database's own access control.
