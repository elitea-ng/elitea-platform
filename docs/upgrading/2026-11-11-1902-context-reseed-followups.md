# Worker and Main: an agent's session follows its whole context view and is rebound instead of re-seeded when it can be (#1902, #1903, #1905, ADR-0050)

- **What changed.**
  - Main now sends a binding of every setting a context view depends on (its mode, side talk, and the join time
    under `since_joined`) as `ContextViewV1.binding` (field 4). The Worker binds the agent's durable session to it,
    so a change of side talk or join time under the same mode is no longer missed (#1902).
  - A session bound to another view, or seeded before the view was recorded, is kept and only rebound when it
    provably holds what the new view holds: every history event in it is in the view, in order, with no gap before
    its recent turns. Its tool results, compaction summary and prompt cache stay. Otherwise it is started again from
    the view, as before (#1905). A re-seed now costs one session read, one delete and one create (#1904).
  - A regeneration of an answer that wrote its own question now names that question (`current_message_id`), so the
    regenerated session keeps its `[message m-…]` ids and the next turn no longer re-seeds it (#1903).
  - An answer that paused for an approval or a question and then went on records the view of the continued run, so
    "What the agent saw" shows what the model was actually given (#1892).
  - Pipelines: the view is stored once when the YAML never names `chat_history` (at the head of `messages`), resumes
    and recoveries no longer rebuild it, and `{chat_history}` in an `fstring` keeps the newest messages that fit the
    64 KB rendered-input limit instead of failing the node (#1913, #1914, #1916).
- **Who is affected.**
  - Operators who run Workers and Main at different versions during a rollout.
  - Pipeline authors who interpolate `{chat_history}` into a template: a long history is now cut to its newest
    messages there. Map `chat_history` as a variable to give the model all of it.
- **What to do.**
  - Roll out the Workers first, then Main. A Worker from an earlier release refuses a `ContextViewV1` with field 4
    set. (It ignores `current_message_id` on a regeneration, so that half is harmless either way.)
  - Nothing else: sessions are rechecked on their next fresh turn, and no migration runs.
