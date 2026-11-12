# ADR-0050 decided follow-ups: workforce demote, memory marks, sub-agent lane storage and nesting, context-view deltas (#1873, #1900, #1923, #1872, #1893)

- **What changed.**
  - **Workforce demote (#1873, ADR-0050 decision 5b amended).** An approved demote proposal now publishes its
    narrowing directly, from the agent's **published release** (never the draft), in the approving transaction:
    no ADR-0040 change request is opened, and the approval is the review. The owner's unpublished draft edits
    are untouched; the same narrowing is applied to the draft too, so a later publish does not undo it. Each
    narrowed published version gets a new release (new digest, toolkit pins); the release head moves to the new
    release of the version it pointed at. Older published versions are narrowed and their wide releases
    withdrawn, so a rollback cannot re-widen the agent.
  - **Memory marks (#1900, ADR-0050 decision 9d).** Each turn that recalls agent memory now draws a random
    10-character tag (`recalled_memory_tag`, `AgentExecutionInputV1` field 78); the instructions show every
    recalled entry's mark as `[memory N·TAG, ...]` instead of `[memory N, ...]`. A `spawn_agent` brief must
    cite a memory entry as `N·TAG` with the turn's own tag; a bare `N` or a mark with another turn's tag is
    refused (`BRIEF_MEMORY_NOT_RECALLED`). Nobody can predict a turn's tag in advance, so a `[memory N, ...]`
    line copied into an agent's instructions or a skill body (never rewritten) cannot point a brief at a real
    entry with different content.
  - **Sub-agent lane storage (#1923, #1823 follow-up).** Each sub-agent lane is now its own row in the new
    tenant table `chat_message_subagent_lanes`, keyed by `(message_group_id, agent_id)`. An accepted
    `agent_subagent` frame updates one child's own row instead of re-encoding and rewriting the whole answer's
    `meta.subagents_v1` array, closing an O(n²)-per-turn write amplification (measured: 111 MB of WAL for a
    12-child, 48-item-per-lane turn with the old whole-array rewrite, 9.8 MB with one row per lane). Every read
    path (the chat page, the message-details route, the single-message route) folds a group's rows back into
    the same `metadata.subagents_v1` shape, so no client sees a difference from this alone.
  - **Sub-agent lane nesting (#1872 remaining half, client contract 1.50).** A depth-2 grandchild's lane now
    nests under its depth-1 parent's `children` instead of showing at the top level. `SubAgentLane` gains
    `depth`, `parent_agent_id` and `children`. `@elitea/client-core/stream` nests both the live fold
    (`foldSubAgentEntry`) and the stored read (`parseSubAgentLanes`), and settles a running grandchild with its
    parent. Web renders a nested grandchild one level under its parent's own lane card. Mobile has no tree view
    for its flat tool-step list: a grandchild's steps follow its parent's, under a label built from both
    (`"Owl · temporary › Hawk · temporary"`).
  - **Context-view storage (#1893, ADR-0050 decision 9 follow-up).** Each answer's "what the agent saw" view is
    now stored as a delta against the same reader's previous view in the conversation (message ids and reasons
    only, in an edit-script form against the base), in a new immutable chain table
    `chat_message_context_view_frames`; every 64th view is a full keyframe. A 2,000-message, 1,000-answer
    conversation measured about 56× smaller stored bytes than one full row per answer. The inspector reads
    exactly as before; a row written before this migration still carries its own full `entries` and reads
    unchanged.
  - Migrations: tenant **0169** (`chat_message_context_view_frames`), tenant **0170**
    (`chat_message_subagent_lanes`).
- **Who is affected.**
  - Any project with an open workforce demote proposal: approving one now publishes immediately instead of
    waiting on the next manual publish.
  - Agents with agent memory on: the `[memory N, ...]` mark an agent's author or a browsed example may have
    shown now reads `[memory N·TAG, ...]`; any external documentation or saved prompt that names a bare `N` to
    a sub-agent must be updated (the model is told the new form in its instructions).
  - Projects whose cloud agents run sub-agents at depth 2 (`subagents_max_depth` 2): their lanes now nest on
    web and mobile; nothing else changes for depth-1-only runs.
  - No change for conversations without multi-agent context views or sub-agent lanes.
- **What to do.**
  - Upgrade Main, the Rust worker and the clients together; apply the tenant migrations. No configuration
    change.
  - A worker before this release cannot send a mark tag or lane depth/parent, and sends neither; Main before
    this release does not read `recalled_memory_tag`, `depth` or `parent_agent_id`, and ignores lanes it
    cannot fold (an unfolded depth-2 lane shows at the top level there, as before this release).
