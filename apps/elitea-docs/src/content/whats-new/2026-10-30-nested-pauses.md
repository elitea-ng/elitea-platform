---
date: 2026-10-30
order: 4
---
## Nested pauses

A HITL node can now pause a pipeline that an agent or another pipeline calls, with a maximum of 3 delegations above the node. Each pause shows one approval card with the path of its callers. After you approve or reject, the paused pipeline continues one time, and then each caller continues. When you reject, the caller reads the result of the **Reject** route. **An attached pipeline that can pause in a position that the worker cannot resume is no longer offered to the agent.** Before, such a pipeline failed when it paused. The worker log names the pipeline and the pause kind. See [HITL nodes in nested agents and pipelines](/how-tos/pipelines/nodes/interaction-nodes#hitl-nodes-in-nested-agents-and-pipelines).
