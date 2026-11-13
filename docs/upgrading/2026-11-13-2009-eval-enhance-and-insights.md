# Agent evaluation: Enhance with AI and run insights (#2009, #2011)

Main has one new operation, `POST /api/v2/elitea_core/eval_run_enhance/prompt_lib/{project_id}/{run_id}`
(`enhanceEvalRun`), and the evaluation orchestrator now records the latency of each
case's agent turn. The web and desktop scorecard uses both, and reads
`GET /api/v2/elitea_core/eval_run_analytics/prompt_lib/{project_id}/{run_id}`, which
already existed.

- **Who is affected:** operators of Main and the web/desktop apps; client builders
  (additive change).
- **What to do:** upgrade Main before, or together with, the web and desktop
  apps; an app that calls the new route against an older Main gets 404. No
  migration, no new setting, no new permission.
- **Enhance:** one model call through the LLM gateway (the judge's completion path;
  `LLM_GATEWAY_URL` must be set, else 503). It needs the run create permission and
  the version read permission. It writes nothing; the web wizard saves each accepted
  fix through the existing version, dimension and dataset-case routes. The call
  carries no run attribution, so it is not counted in the run's own consumption.
  Errors: 409 run not finished, 422 nothing missed or an unusable model answer,
  502 gateway failure, 503 no gateway.
- **Latency:** `agent_latency_ms` (and `agent_timed_out` when the turn ended on a
  deadline) is stored in the result `evidence` of every case scored after the
  upgrade. Runs scored earlier have none and the UI says "not recorded".
