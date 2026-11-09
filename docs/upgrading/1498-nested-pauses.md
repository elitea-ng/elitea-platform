---
title: "Nested pauses (#1498, on by default)"
issue: 1498
date: 2026-10-30
order: 54
---

ADR-0041 decision 5. No migration, no proto change, no setting. Worker only;
Main and the browser are unchanged.

- **Now supported.** A `hitl` node in a pipeline that an agent calls as a
  tool, at any agent tier, and in a saved pipeline that a pipeline `agent`
  node runs, resumes at the level that paused. Each level keeps its
  checkpoint, the resume runs one time, and the callers continue. Before,
  some of these pauses failed at resume.
- **Behaviour change: some attached pipelines are no longer offered.** The
  worker now skips, before any model call, an attached pipeline that can
  pause in a position that it cannot resume: a `hitl` node with more than 3
  delegations above it (the approval card names at most 3 tiers), or a
  `hitl` node in a pipeline tool beneath a pipeline root. Before, these
  pipelines were offered and failed when they paused. The agent answers
  without the pipeline, and the worker logs
  `an attached pipeline child could not be built and was skipped` with
  `cause = pipeline.child_pause_unsupported` and `cause_detail` = the pause
  kind (`pause_past_owner_path_tiers`, `hitl_gate_under_graph_root`,
  `sensitive_tool_approval`, `clarifying_questions`, `mcp_authorization`,
  `unscoped_agent_node`). After the upgrade, search the worker logs for this
  cause. To fix a skipped pipeline, move its `hitl` node to a higher level.
