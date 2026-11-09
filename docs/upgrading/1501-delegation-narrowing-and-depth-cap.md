---
title: "Delegation narrowing and depth cap (#1501, on by default)"
issue: 1501
date: 2026-10-30
order: 53
---

ADR-0044 decision 3. No migration. When an agent calls a saved agent or
pipeline, a pipeline node runs one, or an agent starts a sub-agent, the worker
assembles the callee with the intersection of its own and its caller's
toolkits, tools, credential references, declared data classes and egress
hosts. A missing grant is a tool result with `DELEGATION_GRANT_MISSING`
(`kind` = `toolkit`, `tool`, `credential`, `data_class` or `egress_host`, and
`name`); a delegation past the maximum depth is `DELEGATION_DEPTH_EXCEEDED`.
Both are recorded on the `delegation` run step, which also carries
`delegation_depth` and `effective_grant_digest` (no change to the
`run_steps` columns).

- **Setting.** Configuration › Delegation (`delegation` section, gated on
  `configuration.governance`): `max_depth` 1..16 (default 4) and `mode` =
  `enforce` (default) or `report`. Governed runs always enforce. The depth is
  enforced in both modes.
- **Upgrade order: Main, then the workers** (the chart's default order:
  Argo CD sync wave 0 for Main, wave 1 for the workers). Main sends the
  policy on the new `WorkerCommandV1.delegation_policy` field (67), and a
  child's declared data classes on the private application-version envelope
  (`data_classes`), **only after an administrator saves Configuration ›
  Delegation**. A worker from an earlier release rejects both (unknown fields
  are refused at the wire layer and by the strict envelope decoder), so do
  not save the section until every worker runs this release. Until the
  section is saved, a new worker applies its compiled-in defaults: `enforce`,
  depth 4, and no data class narrowing. Narrowing of toolkits, tools,
  credentials and egress hosts, and the depth cap, are on from the moment a
  worker runs this release.
- **Behaviour change: existing compositions can be refused.** A composition
  deeper than 4 delegations (agent tiers and pipeline limits allowed more)
  is refused at the fifth level. A callee whose toolkits, explicitly selected
  tools, credential references or configured hosts its caller does not hold
  is refused. After every worker runs this release, look for `delegation`
  steps with `delegation_refusal`. To find the rest without refusing them,
  save the section with `mode` = `report`, look for steps with
  `delegation_enforced: false`, and grant the missing toolkit to the calling
  agent (or remove it from the callee). Then set `mode` back to `enforce`.
- **Data classes** come from `elitea_lifecycle.agent_registry.data_classes`.
  They are declared metadata, not classifier output. Data class narrowing
  starts when the section is saved. A root agent with no registry row or no
  data classes imposes no data class limit.
- An ad hoc conversation root (no saved agent) imposes no toolkit limit;
  narrowing starts at the first saved agent or pipeline.
