# Agents: cloud sub-agents show as temporary participants in the sub-agent lane (#1823, ADR-0050 decision 4)

- **What changed.**
  - The Rust worker streams each child a cloud run starts with `spawn_agent` as `agent_subagent` node events on
    the run's own output stream: its start (display name, avatar key, label), its own tool calls and results
    (short summaries, secrets masked), its text and its end.
  - Main folds the same entries onto the answer's `meta.subagents_v1`, in the transaction that accepts the
    frame, so a reloaded transcript shows the settled lanes.
  - Client contract **1.48** (`x-elitea-client-contract`): the `agent_subagent` client frame
    (`ClientFrameAgentSubAgent`) and `ConversationMessage.meta.subagents_v1` (`SubAgentLane`,
    `SubAgentLaneItem`). Both are additive.
  - The web, desktop and mobile apps show each child under its `spawn_agent` row with a **Temporary** badge.
    The desktop's own local sub-agent lanes now also show the spawn profile's name from `subagent_started`.
- **Who is affected.**
  - Projects whose cloud agents bind sub-agents (`spawn_agent`).
  - Answers written before the upgrade keep no lanes: only new runs show them after a reload.
- **What to do.**
  - Upgrade the workers and Main. No migration and no configuration change.
  - A worker before this release sends no `agent_subagent` frames, so nothing shows. Main before this release
    does not keep the lanes, so they show live and disappear on reload until Main is upgraded too.
