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
- **Also additive:** `POST .../versions/prompt_lib/{project_id}/{application_id}` accepts
  `copy_tools_from_version_id` (a version of the same application), which copies the
  source's toolkit and tool bindings onto the new version in the same transaction,
  as `copy_skills_from_version_id` does for skills. `GET .../eval_runs/prompt_lib/{project_id}`
  accepts `before_run_id` and `scored_only`, so "the previous scored run" is one query.
- **Long instructions:** when an agent's instructions exceed 24000 characters, Enhance
  asks for no instruction patch and says why (`instruction_patch_unavailable`).
- **Timeouts:** an agent turn counts as timed out (`agent_timed_out`) on the caller's
  deadline, a gRPC deadline from the gateway hop, a gateway 504 or 408, or a network timeout.
