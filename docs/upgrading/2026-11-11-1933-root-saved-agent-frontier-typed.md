# Worker and Main: a root saved-Agent node lost mid-run ends with a readable failure (FU-291 D8)

- **What changed.**
  - A pipeline whose root Agent node runs a saved Agent cannot yet resume after the Worker is lost inside that node
    (FU-323 adds the resume). The reclaim used to end with the generic internal failure. It now ends with the
    registered `RECOVERY_FRONTIER_UNSUPPORTED` code and this text: "This run stopped during an Agent step that cannot
    continue after a restart, so nothing after that step ran and nothing was repeated; check what the Agent
    completed, then start the run again."
  - The Worker logs one WARN line, code `pipeline.recovery.saved_agent_frontier`, event
    `pipeline_recovery_frontier_refused`, with `execution_id`, `generation`, `node_id` and `support_ref` (the
    execution id). It carries no task text, arguments or results.
  - New registry entries: `runtime.recovery_frontier_agent_step` (user) and `pipeline.recovery.saved_agent_frontier`
    (operator).
  - Nothing else changes: the same runs stop at the same point, and nothing runs that did not run before.
- **Who is affected.**
  - Operators who alert or count on the internal failure code for these runs, or on
    `RECOVERY_FRONTIER_UNSUPPORTED`.
  - Operators who run Workers and Main at different versions during a rollout.
- **What to do.**
  - Roll out Main first, then the Workers. A Main without this change does not know the new text and refuses it
    once; the Worker then sends the generic `RECOVERY_FRONTIER_UNSUPPORTED` text instead ("This run stopped at a
    point where it cannot be safely continued. …"), so the run still ends readable and typed.
  - Point alerts for lost root saved-Agent runs at `pipeline.recovery.saved_agent_frontier`.
