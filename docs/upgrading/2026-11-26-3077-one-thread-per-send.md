# Desktop, web and mobile: one thread per send, however often Enter is pressed (#3077)

**What changed.** Pressing Enter several times quickly in a desktop Local work
new thread made one thread, one task worktree and one agent run per press. Now
a message is sent once:

- Every composer (desktop Local work and cloud work, web chat, mobile) ignores
  a second submit while the first is in flight. The check is synchronous, so
  presses in the same tick are caught; the input stays open for typing.
- Each message carries a client id. The desktop host makes `worktree_create`
  idempotent on the new thread's UUID and `agent_turn_start` idempotent on the
  new `submit_id`, so even a double IPC call yields one worktree and one turn.
- `createConversation` takes an optional `client_request_id` (a UUID). A create
  whose id already made a conversation of the caller in the project answers
  that conversation instead of making a second one. The id is kept as
  `meta.client_request_id`.

**Who is affected.** Everyone who upgrades elitea-main and the clients. Older
clients send no id and behave as before.

**What to do.** Nothing. Tenant migration `0255` adds a partial index,
`chat_conversations_client_request_id_idx`, on `(author_id,
meta->>'client_request_id')`. It is created inside the migration transaction
and indexes only conversations made with an id, so it is small and quick to
build.
