---
title: "Run caps per risk tier (#1498, tool call default on)"
issue: 1498
date: 2026-10-30
order: 57
---

ADR-0041 decision 3. Shared migration **0212** adds
`elitea_runtime.execution_jobs.run_caps` (the effective caps of each agent
run, written when Main prepares its command) and `cancel_cap` (the Main cap
that cancelled a run: `max_wall_seconds` or `max_spend`), the table
`elitea_runtime.run_cap_overrides`, and the permission
`safety.override_cap` for the platform administrators (`super_admin`,
`admin`) and the project `admin` role, including projects whose role matrix
was saved. It also adds the index `idx_llm_request_logs_execution_time` on
`gateway.llm_request_logs (execution_id, occurred_at)` for the spend routine.
No backfill.

- **Lock during the upgrade.** The migration runner is transactional, so the
  index is not built `CONCURRENTLY`. While the index builds, writes to
  `gateway.llm_request_logs` wait (the gateway's request log writes are
  delayed, reads continue). On a large log table, run the migration in a
  quiet period, or create the index before the upgrade:
  `CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_llm_request_logs_execution_time ON gateway.llm_request_logs (execution_id, occurred_at) WHERE execution_id IS NOT NULL;`
  The migration then skips it.

- **Setting.** Configuration › Run Caps (`run_caps` section,
  `configuration.governance`) keeps **Maximum Run Time** and gains, for each
  tier (`low`, `medium`, `high`, `critical`), `<tier>_max_tool_calls`
  (default 200), `<tier>_max_model_turns`, `<tier>_max_spend` (USD, at most
  four decimals) and `<tier>_max_records_read` (default 0, no cap). The tier
  is the root agent's registry tier; no record is `low`.
- **Behaviour change: 200 tool calls.** A worker of this release stops a run
  after 200 toolkit or MCP tool calls, also when Main sends no caps. The run
  ends with `CAP_REACHED` (`cap` = `max_tool_calls`). If your agents need more,
  save a higher `<tier>_max_tool_calls` after the workers roll, or set 0 for
  no cap. Model turns, spend and records read are not capped until you save
  a value. An agent's `step_limit` still applies; with `max_model_turns` the
  lower one stops first.
- **Upgrade order: Main, then the workers** (Argo CD sync wave 0, then wave
  1). Main sends the caps on the new `WorkerCommandV1.run_caps` field (69)
  **only after an administrator saves a per-tier key**. A worker of an earlier
  release refuses the unknown field, so do not save the per-tier caps (and do
  not save the Run Caps form, which saves them all) until every worker runs
  this release. Saving only Maximum Run Time through the API, as before,
  sends nothing new.
- **Spend cap.** The routine `main.run_spend_cap` (Admin › Routines, every 30
  seconds) cancels a running agent run whose spend in
  `gateway.llm_request_logs` reached its `max_spend`, through the usual
  desired-state cancel. A run can pass the cap by the spend of one interval
  plus one lease renewal.
- **Records read.** Only `jira.search_using_jql`, `github.search_issues`,
  `sql.execute_sql`, `confluence.search_pages`, `confluence.search_by_title`
  and every index toolkit's `search_index` report records. Other tools count 0.
- **Author caps.** An agent version's `meta.run_caps` can lower the caps for
  that agent. Creating an agent or saving a version with a value above the
  tier cap now answers HTTP 422 `RUN_CAP_ABOVE_TIER` (or `RUN_CAP_INVALID`).
- **Override.** `POST /api/v2/elitea_core/run_cap_overrides/prompt_lib/{project_id}`
  (new operation `createRunCapOverride`, `safety.override_cap`) lets the next
  agent run of the conversation of a `CAP_REACHED` answer use twice that cap.
  Each grant writes an `ai.elitea.security.v1.agent.cap.override` event to
  the security audit outbox. A worker cap cannot be overridden before the
  per-tier caps are saved. One unused override is permitted for each
  conversation; an override that expired unused does not block the next
  one. A run whose command carries no caps does not use an override on a
  worker cap.
- **Evidence.** The metric `elitea.runtime.run_caps.reached` gains the label
  value `cap=max_spend`. The worker logs `agent_run_cap_reached` for a worker
  cap. Counters are per execution: a run that continues after an approval
  pause starts its counts at zero.
