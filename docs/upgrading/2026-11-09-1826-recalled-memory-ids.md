# Agents: recalled memory entries carry their ids into the run (#1826, ADR-0050 decision 9d)

The notes Main recalls into an agent's instructions at the start of a turn now
show their id (`[memory 42, fact] ...`), and the run input carries each
recalled entry's id and stored text in the new `AgentExecutionInputV1` field
`recalled_memory` (76). A `spawn_agent` brief may then name an auto-recalled
note in `context.memory_entries`, not only one the agent's `recall` tool
returned.

- **Who is affected:** operators upgrading Main and the Rust worker.
- **What to do:**
  - **Upgrade workers before Main.** A worker from an earlier release refuses
    a run input that carries field 76, so a turn of an agent with agent memory
    fails until its worker is upgraded. A new worker reads inputs from an
    older Main as before.
  - Nothing else: no migration, no setting.
- **Behaviour:**
  - The recalled notes block is a little longer per note (the id). The same
    note renders the same text on every turn, so prompt caching still matches.
  - A note about a person is recalled, and can be passed to a sub-agent, only
    while that person is in the conversation.
