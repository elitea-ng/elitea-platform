# Per-participant worker sessions in multi-agent conversations (issue 1869)

- **What changed.**
  - **Each agent of a conversation with several agents has its own worker session.** The thread the platform freezes for
    the turn is now `<conversation uuid>:<participant id>` for such an agent. A single-agent conversation and an ad hoc
    chat keep the conversation uuid, so their existing sessions keep resolving.
  - **A regenerated answer keeps the thread its first answer ran in.** An answer from before the upgrade, which
    recorded none, regenerates on the conversation's thread.
  - **A start always names one agent.** A start with no participant is refused before any thread is chosen, so no
    multi-agent start falls back to the shared thread.
  - **A conversation that switches key starts a fresh session per agent on its next turn.** The turn's chat history is
    authoritative, so the session is rebuilt from the agent's view of the conversation. No session is moved or split.
    Paused and continued turns keep the thread they started with.
  - **Idle compaction waits only on the same agent's runs** when the source answered on its own session thread. A peer's
    live or settled turn no longer delays or skips the check. A source on the conversation's thread keeps the
    conversation-wide wait.
- **Operator action.** None is needed. No migration is involved. The first answer of each agent after the upgrade can miss
  the provider's prompt cache once.
- **Rollback.** Deploy the previous release. Agents of a multi-agent conversation return to one shared session per
  conversation, rebuilt from history on the next turn.
