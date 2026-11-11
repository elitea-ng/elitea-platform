# Main and Workers: agent follow-ups — sub-agent depth record, idle compaction marker, workforce pass (#1921, #1872, #1876, #1877, ADR-0050)

- **What changed.**
  - **Sub-agent depth in the spawn record (#1872).** Shared migration 0239 adds `depth` and `parent_agent_id`
    to `elitea_runtime.ephemeral_agents`. Existing rows read as depth 1 with no parent. The runtime proto
    `EphemeralAgentV1` gains `depth` (field 32) and `parent_agent_id` (field 33); fields 17 to 31 stay reserved.
    The run-wide "16 sub-agents running at a time" cap is gone: it could never bind below the per-turn budget of 12
    (#1935).
  - **Idle compaction run marker (#1921).** Shared migration 0240 adds `elitea_runtime.execution_jobs.idle_compaction`
    (boolean, default false) and backfills it from `agent_idle_compactions`. Analytics read it, so an idle
    compaction's calls stay in the `idle_compaction` bucket after the source run's job is pruned. Historical figures
    for already-pruned sources are not recovered.
  - **Idle compaction credentials (#1920).** An idle compaction run binds the agent identity but no longer resolves
    toolkit credentials, and writes no definition-mismatch audit event. A revoked toolkit grant no longer drops the
    summary.
  - **Workforce policy pass (#1876, #1877).** The hourly pass reads a project's active agents, latest scorecards,
    legal holds and objections in one batch per project instead of per agent. While a project holds its maximum
    number of automatically hired agents on probation, the pass leaves open proposals untouched. An automatically
    hired agent is created through the same path as the create route, so it also carries the proposal's welcome
    message and conversation starters.
  - Main logs a failed compose of the agent proposals or agent memory stores (their routes answer 503) instead of
    failing silently (#1862), and logs an idle compaction source skipped for a missing frozen version (#1919).
- **Who is affected.**
  - Every operator: two shared migrations.
  - Operators who run Workers and Main at different versions during a rollout.
- **What to do.**
  - Upgrade order: run the migrations (0239, 0240) and roll out Main first, then the Rust Workers. A Worker upgraded
    first sends the new `EphemeralAgentV1` fields to a Main that treats them as unknown; that Main refuses the
    sub-agent report, so the run's sub-agent records are lost until Main catches up. The run itself is unaffected.
  - Main must not run before migration 0240: the idle compaction admission writes the new column.
  - No configuration change.
