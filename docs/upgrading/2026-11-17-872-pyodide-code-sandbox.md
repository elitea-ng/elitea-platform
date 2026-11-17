# Python sandbox module: the code sandbox now serves it (pyodide, run_code)

**What changed.** The agent form's **Python sandbox** module (stored as `pyodide`) is no longer skipped by the
native worker. It binds a `run_code` tool that runs the program on the deployment's existing code sandbox, in
the languages of `worker.runtime.sandboxRuntimes`. `GET /elitea_core/runtime_capabilities` reports
`internal_tools.pyodide` true, and lists the languages in the new optional `code_sandbox_languages`, only when
elitea-main has `ELITEA_CODE_SANDBOX_LANGUAGES`. The chart sets it from `worker.runtime.sandboxRuntimes`.
`run_code` is declared `code_execution` in `tool-categories-v1.json`, so the code-execution approval rules
apply to it. No migration.

**Who is affected.** Operators whose install has `worker.runtime.sandboxRuntimes`: the toggle becomes
available. Operators with none see no change: the toggle stays disabled, and agents that saved the module still
run without the tool.

**What to do.** Upgrade Main, the worker and the web app together. Helm installs need nothing else. A deployment
that does not use the chart sets `ELITEA_CODE_SANDBOX_LANGUAGES` on elitea-main to the comma-separated languages
its sandbox serves (for example `python,javascript`), matching the worker's `sandbox_runtimes`. If your approval
policy for `code_execution` asks a human before a run, `run_code` now asks too.
