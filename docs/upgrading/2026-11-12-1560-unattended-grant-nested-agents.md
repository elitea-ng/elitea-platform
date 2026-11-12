# Unattended grant reaches nested agents (issue 1560)

- **What changed.** A cloud unattended grant that names a sensitive tool now approves it for a nested agent too (an agent
  that the root agent calls as a tool), when the nested agent still holds the tool after delegation narrowing. Before,
  only the root agent used the grant, and the nested call paused. The ledger gets one `unattended_grant` row for the call.
  Pipelines that an agent calls do not use the grant yet.
- **Operator action.** None is needed. No migration is involved.
- **Rollback.** Deploy the previous release. A nested agent's sensitive call pauses for the person again.
