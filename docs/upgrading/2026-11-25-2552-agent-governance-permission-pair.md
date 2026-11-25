# Administration: agent governance gets its own permission pair (#2552)

**What changed.** Two new administration permissions, in the Agents and
governance area, narrow `configuration.governance` for agent governance:

- `agents.governance.view` reads the agent registry
  (`GET /api/v2/admin/agent_registry`), the delegations awaiting review
  (`GET /api/v2/admin/delegation_review`), the worker egress hosts
  (`GET /api/v2/admin/worker_egress/hosts`) and the fifteen agent governance
  Configuration sections: `agent_lifecycle`, `agents`, `ai_disclosure`,
  `approval_policy`, `approvals`, `browser`, `catalogue`, `credential_review`,
  `delegation`, `notifications`, `platform_preamble`, `proactivity`,
  `remote_agents`, `run_caps` and `worker_egress`.
- `agents.governance.manage` reads the same, saves those sections and runs the
  delegation review scan (`POST /api/v2/admin/delegation_review/scan`).

Every one of these routes still accepts `configuration.governance` (and, for
the Configuration sections, `runtime.plugins` with `configuration.governance`),
so its holders see no change. The guardrail and circuit breaker sections keep
their `llm.*` permissions: the pair does not open them.

The shared migration `0279_agent_governance_permissions.sql` grants both
names to the `super_admin` and `admin` administration roles. No other role
gets them: no built-in narrow role holds the pair, and the agent governance
role is left for a later release.

**Who is affected.** No one loses access. Both administrator roles gain the
pair; holders of `configuration.governance` see no change.

**What to do.** No operator action. To delegate agent governance without
`configuration.governance`, which ends on the sunset date, give a custom role
`agents.governance.view` or `agents.governance.manage`.
