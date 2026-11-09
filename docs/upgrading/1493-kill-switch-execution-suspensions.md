---
title: "Kill switch: execution suspensions (#1493, ADR-0041 decision 1)"
issue: 1493
date: 2026-10-28
adr: ADR-0041
order: 40
---

* **Migration 0194** adds `elitea_runtime.execution_suspensions` and the
  column `elitea_runtime.agent_execution_jobs.application_id`. The migration
  Job applies it on upgrade. Agent runs admitted before 0194 have no
  `application_id`, so an agent-scope suspension does not cancel them; a
  project or platform suspension does.
* **New permission `safety.suspend`.** Granted to the administration roles
  `super_admin` and `admin` (every scope) and to the project role `admin`,
  including saved per-project `admin` roles (agent and project scope of that
  project). Remove it from a role if those people must not stop execution.
* **New NATS subject `elitea.rt.v1.safety.suspension`** (core NATS, ids only)
  for the `elitea-main-runtime` identity, publish and subscribe. The chart
  grants it. A deployment that keeps its own NATS authorization must add the
  subject to that identity, or each replica sees a change only after its
  10-second reload.
* **New refusal code `execution_suspended` (HTTP 409)** on every run start and
  continue path, and as the code of a refused tool call in the worker. A
  replica that cannot read the suspensions at start-up refuses runs with HTTP
  503 `suspension_state_unavailable` until it can. Clients that treat every
  409 on a run start as "already running" must check the `code`.
* **New routine `main.kill_switch_selftest`** (daily, on by default, paused
  during maintenance) and the Prometheus gauge
  `elitea_kill_switch_selftest_last_success_timestamp`. Change it in
  **Admin › Schedules & Tasks**.
* **What to do:** nothing to enable. Read the
  [emergency stop runbook](../../apps/elitea-docs/src/content/docs/deployment/emergency-stop-runbook.mdx),
  decide who keeps `safety.suspend`, and add an alert on the gauge.
