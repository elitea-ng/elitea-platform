---
title: "Per-agent context view (#1609, ADR-0050 decision 9)"
issue: 1609
date: 2026-11-07
adr: ADR-0050
order: 93
---

No migration. Upgrade the workers (`elitea-worker-rust`) before Main. Main
now sends `AgentExecutionInputV1.context_view` (field 72) on every agent
turn. A worker from an earlier release refuses the unknown field, so every
turn fails until the workers are upgraded. A new worker refuses an input
whose view digest does not match its history.

- Each agent in a conversation now gets its own history: side talk between
  two other agents is no longer in it, and the version's new
  `meta.context` block (`history`: full, since_joined, addressed_only or
  none; `include_side_talk`: party or none) narrows it further. A version
  without the block sees what it saw before, minus foreign side talk.
- A participant's `entity_settings.context_history: since_joined` is
  "share history from: now". Only `since_joined` and `full` are accepted.
- The filter applies to what Main sends on each turn. An agent's durable
  session keeps what it already received: after a mode is narrowed, or a
  participant is switched to `since_joined`, the session can still hold
  messages the new mode would leave out. Start a new conversation where
  that matters.
- Saves with an invalid `meta.context` answer `422 context_block_invalid`.
