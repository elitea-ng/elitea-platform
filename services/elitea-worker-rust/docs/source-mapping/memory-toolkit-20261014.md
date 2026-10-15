# The `memory` toolkit on Rust (worker unit M9), 2026-10-14

Status: personal and project scopes, time-versioned memories and
`adk-memory` / temporal-memory evaluations are implemented or decided here.
The port itself is in `SOURCE_PARITY.md` (the `memory` row). This page records
the design decisions, the evaluations and the follow-ups.

## What was built

* Four SDK tools (`manage_memory`, `search_memory`, `get_memory`,
  `delete_memory`), SDK names, schemas, descriptions and sentences, over
  `elitea-vector` (gRPC over mTLS, the claim's vector token). Embeddings go
  through the budgeted `/llm` route on the deployment's
  `memory_embedding_model` (`worker.runtime.memoryEmbeddingModel`). No new
  internal REST. Code: `libs/rust/vector-index/src/memory.rs`,
  `libs/rust/agent-runtime/src/toolkits/families/memory/mod.rs`,
  `services/elitea-worker-rust/src/indexing/tools.rs`.
* **Two kinds of memory** (owner decision). `memory_scope` on the toolkit:
  `personal` (default, privacy-safe) or `project`. A toolkit is one or the
  other (owner decision, review round 3): the former `both` mode and its
  per-call `scope` argument are removed, and an agent that needs both attaches
  two memory toolkits. A toolkit saved with `both` is read as `personal` (no
  data migration); Main no longer saves `both`, and the create form offers the
  two values. A *personal* memory belongs to one authenticated user and is
  invisible to every other user, through search, get and delete. A *project*
  memory is shared by everyone in the project (the SDK's behaviour).
* **Who may see a memory is decided by `elitea-vector`, not by the worker.** See
  "Security model" below. The worker only says which scope a call works in.
* **Personal memory stays per project.** The vector facade injects the token's
  project into every call and refuses another, and the claim holds a token for
  the run's project only; a personal memory that followed a user across projects
  would have to live in the user's personal project, which a run in another
  project has no token for. Per-project is the choice that keeps the isolation
  rule simple. (A user's personal project is therefore *not* their cross-project
  memory.)
* **Time.** Every memory is versioned. `manage_memory` on an existing key
  *supersedes*: the new version is written, then every older open version is
  closed in place (`valid_to` set; the vector is kept, nothing is embedded
  again); saying the same content again is a no-op. A version's id is a random
  UUID, so concurrent writers never share a point and none is lost: "current" is
  the open version with the latest `valid_from`, and the other racer stays in the
  history with its end derived from the next version. `get_memory` and
  `search_memory` return current versions by default in the SDK's output shape;
  `as_of` (RFC 3339) returns what was valid then (a boundary instant belongs to
  the newer version); `include_history` returns every version with
  `valid_from` and `valid_to` (together with `as_of`: every version that existed
  by then). Search re-ranks by `0.9 * cosine + 0.1 * recency` (recency halves
  every 30 days of age from `valid_from` to now, or to `as_of`), so recency
  nudges and cosine decides. `delete_memory` removes **every** version: privacy
  beats history. At most 20 versions are kept per key (the oldest is deleted by
  its own generation); the 10,000-point namespace limit counts versions.
* **Hot path.** A write is one read of the key's versions (`ReadMemory`, no
  vector), one embedding, one count scoped to the namespace and owner, one
  upsert and one in-place metadata update per version it closes. Nothing lists
  indexes or counts the project. A run remembers its embedding dimension, so a
  read that embeds nothing (`get_memory`) names its one collection.
* Limits: 16 KiB content, 8 KiB metadata, 256-byte key, search limit 50.

## Security model (review round 1)

The first version kept users apart only by hashing the user id into the
namespace the worker chose; `elitea-vector` checked the project and nothing
more, so a compromised or buggy worker, or anything that computed another
user's namespace id, could read or delete that user's memories. The service now
enforces it:

* **A distinct source.** Memory points have source `memory` (`SOURCE_MEMORY`),
  not `toolkit_index`. Main mints the claim token with the `memory` source
  beside `toolkit_index`; a callback token never has it.
* **The owner is stamped from the token.** On upsert the facade writes an
  `owner` payload key: `user:<id>` for the `personal` scope, from the token's
  principal (a worker-claim token only, and only a positive integer id), or the
  literal `project` for the `project` scope. No request field carries an owner,
  `owner` is not a filterable key (a filter naming it is refused), and the point
  id includes the owner, so two users writing the same key into one namespace
  never overwrite each other.
* **Every operation is narrowed by it.** Search, hybrid search, count, read,
  list-documents, delete and the in-place metadata update all add
  `owner == <the token's owner for the requested scope>` to the project scope.
  User B's token, even holding user A's namespace id, reads, counts, lists,
  updates and deletes nothing of A's (service tests against a live Qdrant).
* **No user, no personal memory.** A token whose principal is not `user:<positive
  id>` gets `PERMISSION_DENIED memory.personal_unavailable` for the personal
  scope; the project scope still works. In practice Main mints a worker claim
  token only for a run a person started or a schedule a person owns (the
  actor must be a positive user id), and scheduled runs act as their owner and
  use the owner's personal memory; a run that has no token has no vector access,
  so no memory at all.
* **Personal memory is decided at each personal-scope call (rounds 3-4).**
  elitea-vector tells Main what a request is about to do
  (`IntrospectTokenRequest.memory_intent`: personal read, personal write,
  project write, shared-index write). Toolkit-index reads and project-scope
  memory reads keep the claim-TTL cache. A personal write and every shared
  write (a project-memory write, a toolkit-index upsert or delete by a worker
  claim) are asked afresh. An allowed personal READ answer is reused by the
  same token for 3 seconds (`PERSONAL_READ_REUSE_SECONDS`), so a burst of
  searches in one turn costs one call; a conversation made shared or a
  participant added is therefore seen within 3 seconds. A long Upsert stream is
  decided afresh at its header and then at most every 10 seconds
  (`UPSERT_REVERIFY_INTERVAL`), so a revocation, a conversation made shared or a
  taint is seen within 10 seconds of a stream that is already writing.
  Main answers `personal_memory_allowed` only for a personal intent, and only
  if the run is a person's own chat turn: an `agent_execution_jobs` row and
  `trigger_origin = 'manual'` (start, regenerate and continue all record the
  caller's real origin, so a PAT/API caller is `api`; an MCP `tools/call`, a
  schedule, a webhook, a direct toolkit call and an index run are refused).
  The conversation is bound server-side: the stream the admission wrote is
  compared as a uuid (`conversation.uuid = $1::uuid`) AND that conversation
  holds the response message group whose `task_id` is this execution; it is a
  chat conversation (`source = 'elitea'`), private, and its only user
  participant is the actor. Anything else answers false
  (`memory.personal_shared_conversation`).
* **A conversation that read personal memory cannot write shared data (owner
  decision, round 4).** When Main allows a personal READ for an execution, it
  taints the execution's conversation
  (`elitea_runtime.memory_conversation_taints`, keyed by project and
  conversation uuid, shared/0166) before answering. While a conversation is
  tainted, EVERY execution bound to it (this turn, a regenerate, every later
  turn) is refused shared writes: project-memory upserts, deletes and metadata
  updates (`memory.project_write_after_personal_read`) and toolkit-index
  upserts and deletes (`memory.shared_write_after_personal_read`, for every
  claim token of the run, with or without the memory source), and Main's own
  `remove_index` from such a run (the claim remover answers
  `SHARED_WRITE_REFUSED`; round 5). Only an agent execution's token is
  conversation-bound (`conversation_bound` in the introspection answer), so an
  index ingest's or a direct toolkit call's index writes cost no extra
  introspection; a callback token's only source is its provider's, so it
  never writes a toolkit index. Reads are not affected. The taint is never
  cleared and is deleted with the conversation on every delete path by a
  tenant trigger (tenant/0149: the conversation route, the MCP run cleanup,
  pipeline trigger runs, bulk deletes) and with the project (foreign key). Setting and checking take one advisory lock keyed by
  the conversation, so a parallel read and write in one conversation are
  decided one after the other. An execution with no conversation is
  unaffected (it cannot read personal memory). A personal WRITE is not a read,
  but `manage_memory` reads the key's versions first, so in practice it taints
  too (conservative). A refused personal read and a request refused for a
  fixed reason (a toolkit the run was not given, a bad scope; checked before
  Main is asked) taint nothing.
* **Accepted residual risk.** The rule covers the data stores the platform
  shares (project memory, shared indexes). Other write-capable tools the user
  attached to the same agent (GitHub, Slack, email, an HTTP tool, ...) can still
  carry what was read out of the conversation, for example under prompt
  injection from a fetched page. The memory toolkit's help text in the create
  form says so: attach personal memory only to agents whose other tools are
  trusted with what the user tells it.
* **The run's own toolkits only (rounds 3-4).** Main computes the run's memory
  toolkit ids ONCE, at admission, from the server-frozen toolkit set: the
  top-level entries of the frozen version's `tools` (an application turn) or of
  the input's `tools` (an ad-hoc turn), which Main built from the project's own
  toolkit rows and which the worker materializes from; for a direct toolkit
  call, the resolved toolkit itself when it is a memory toolkit. A sub-agent's
  memory toolkits are the run's too (round 5): the admission follows the
  frozen `application` entries (application and version id, built by Main)
  recursively through each sub-agent's saved version in the run's project, the
  same projection the nested-agent route freezes the child from; a sub-agent
  that cannot be read fails the admission. They are stored
  on the execution (`execution_jobs.memory_toolkit_ids`); a frozen set that
  cannot be decoded fails the admission. Nothing a caller writes (tool
  arguments, user input, chat history, runtime-context entries, a nested
  settings object) contributes. The claim-token mint copies them in the same
  statement (no bundle decoding; never NULL) and adds the `memory` source only
  when there is one. elitea-vector refuses a memory request for any other
  toolkit id (`memory.toolkit_not_in_run`).
* **Rollout safety (round 3).** elitea-vector ignores a source value it cannot
  name and keeps the ones it can. The chart puts the whole vector stack in
  Argo CD sync-wave -5, before elitea-main (wave 0).
* **The store is the toolkit's.** The worker names the toolkit id (from the
  run's frozen toolkit reference, not from the model); the facade stamps it as
  the `toolkit_id` payload key, includes it in the point id, and narrows every
  operation by it. It is not a client filter key. The namespace id the worker
  sends is the hash of the toolkit id and the `namespace` label, so two
  toolkits never share a store even with the same label. This is a deliberate
  difference from the SDK, which shared memories by the namespace string: here
  `namespace` is only a label inside the toolkit (default `default`). A
  toolkit's memories are removed with the toolkit (`DeleteMemoryToolkit`), so a
  rename or a changed label strands nothing.
* **Memory is not an index.** `ListIndexes` never lists memory namespaces and
  refuses `source = memory`.
* **Administrators.** The administrator identity has no memory scope; it has
  `DeleteMemoryToolkit` (every point of one toolkit of a project, every
  namespace and owner: a toolkit delete) and `DeleteMemoryUser` (every personal
  point of one user in one project, or with project 0 in every project). `DropProject` still removes everything of a project.
  `DeleteMemoryToolkit` is deliberately NOT a cross-project delete: toolkit ids
  are a `SERIAL` per tenant schema, so toolkit 41 of project A and toolkit 41
  of project B are different toolkits, and a delete keyed on the id alone
  would erase another project's memories. A run's points always carry the
  run's resource project, which is the toolkit's own project (the freeze reads
  toolkits from the run's project; a catalogue version carries no toolkits),
  and the toolkit binding above makes that structural, so `(project, toolkit)`
  is the toolkit's whole store (round 3 review item 7, decided with the
  owner).
* **The namespace id holds no secret.** It is a hash of the toolkit id and the
  `namespace` label; elitea-main needs neither (it deletes by toolkit id).
* **`ReadMemory` is deterministic.** The facade reads every matching point of
  each collection in pages (refusing, `RESOURCE_EXHAUSTED`, past 5000 matched),
  sorts them newest first by `valid_from`, then by the larger version id
  (`metadata.t.id`, the tie-break the client uses to pick the current version),
  then by the point id, across all collections, and only then keeps `limit`,
  so versions beyond a page, or spread over two embedding spaces, still come
  back newest first and in the client's order.

### Prompt injection and cross-scope copying

Anything the model writes into a memory is later read back as a fact. A model
that moves content from personal to project memory shares one person's words
with everyone in the project, and a model that saves text taken from a web
page, a document or a tool result into project memory lets that text steer
every later conversation of the project. Round 3 closes the first path on the
SERVER: a toolkit is personal or project (no `both`, no per-call scope), and a
run that has read personal memory is refused every project-memory write, even
through a second (project) toolkit attached to the same agent. The second path
(untrusted content into project memory) is a mitigation, not a guarantee:
project toolkits should be attached to agents whose inputs are trusted.

### Lifecycle

* **Deleting a memory toolkit** deletes every memory point stamped with its id
  (project memories and every user's personal ones, every namespace) through the
  existing toolkit cleanup journal (`centry.toolkit_deletions`, row-first:
  recorded in the delete's transaction, drained by the reconciler with the
  journal's backoff and permanent-failure notes). Recorded only where memories
  live in the vector store (the Rust indexing runtime).
* **A user leaving a project, or being deleted,** has their personal memories
  deleted through the SAME journal (app `memory_user`; the `toolkit_id` column
  holds the user id; project 0 means every project). The row is written in the
  transaction that removes the user or the membership, by one helper
  (`Journal.EnqueueMemoryUsers`), and the reconciler retries it until
  `elitea-vector` has done it; a vector service that is down only delays it.
  The drain re-checks, right before the delete, that the user is still not a
  member of the project (for a project-0 row: that no user row with the id
  exists again); a user re-added before the drain closes the row as
  `superseded` and keeps their memories (round 3). The three hooks share one
  `cleanupjournal.MemoryUserJournal` interface and one typed-nil guard.
  Hooked: the project members route (`DELETE users`), the administrators' user
  delete (project 0: every project, no membership lookup), and SCIM group sync
  (a withdrawal that leaves the person with no role in the project, including
  rebinding and group delete). Not hooked: SCIM deactivation (it suspends,
  nothing is removed). No migration is needed: the existing journal table
  already carries the rows, so this PR adds none (the next free shared number
  is still 0166).
* **No data migration from the Python store.** Memories written under the
  Python worker stay in the toolkit's pgvector database; nothing reads or
  copies them.

## Evaluation: `adk-memory = "=2.2.0"` as the seam (decision: NO-GO)

Read from the 2.2.0 crate source (not compiled here; the crate is not in the
local registry cache).

* Fit with our dependencies: it needs only `adk-core 2.2.0` (the version in our
  lock; no second `adk-core`), `async-trait`, `chrono`, `serde`, `serde_json`,
  `tracing`. Default features are empty. Licence Apache-2.0. `cargo deny` and
  `cargo audit` are not installed on this machine, so no advisory check was run.
* Why not: `MemoryService`'s entry is `{content, author, timestamp}`. It has no
  key, no metadata, no read-by-key and no delete-by-key. The SDK tools need all
  four (`get_memory(key)`, `delete_memory(key)`, `manage_memory(key, metadata)`).
  `delete_entries(query)` removes entries *matching a query*, which for a
  semantic backend is a fuzzy match and could erase memories the user did not
  name. Its project scoping returns *global entries plus the project's* for a
  project search, a union, where we need strict separation of personal and
  project. Building the tools on it would mean encoding the key into the content
  or author and re-implementing exactly what `memory.rs` does, with an extra
  crate for no used function.
* Where it could help later: `adk_core::Memory` (already in `adk-core`) is the
  trait the vendored runner accepts (`memory_service` on the builder). A thin
  `adk_core::Memory` over `MemoryTools` would be the seam for automatic recall
  without adding `adk-memory`.
* `delete_user` (GDPR) has no platform hook to call; see "Deleting a user".

## Evaluation: automatic recall in the runner (decision: follow-up)

The vendored runner only *exposes* `ctx.memory()` to tools; nothing in
`adk-agent` searches it and injects the result before a turn. Automatic recall
would be new code: an instruction-injection step with a token budget, a framing
that marks recalled text as data and not as instructions, an agent-level toggle
(default off) in Main's agent version schema and the web form, and one
embedding call per turn charged to the project. That is more than a small safe
change, and the platform already has Main's personal-memory recall
(`ResolveCurrentMemoryRecall`, gate 7b) for the "remember me" behaviour. Left as
a follow-up with the scope rules above (personal = the actor, project =
shared), budgeted embeddings and bounded injection.

## Validation: "cognitive temporal memory" (the `graph-memory` feature)

`adk-memory`'s `graph.rs` (SQLite) and `neo4j.rs` give an agent:

* an **episodic log** (`kg_episodic`): each turn is recorded cheaply, fact
  extraction is left to an out-of-band consolidation pass and never runs on the
  hot path (the crate ships the ingest, not the extractor);
* **bi-temporal facts and relations**: `valid_from` and an optional `valid_to`;
  superseding stamps `valid_to` instead of deleting, so "current" is
  `valid_to IS NULL` and history is kept; queries never surface stale facts;
* in Neo4j, **`FOLLOWS` adjacency** between consecutive entries, used to pull in
  the neighbours of a hit; episodic recall in SQLite is token overlap, not
  vectors;
* `delete_user` (drop the user's graph and log) for erasure.

What fits and was taken: the supersede-by-stamping model, "as of" recall,
current-by-default reads and a recency component, on our own stack (one point per
version in `elitea-vector`, validity in the payload, filters on
`metadata.t.valid_from` / `metadata.t.valid_to`), with strict per-owner scoping.
Conflicting statements about one key are handled by supersession; deletion is a
hard delete of all versions (the graph's own `delete_user` agrees: privacy wins).

What was not taken, and why:

* **Entities, relations and `FOLLOWS` adjacency**: a graph needs a graph store
  (SQLite and Neo4j are not usable server-side here), and a flat key/value tool
  surface does not ask the model for entities. A follow-up if recall quality
  needs it.
* **Episodic log and consolidation**: see the design below.

Costs and risks of the full cognitive model: an LLM extraction budget (one or
more model calls per consolidated conversation, charged to the project);
*hallucinated or injected facts* (an extractor that reads untrusted content, for
example a web page or a document an agent fetched, can write a "fact" a person
never said, and a later turn then trusts it); the *privacy* of an episodic log
(it is a verbatim transcript store, so retention, per-user isolation and
deletion apply to every turn rather than to chosen facts); and *erasure* (a fact
derived from a turn must be erased with the turn).

Fit with the scopes: it fits both. Personal consolidation reads only the actor's
own turns and writes personal memory; project consolidation would write shared
memory, which makes provenance and review matter more (anyone's turn could seed
a fact everyone reads), so it should be opt-in and reviewable.

## Consolidation (episodic to facts): design, not implemented

Not implemented in this change: it needs an LLM extraction budget, an opt-in
setting, and a rule for untrusted input, none of which is small. Design:

1. Opt-in per toolkit (`memory_consolidation: off | personal | project`, default
   `off`) and a per-day extraction budget in the project's model budget.
2. Out of band: a scheduler job after a conversation settles reads only the
   *user's own messages* of that conversation, never tool results or fetched
   content, and asks the project's configured model for candidate facts as
   `{key, content}` pairs.
3. Candidates are written through the same supersede path (so history is kept),
   marked `metadata.m.source = "consolidated"` with the conversation id, bounded
   to N facts per run, and personal-scope only unless a person confirmed.
4. Deleting a conversation or a user deletes what was consolidated from it
   (the `source` metadata is the handle).

## Deleting a user

See "Lifecycle" above: a durable journal row (`memory_user`) per user, written
in the removal's transaction and retried by the reconciler.

## Open items

* The `pgvector_configuration` field is dropped from the memory form under the
  Rust indexing runtime (Main serves the type without it); the native filter on
  `metadata.t.*` payload paths is tested against the in-process fake only, not a
  live Qdrant.
* Automatic recall, consolidation and the entity/relation graph are follow-ups
  as above.
