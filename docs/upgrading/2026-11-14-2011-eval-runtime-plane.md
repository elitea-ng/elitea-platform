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
  characters of each tool step's output, first 60 steps, in the result evidence,
  reading the run evidence content as the run's author and recording each read).
- **Behaviour, evidence and attribution:** each result row of a case carries
  `evidence.trajectory` (counts of steps, model calls, tool calls, tool errors,
  retries, per-tool figures and a bounded step list), `execution_id` and
  `conversation_uuid`. `eval_case_runs` records the execution of each case, and the
  gateway calls signed with that execution id count as the run's agent calls in
  its limits and in `GET /eval_run_analytics` (agent role, per case). The
  cost-estimate report across projects (`/analytics` evaluation estimate) still
  reads only the `eval:` ids, so it does not include agent runs.
