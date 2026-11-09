# Agents: sub-agent profiles, briefs and records (#1614, ADR-0050 decision 4/9d)

**No order is required.** A worker of this release sends
`RuntimeControlService.ReportEphemeralAgents` before each settlement of a run
that started sub-agents. A Main of an earlier release answers `Unimplemented`;
the worker logs `worker_ephemeral_agents_failed` and settles the run.

- Migration `shared/0232_ephemeral_agents.sql` adds
  `elitea_runtime.ephemeral_agents`, one row per spawn. No data, no
  permission.
- `spawn_agent` accepts `profile.display_name`, `profile.soul`,
  `profile.model` (`inherit` only) and `profile.avatar`, and a `context`
  brief. A call that used one of these keys was refused before, so no
  existing call changes.
- Main's control codec admits 256 KiB for `ReportEphemeralAgentsRequestV1`.
