# Worker: a root saved-Agent node resumes after a Worker loss (FU-323a)

- **What changed.**
  - A pipeline whose root Agent node runs a saved Agent now continues where it stopped when its Worker is lost
    (a crash, or a planned stop that outlasts the 30 s drain), instead of ending with the Agent-step
    `RECOVERY_FRONTIER_UNSUPPORTED` text (#1933):
    - in the Agent's model call: the stored request is sent once more and the answer lands once (the model gateway
      sees that call twice: a cost, not a repeated effect, as for an ordinary agent);
    - in a read-only tool: the tool runs once more;
    - in a call to another saved Agent: that Agent resumes from its own step;
    - after the Agent answered: the answer is served without a model call.
  - A tool that may have changed something (an MCP or toolkit tool that is not read-only, a Code tool, a saved
    pipeline used as a tool) is never repeated: the run ends with the registered `RECOVERY_REFUSED` text ("may have
    started, not repeated"). A saved-pipeline tool resumes in a later change (FU-323b).
  - A state the Worker cannot prove ends with the Agent-step `RECOVERY_FRONTIER_UNSUPPORTED` text, as before, with
    one WARN operator line per cause: `pipeline.recovery.agent_receipt_mismatch`, `pipeline.recovery.agent_step_paused`,
    `pipeline.recovery.agent_step_invalid`, `pipeline.recovery.agent_step_foreign_execution`,
    `pipeline.recovery.agent_step_session_without_checkpoint`, `pipeline.recovery.agent_definition_changed`,
    `pipeline.recovery.agent_definition_unproven`, and
    `pipeline.recovery.agent_step_may_have_started`
    for the `RECOVERY_REFUSED` end. Lines carry ids and `support_ref` only.
  - **Every frontier kind:** a reclaim whose checkpoint read hits a transient PostgreSQL failure (connection loss,
    timeout, serialization failure, deadlock, lock not available) no longer ends the run with `DependencyUnavailable`.
    The delivery is kept and reclaimed again (about once a minute) until the run's deadline, then it ends
    `DeadlineExceeded`. Its retained line carries result code `pipeline.recovery.inspect_transient`.
  - For that result code only, `agent_delivery_retained_no_ack` is written at most once a minute however many
    executions are affected; when the minute runs out one `agent_delivery_retained_summary` line reports how many were
    kept quiet (`suppressed`), how many executions (`executions`) and up to 8 of their `sample_support_refs`. Every
    other retained result code is unchanged: one line per retained delivery. The same retain applies to a direct
    agent's reclaim, not only a pipeline's.
  - If the saved Agent's version was edited while the run was stopped (a draft version can be edited in place), the
    reclaim refuses to continue it: the Agent-step `RECOVERY_FRONTIER_UNSUPPORTED` text, operator code
    `pipeline.recovery.agent_definition_changed`, and the edited Agent does not run. A call started by an older Worker
    carries no definition stamp, so its version cannot be proven: refused the same way, operator code
    `pipeline.recovery.agent_definition_unproven`. A step whose answer was already stored is served either way.
- **Who is affected.**
  - **Stock Helm: nothing yet.** Both recovery flags are off by default (`agentModelCheckpointRecovery: false`,
    `agentNodeRecovery: false`), so these runs still end typed after a Worker loss. They resume once PLAN-REMAINING
    0.7a flips the defaults (scheduled right after FU-323), or when an operator turns `agentNodeRecovery` on (it also
    turns model-step resume on).
  - **Compose deployments** (`agent_node_recovery: true` in `deploy/runtime/worker-runtime.rust.json`) benefit now.
  - Operators alerting on `agent_delivery_retained_no_ack` lines with result code `pipeline.recovery.inspect_transient`:
    use the `agent_delivery_retained_summary` counts too.
- **What to do.**
  - Nothing to migrate: no new durable record, schema or Main change.
  - During the rollout an older Worker may reclaim such a run; it ends it typed as before (#1933) and repeats nothing.
  - Point alerts for these runs at the new operator codes above, and at `pipeline.recovery.inspect_transient` for
    PostgreSQL trouble during reclaims.
