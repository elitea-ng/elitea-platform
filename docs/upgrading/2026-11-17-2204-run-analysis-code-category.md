# Data analysis `run_analysis_code` is now a `code_execution` tool

**What changed.** `run_analysis_code` (the Data analysis tool that runs a model-written program on the code
sandbox) was not listed in `tool-categories-v1.json`, so approval rules treated it as `write`. It is now declared
`code_execution`, like `run_code`, `pandas_analyze_data` and `execute_sql`. No migration.

**Who is affected.** Deployments whose approval policy gates `code_execution` (ask a human, or block) and whose
agents use Data analysis with a code sandbox: those calls are now held or blocked by that policy.

**What to do.** Upgrade Main and the worker together (the category contract is read by both). If you do not want
`run_analysis_code` held, adjust the `code_execution` rule instead.
