---
title: "Approval policy by risk tier and tool category (#1498, off until saved)"
issue: 1498
date: 2026-10-30
order: 55
---

ADR-0041 decision 5. No migration. Every built-in tool has a category in
`libs/proto/contracts/tool-categories-v1.json` (`read`, `write`, `delete`,
`external_send`, `payment`, `code_execution`; a tool that is not listed is
`write`). A custom MCP server's pinned tools take the category the approver
gave them (`pinned_tools[].category`).

- **Setting.** Configuration › Approval Policy (`approval_policy` section,
  gated on `configuration.governance`): per tier, the categories whose tools
  need approval (default: `delete`, `external_send`, `payment` at `high` and
  `critical`, nothing at `low` and `medium`), and
  `require_hitl_tiers` (default none).
- **Upgrade order: Main, then the workers.** Main sends the policy on the new
  `WorkerCommandV1.approval_policy` field (68) **only after an administrator
  saves the section**. A worker from an earlier release refuses the unknown
  field, so do not save the section until every worker runs this release.
  Until it is saved, only the platform's sensitive action tools pause, as
  before. The defaults above apply from the save.
- **Tier.** The root agent's `risk_tier` from its registry record (no record
  is `low`). Callees use the root agent's tier.
- **Evidence.** A sensitive tool pause carries `approval_rule`
  (`{"source":"policy","risk_tier":"high","category":"delete"}`, or `source`
  `platform` / `builtin`). A pipeline pause writes it only for a policy rule,
  so an earlier worker still reads the pause; the chat frame shows `platform`
  when it is absent.
- **Behaviour change: pipeline starts can be refused.** With
  `require_hitl_tiers` set, a pipeline at one of those tiers whose path to an
  `external_send` or `payment` toolkit or MCP node has no `hitl` node does
  not start: HTTP 422 `PIPELINE_HITL_REQUIRED`, with the node, the tool and
  the path in the message. Pipelines are not publishable, so the check is
  at every start (chat, schedule, trigger).
