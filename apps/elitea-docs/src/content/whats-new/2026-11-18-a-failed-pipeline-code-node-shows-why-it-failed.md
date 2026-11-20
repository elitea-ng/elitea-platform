---
date: 2026-11-18
order: 5
---
## A failed pipeline Code node shows why it failed

When the program of a pipeline **Code** node fails, the run's error now ends with `Program output:` and the last 1,500 characters the program wrote to its error stream: the exception message of a JavaScript or TypeScript node, a Rust panic, or the compiler's diagnostic for a Rust node that does not compile (with Rust compiled snapshots enabled too, and for the `run_code` tool). Before, the error was the sentence "The pipeline stopped at a Code node" alone, so the author had to guess. The same text is in the run's error summary and the `pipeline.run.failed` event. Upgrade Main before the worker. See [Python sandbox](/menus/internal-tools#python-sandbox-code-execution).
