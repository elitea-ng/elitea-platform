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
  `personal` (default, privacy-safe), `project`, or `both` (every tool then
  takes an optional `scope`, `personal` by default; a single-scope toolkit has
  no such argument). A *personal* memory belongs to one authenticated user and
  is invisible to every other user, through search, get and delete. A *project*
  memory is shared by everyone in the project (the SDK's behaviour).
* **Identity.** The personal owner is `VectorClaimTokenV1.actor_user_id`, new in
  this change: Main fills it from the execution's actor when it mints the
  claim's vector token. Main mints a token only for an actor that is a positive
  user id, so scheduled and system runs get no token and nothing in a tool
  argument or a toolkit setting can name a user. A token that states no user
  (an older Main) leaves personal memory unavailable with a sentence, project
  memory working.
* **Namespacing.** The vector namespace is a SHA-256 of a domain tag, the owner
  (project, or user id) and the length-prefixed toolkit `namespace`. The project
  comes from the token on the server side. So project, each user, each namespace
  string and each project are separate id spaces, and none can equal an
  `index_registry` id.
* **Personal memory stays per project.** The vector facade injects the token's
  project into every call and refuses another, and the claim holds a token for
  the run's project only; a personal memory that followed a user across projects
  would have to live in the user's personal project, which a run in another
  project has no token for. Per-project is the choice that keeps the isolation
  rule simple. (A user's personal project is therefore *not* their cross-project
  memory.)
* **Time.** Every memory is versioned. `manage_memory` on an existing key
  *supersedes*: the new version is written, then the previous one is closed
  (`valid_to` stamped), nothing is overwritten; saying the same content again is
  a no-op. `get_memory` and `search_memory` return current versions by default
  in the SDK's output shape; `as_of` (RFC 3339) returns what was valid then
  (a boundary instant belongs to the newer version); `include_history` returns
  every version with `version`, `valid_from`, `valid_to`. Search re-ranks by
  `0.9 * cosine + 0.1 * recency` (recency halves every 30 days of age from
  `valid_from` to now, or to `as_of`), so recency nudges and cosine decides.
  `delete_memory` removes **every** version: privacy beats history. At most 20
  versions are kept per key (the oldest is deleted by its own generation); the
  10,000-point namespace limit counts versions. Closing a version re-writes its
  point, and the store does not return vectors, so the old text is embedded
  again in the same batch as the new one.
* Limits: 16 KiB content, 8 KiB metadata, 256-byte key, search limit 50.

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

There is no user-deletion cleanup journal in elitea-main: the admin
`deleteUsers` handler and SCIM `DeleteUser` act directly, and the project
delete's `vector_data` step drops a whole project, which removes every memory of
that project (personal and shared) through `DropProject`. Removing one user's
personal memories needs their namespaces, and a namespace id is a hash:
`elitea-vector` cannot delete by owner, and `ListIndexes` does not say whose a
namespace is. A follow-up needs either a Main-side list of memory toolkits'
`namespace` strings per project (so Main can recompute `namespace_id(User(id),
ns)` for each project the user belonged to and delete it with its admin
credential) or an `owner` payload key the facade can delete by. Until then a
user's personal memories are removed by their own `delete_memory`, or with their
project.

## Open items

* The `pgvector_configuration` field is dropped from the memory form under the
  Rust indexing runtime (Main serves the type without it); the native filter on
  `metadata.t.*` payload paths is tested against the in-process fake only, not a
  live Qdrant.
* Automatic recall, consolidation, entity/relation graph and user-deletion
  cleanup are follow-ups as above.
