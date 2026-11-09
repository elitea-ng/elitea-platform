---
title: "Throttles per agent and per user (#1498, off until authored)"
issue: 1498
date: 2026-10-30
order: 58
---

ADR-0041 decision 4. A `rate_limit` row in `gateway.governance_config` can
name `scope.agent_ids` (with exactly one `scope.project_ids` entry) and
`scope.user_ids`. No migration and no chart value changes: the row data is
JSONB, and the `type` value set does not change.

- **Upgrade order.** Upgrade the LLM gateway before you author a throttle. A
  gateway from an earlier release does not know the two fields and applies
  the row as a rate limit for the WHOLE project.
- **Database reads.** When a loaded rate limit names agents, the gateway reads
  `elitea_runtime.agent_execution_jobs` and `elitea_runtime.execution_jobs`
  (one primary-key read per run, cached for 10 minutes) on its existing
  database connection. If the gateway connects with its own database role,
  give that role `SELECT` on both tables. Without it, agent throttles fail
  open and `rate_limiter.degraded` on `GET /governance/status` grows.
- **Write validation.** `POST` and `PUT /admin/gateway/governance` refuse the
  fields on every type other than `rate_limit`, refuse more than 100 ids,
  and refuse an agent that is not in the named project or a user that does
  not exist.
- **Fail open.** Like the project rate limits, a throttle does not apply when
  the NATS counter is not available.
