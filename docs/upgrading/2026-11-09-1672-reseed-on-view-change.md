# Agents: an agent's session is started again when its context view mode changes (#1672, ADR-0050 decision 9)

- **What changed.** The Rust worker now binds an agent's durable session to the context view mode
  (`context.history`) it was seeded under, and records that mode in the session's
  `elitea.history.reconciled.v1` state. On an agent's first fresh turn under another mode, the worker deletes the
  session (and the session's compaction summary) and seeds it again from the current view, in conversation order.
  A session seeded before this change has no recorded mode and is seeded again once. That includes sessions that
  hold side talk from before #1609.

  When the sub-agent tools are bound, a session holding messages without their `[message m-…]` ids is seeded again
  too (#1865). Resumes and continuations never trigger a new seed; one that reaches a session first leaves the seed
  to the next fresh turn.
- **Who is affected.** Every agent conversation that has a durable worker session, the first time each agent
  answers after the upgrade. After that, an agent is affected only when its mode changes, or when sub-agents are
  turned on for it. Each new seed costs one turn without the provider prompt cache (a full prompt, billed at the
  uncached rate). If the view is longer than the model's window, it also costs one compaction. When nothing changes,
  the prompt bytes are the same as before and the cache matches.
- **What to do.** Nothing. If you track model spend closely, expect one uncached turn per affected agent in each
  active conversation after the upgrade.
