# Agent Evaluation: human scores and run limits (#2008, #2013)

Main gains the human score surface of Agent Evaluation and the first step of
run limits (the evaluation suite).

- **Who is affected:** operators of every deployment (two migrations); clients of
  the evaluation API; client builders (additive change, six new operations).
- **What to do:** run the migrations before the new Main starts: tenant
  `0244_eval_suites_human_scores` (every project schema: `eval_suites`, the
  append-only `eval_human_scores`, and `eval_runs.limit_reached`) and shared
  `0245_evaluation_human_score_permissions` (grants
  `models.applications.evaluation.human_score.read` to admin, editor and viewer
  and `.create` to admin and editor, centrally and for projects that override
  their role permissions). Upgrade Main before, or together with, the web app: an
  older Main answers 404 to the new routes. No new setting.
- **Behaviour, human scores:** a dimension that allows only the `human` engine is
  no longer refused at run start (it used to be a 501). Its results are stored as
  `pending_human` with no score and the judge is not called for them.
  `POST /eval_human_scores/prompt_lib/{project}/{run}` appends a score with the
  reviewer and a note, turns the result into an `ok` result and recomputes the
  run's headline in one transaction; `GET` lists a run's scores. A score outside
  the dimension's frozen scale is a 400, a result the AI judge scored is a 409.
  The release gate waits (stays `pending`) while any result of its runs is
  `pending_human`, so a gate with a human dimension never decides before a person
  has scored every case. The scorecard no longer lists `human_scores` as
  unavailable.
- **Behaviour, run limits:** `eval_suites` holds token and cost limits per case
  and per run and `on_run_limit` (`stop` or `report`). Routes
  `/eval_suites/prompt_lib/{project}` (list, create) and
  `/eval_suite/prompt_lib/{project}/{suite}` (update, delete) are gated on
  `run.read` (list) and `run.create` (writes). `POST /eval_runs` takes an optional
  `suite_id`; the suite's limits are copied into the run's snapshot, so a later
  edit changes no started run. The orchestrator reads the usage the gateway
  attributed to the run (`gateway.llm_request_logs`, the same source as the run
  analytics) before each case and before each judge call. A reached run limit with
  `stop` ends the run as `errored` with the reason, keeping the scored cases; with
  `report` the run finishes and `limit_reached` (`tokens` or `cost`) is set. A case
  limit skips the remaining judge results of that case. A usage read that fails is
  logged and the limits are not enforced until it works. Limits are checked between
  calls, so a run can pass one by the call that crosses it, and a cost limit counts
  only calls the model price list prices. Steps, time and tool output limits are
  not part of this change.
