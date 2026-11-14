# Agent Evaluation: cases as agent runs (#2011 step B, #2013 step 2)

Main gains a second execution mode for evaluation runs, `runtime_plane`, and the
per-case controls that belong to it.

- **Who is affected:** operators of every deployment (one tenant migration);
  clients of the evaluation API; client builders (additive change, no new
  operation).
- **What to do:** run tenant migration `0246_eval_runtime_plane` before the new
  Main starts (every project schema: new `eval_suites` columns `execution_mode`,
  `step_limit_per_case`, `time_limit_per_case_seconds`, `store_tool_outputs`, and
  the new table `eval_case_runs`). Existing suites keep `predict_blocking`. Upgrade
  Main before, or together with, the web app. No new setting and no new
  permission; no workers need upgrading first.
- **Behaviour, the mode:** `eval_suites.execution_mode` and the optional
  `execution_mode` of `POST /eval_runs` (the start overrides the suite) choose
  `predict_blocking` (the default, one blocking LLM call per case, unchanged) or
  `runtime_plane`. A `runtime_plane` run needs `application_id`, and a run with a
  frozen agent (a release gate run) is refused with 422 and keeps using
  `predict_blocking`. A run's `execution_mode` and frozen `snapshot.limits` say
  what ran.
- **Behaviour, runtime_plane:** each case creates a conversation (source
  `evaluation`, visible in the author's chat list) and starts a turn of the run's
  pinned agent version through the chat start use case, as the run's author, with
  trigger origin `api` and no unattended grant. Admission, the kill switch,
  governed tools, credentials, guardrails and run evidence apply as for a chat. A
  step that needs a person pauses the turn: Main cancels it and the case stores an
  error naming the pause. A deployment without the agent runtime errors the run
  with a reason. The final answer is judged as before.
- **Behaviour, controls (runtime_plane only; refused on a `predict_blocking`
  suite):** `step_limit_per_case` (1 to 1000; model calls plus tool calls, checked
  while the run goes and again on the recorded steps), `time_limit_per_case_seconds`
  (1 to 3600; cancels the turn and stores a result flagged `agent_timed_out`; 15
  minutes when unset), `store_tool_outputs` (default true; keeps up to 1000
  characters of each tool step's output, first 60 steps, in the result evidence).
  A copy follows the run evidence rules: it is made only when the run's author
  holds `runs.evidence.read` in the project, the read is audited as the author
  first and refused when the audit write fails, the bytes must match the step's
  stored SHA-256 (outputs over 32 KiB are not copied), NUL bytes are removed and
  binary content is kept as base64. The scorecard (`GET /eval_results`) and the
  score a human records return the kept outputs only to callers who hold
  `runs.evidence.read` in the project; others get the result without them. **A
  copy follows the evaluation result's lifetime, not the run evidence's:** it is
  kept until the run or its dataset is deleted, even after the run evidence's own
  content retention has removed the original.
- **Behaviour, evidence and attribution:** each result row of a case carries
  `evidence.trajectory` (counts of steps, model calls, tool calls, tool errors,
  retries, per-tool figures and a bounded step list), `execution_id` and
  `conversation_uuid`. `eval_case_runs` records the execution of each case, and the
  gateway calls signed with that execution id count as the run's agent calls in
  its limits and in `GET /eval_run_analytics` (agent role, per case). The
  cost-estimate report across projects (`/analytics` evaluation estimate) still
  reads only the `eval:` ids, so it does not include agent runs.
- **Behaviour, limits during a case:** while a case runs, Main polls the usage
  attributed to the run and the case at the step poll's cadence and cancels the
  turn when a per-case token or cost limit, or a per-run limit with
  `on_run_limit=stop`, is crossed (the case stores an error naming the limit).
  Executions linked to the case's root run (run chain continuations) count with it;
  a delegated sub-agent runs inside its parent's execution and is already counted.
  The cross-project cost estimate now includes agent-run spend through
  `eval_case_runs`.
- **Behaviour, evidence layout:** a case's `evidence.trajectory` is stored once, on
  the first result row written for the case; the other rows carry
  `evidence.trajectory_ref` (the dimension id of that row). A case that recorded no
  steps has no trajectory. A result the table refuses is replaced by an error row
  with `evidence.store_failed`; when even that fails the run is errored with a
  count. A runtime_plane run with no recorded author is errored once, at start of
  its walk.
- **Behaviour, release gates:** a gate run whose suite asks for `runtime_plane`
  runs as `predict_blocking`, and `snapshot.execution_note` says so (the scorecard
  shows it). An explicit `execution_mode: runtime_plane` on a gate start is still
  refused with 422.
