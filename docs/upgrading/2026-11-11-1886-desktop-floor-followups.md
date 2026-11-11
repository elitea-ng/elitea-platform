# Desktop: several agents in a thread, follow-up fixes (ADR-0050 decision 7f)

Fixes to how a desktop **Local work** thread with several agents routes a message and runs its agents one after
another (#1886, #1887, #1888, #1889, #1908, #1909, #1926, #1927, #1928, #1929, #1931, #1932).

- **What changed.**
  - **The right participant answers.** `agent_turn_start` takes `participant_id`, the agent participant meant to
    answer. The host answers on it, not on another participant of the same agent version (#1927).
  - **Unpinned agents run.** An agent participant with no `version_id` (an unpinned member of a saved team) runs its
    agent's default version, as the lead and as a later agent of the message (#1926).
  - **Main routes before it recalls.** `startLocalTurn` routes the message by the floor inside the start's
    transaction, before it recalls memories, so a start Main refuses no longer reads memories first (#1888).
  - **A failed save stops the message.** When the commit of an agent's turn panics, the host sends
    `floor_stopped` `commit_failed` (and ends an unattended chain as failed) instead of leaving the remaining agents
    waiting (#1886).
  - **A later agent's `done` names the person's message.** Its `message_ids[0]` is the message's `question_id`, not a
    question Main never wrote (#1887).
  - **A cut cloud answer says so.** A cloud agent's answer cut at 512 KiB ends with a note (#1889).
  - **Briefs name chat messages.** `subagent_started.brief_refs.message_ids` are the quoted messages' chat message
    uuids, not the short `m-…` ids; the short id rule is shared with the worker (#1908, #1909).
  - **The composer.** The app keeps a thread's agents and lead for 30 seconds and reads them again after an add; a
    retry's own refusal is handled like the first (at most twice); a cloud lead is named above the message box
    with a local agent to address instead (#1928, #1929, #1931). The web and desktop share one routing rule
    (client-core `firstAnswerer`, #1932).
- **Who is affected.** People using desktop threads with several agents. Single-agent threads behave as before.
- **What to do.** Nothing. Older desktop builds that send no `participant_id` still work: the host answers on the
  first participant of the agent version, as before.
