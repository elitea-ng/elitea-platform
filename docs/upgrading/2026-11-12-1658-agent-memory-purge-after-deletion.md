# Agent memory is purged after the retention period of a deleted agent (issue 1658, ADR-0042)

- **What changed.**
  - **The memory of a deleted agent is now deleted after the retention period.** Before, it stayed for ever. The
    `main.step_record_retention` pass (hourly) deletes the notes, their versions and the opt-outs of an agent deleted
    longer ago than the project's `step_record` retention maximum. It writes one `agent.memory.cleared` audit event for
    each agent, with the reason `retention_after_deletion`.
  - **A legal hold on the project or on the agent keeps the notes.** With the seeded policy (`step_record` keeps
    forever) nothing is deleted.
  - **New tenant migration 0171** adds the table `agent_memory_orphans`. It records when an agent lost its agent.
    Deleting an agent writes the row. The pass writes a row for an agent deleted before this release, when it first
    finds the notes without their agent. The retention period for such an agent starts at that time.
- **Operator action.** None is needed. The migration runs on start and is safe to run again.
- **Rollback.** Stop the `main.step_record_retention` routine on the Schedules page to keep the notes. The table is
  not read by older versions.
