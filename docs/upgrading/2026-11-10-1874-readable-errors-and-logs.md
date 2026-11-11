# Logs: one readable fault line per failure, support references, JSON Worker logs (#1874, FU-299)

Errors are now written so a person can read them, and they are easy to find from what a user reports.

- **What changed.**
  - **Fault lines.** Every warning or error about a failure carries a plain sentence (`msg`), a stable `code`, an
    `action`, and a `cause` list made safe by type. Panics and unexpected errors also carry a bounded stack. The codes
    and what to do about each are listed in [`docs/errors/README.md`](../errors/README.md). The existing `err` field
    is unchanged.
  - **Support references.** Main gives every request a reference such as `sr-1a2b3c4d5e6f7a8b`. It is returned in the
    `X-Elitea-Support-Ref` response header and in every JSON error body, and it appears on that request's log lines.
    Inside an agent run, the reference is the run's `execution_id`. A client-supplied `X-Request-Id` is never used as
    the reference.
  - **Worker log format.** The Worker's stdout is now **JSON lines**, one object per event, with span fields
    flattened. Span timing lines are no longer on stdout: they go to OTLP, or to stderr when the Worker starts with
    `ELITEA_RUST_SPAN_TIMING=stderr`. Every panic writes one redacted line to stderr.
  - **Quieter outages.** During a reported database or NATS outage, Main's 12 runtime runners log their per-cycle
    failures at DEBUG; the outage itself is one WARN and one recovery INFO. Outside an outage, a failing runner logs
    `runner.cycle_failed` at ERROR at most once a minute, with a `suppressed` count. Some other background loops are
    still logged per cycle (FU-305).
- **Who is affected.** Anyone who parses Worker stdout as text (log pipelines, grep-based alerts), and anyone
  searching Main logs for a user's report.
- **What to do.**
  - Parse Worker stdout as JSON lines. Match on `code` or `event` rather than on message text.
  - When a user reports an error, ask for the support reference (shown with the error) and search the logs for it.
  - To capture span timing for performance work, set `ELITEA_RUST_SPAN_TIMING=stderr` on the Worker temporarily.
