---
date: 2026-11-18
order: 3
---
## `run_code` shows the model why a program failed

When a program run by **Python sandbox** fails, the model now gets the program's own error output: the Rust compiler's diagnostic for code that does not compile, a JavaScript or TypeScript exception message, a Rust panic. Before, it was told only that the sandbox could not run the program, so it could not correct the code. The sandbox's supervisor keeps the last 1,500 characters of that output with the failed run (a new `failure_detail` column, applied by Main's agent-state migration 0015); the worker reads it from an additive field of the internal gRPC response, so the worker and the supervisor can be upgraded in either order. See [Python sandbox](/menus/internal-tools#python-sandbox-code-execution).
