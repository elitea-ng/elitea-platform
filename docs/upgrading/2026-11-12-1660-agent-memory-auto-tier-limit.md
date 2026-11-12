# Agent memory: write mode `auto` is limited by the agent's risk tier (issue 1660)

- **What changed.**
  - **A version save with `meta.memory.write` set to `auto` is refused for an agent above a configured risk tier.**
    The answer is status 422 with the code `memory_auto_above_tier`. It names the agent's tier and the limit.
  - **The limit is a new setting** in the Agent Lifecycle section (`memory_auto_max_tier`). The default is `medium`.
    An administrator with `configuration.governance` can change it to `low`, `medium`, `high` or `critical`.
  - **Only a save is checked.** An agent that already has `auto` keeps it. Its next save must use `auto_attended` or
    `review` if its tier is above the limit.
- **Operator action.** None is needed. To keep `auto` for high tier agents, set **Highest Tier for Automatic Agent
  Memory** to `high` or `critical`.
- **Rollback.** Set `memory_auto_max_tier` to `critical`. No migration is involved.
