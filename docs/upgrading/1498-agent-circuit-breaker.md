---
title: "Agent circuit breaker (#1498, on by default)"
issue: 1498
date: 2026-10-30
order: 59
---

ADR-0041 decision 2. Shared migration **0213** adds
`elitea_runtime.agent_breaker_runs` (the breaker's window of ended agent
runs, kept 7 days), the partial index `execution_jobs_agent_settled` on
`elitea_runtime.execution_jobs (settled_at)` for agent runs, the columns
`detail`, `resume_approved_by` and `resume_approved_at` on
`elitea_runtime.execution_suspensions`, and `circuit_breaker` (jsonb, default
`{}`) on `elitea_lifecycle.agent_registry`. The index is built in the
migration transaction, not `CONCURRENTLY`: on a large `execution_jobs` table
it blocks writes to that table while it builds. Run the migration in a quiet
period if the table is large.

- **Behaviour change: agents can be stopped automatically.** The routine
  `main.agent_breaker` runs every minute. By default it suspends an agent
  when more than 50 % of its latest 20 ended runs failed, or when one tool
  failed 5 times in a row in one run. A run that a run cap stopped
  (`CAP_REACHED`) is not a failure. Spend per hour and runs per hour are off.
  A stopped agent's new runs are refused with `execution_suspended`, and its
  runs in flight are cancelled. To keep the old behaviour, turn off
  **Configuration › Circuit Breaker › Circuit Breaker On**, or turn off the
  routine in **Admin › Schedules & Tasks**. The first pass reads only the
  runs that ended in the 10 minutes before it.
- **Resume of high and critical agents.** A breaker stop of a `high` tier
  agent needs a platform administrator; a project admin gets 403
  `resume_forbidden`. A `critical` tier stop needs two different platform
  administrators: `POST /api/v2/admin/execution_suspensions/{id}:resume` by
  the first answers **202** with the suspension still active, and the same
  administrator again gets 403 `second_person_required`. A client that
  treats every 2xx answer of a resume as "resumed" must check `active`.
- **Many agents at once.** When more agents than the storm limit (default 5)
  would trip on the error rate or tool failure rules in one pass, none is
  stopped and the `EliteaAgentBreakerStorm` alert fires.
- **Metrics without the audit export.** elitea-main now serves `/metrics` on
  `ELITEA_AUDIT_EXPORT_METRICS_ADDR` (chart default `:9464`) also when no
  audit exporter is configured, with the runtime safety metrics
  `elitea_safety_suspensions_active{scope,source}`,
  `elitea_breaker_trips_total{rule}`, `elitea_breaker_storms_total` and
  `elitea_breaker_pass_failures_total`. Set the variable to empty to close the
  port.
- **Chart.** New values `main.safetyAlerts.podMonitor` and
  `main.safetyAlerts.prometheusRule` (both on). Where the Prometheus Operator
  API exists, the chart renders a `PodMonitor` named `<release>-safety` (only
  when the audit export's PodMonitor is not rendered) and a `PrometheusRule`
  named `<release>-safety`. `EliteaPlatformExecutionSuspended` and
  `EliteaAgentBreakerStorm` are `critical`; route them to your on-call.
- **Evidence.** Each trip writes one `ai.elitea.security.v1.agent.breaker`
  event to the security audit outbox and notifies the agent's owners and the
  project admins (`agent_breaker_tripped`).
- **API.** `ExecutionSuspension` gains `breaker`, `resume_approved_by` and
  `resume_approved_at`. `AgentRegistryRecord` gains `circuit_breaker`;
  `updateAgentRegistry` accepts it and refuses a looser override with 422
  `circuit_breaker_override_loosens`. No operation was added.
