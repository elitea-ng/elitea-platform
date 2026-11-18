# Pipelines: a failed Code node's error carries the program's output (issues 2227, 2228)

**What changed.** When the program of a pipeline **Code** node fails (an uncaught exception, a Rust
panic, code that does not compile), the run's stored error is now the registered Code failure sentence
followed by `Program output:` and the last 1,500 characters the program wrote to its error stream, with
control characters removed. Before, the error was the sentence alone. The worker builds the text into
the failure frame it sends, and Main admits that one shape and nothing else after the sentence (a
bounded, clean text after the exact separator). The same text reaches the run's `error_summary` and the
`pipeline.run.failed` event. No protocol field is added.

With Rust compiled snapshots enabled, the compiler's diagnostic of a compile-stage failure is carried
the same way, for the `run_code` tool and for Code nodes.

**Who is affected.** Everyone who runs pipelines with a Code node. The error text is visible to every
member who can read the run, so a program that prints secrets to its error stream now shows them in the
error. What a program prints is not redacted here, as in the `run_code` tool's answer.

**What to do.** Upgrade Main before the worker (the chart does this in one rollout). A worker that
sends the new text to a Main that does not admit it has its terminal refused once and replaced by the
generic internal failure, so the run still ends, without the Code failure's own sentence. After a worker
restart in the middle of a failing run the error carries the sentence alone.
