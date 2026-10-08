# Fan-out V2 runtime design (Parallel and Map)

Status: design for review, 2026-10-08. Wave 2 design (Track D1). Documentation only. All admission gates stay
`false` (`FIXED_PARALLEL_INTEGRATION_READY`, `parallel_compiler.rs:33`; `MAP_INTEGRATION_READY`,
`map_compiler.rs:31`; Web `CompilerAdmittedNodeTypes`). Nothing here is implemented.

Paths are relative to `services/elitea-worker-rust/src/` unless they start with `services/` or `libs/`. Anchors
were checked against this branch (from `origin/main`). Code that only exists on PR #1084 is marked "lands with #1084".

Binding inputs:
- Wire contract: `libs/proto/contracts/fanout-interrupt-decisions-v1.md` and
  `libs/jsonschema/runtime/v1/fanout-*.schema.json`. This document uses its names and does not restate its wire
  details. The schemas win over any text here.
- Decisions: Point 5 plan §1 (user decisions U3, U4, U5) and §6 (answers 1-5, UI correction).
- Supersedes the complete-decision-set sections of `parallel-pipeline-node-design.md` and the pausing parts of
  `map-reduce-pipeline-node-design.md` (marked in Track D4/D3).

## 1. Why V2

Two defects block any fan-out HITL. A third problem limits scale.

1. **Lineage loss on resume.** Child thread ids hash `execution_id` and `generation`
   (`state/postgres_checkpointer/parallel_children.rs:113,117`; `map_children.rs:110-111`). Every HITL continuation
   is a new execution. After a resume the parent derives different child threads: completed branches re-run and
   paused branches lose their checkpoint. In-memory unit tests cannot see this. Map hides it by refusing resume
   (`agents/graph/map_turn.rs:42`).
2. **Complete-set resume in three layers.** The Worker accepts only an exact full decision set
   (`parallel.rs:545` `prepare_resume`, `:1151` `decisions_for_activation`; test
   `parallel_tests.rs:1096`). Main requires `len(decisions) == len(interrupts)`
   (`services/elitea-main/internal/application/agentexecution/continue.go:409-432`) and the SQL repeats it
   (`internal/db/queries/agent_chat.sql:2145,2153,2368,2380`). Pending cards live only in message meta, so Main cannot
   consume one card once.
3. **Progress amplification.** One frame per streamed chunk, each a Main transaction plus ACK. A 64-item Map is
   about 19.6k Main transactions (performance expert §2). Parent saves are also wrapped per super-step
   (`parallel_checkpoint.rs:288`).

User decision U4 asks for **per-interrupt HITL**: answering one card resumes only its child; siblings keep running;
nothing waits for a complete set. The legacy async-supervisor design is not reproduced.

## 2. Architecture

```
Parallel planner (K fixed branches, any node) -+
Map planner (N items, one worker)  -----------+-> FanoutRunner (single-owner task)
  occurrence row: freeze  (parent row 1)
  children: Agent | LLM | Pipeline | Code (Wave 3), identity frozen at freeze
  loop: select{ child settled | decision tick (only while cards open) | cancel/deadline notify }
    child Interrupted     -> child receipt Paused{interrupt}; card frame -> Main row PENDING
    decision tick         -> claim-fenced fetch -> apply(child): prove Paused(interrupt) -> resume child
                             -> child checkpoint saved -> ACK -> CONSUMED
    nothing runnable, cards open, final fetch empty -> return interrupt{open cards, seen revision} (park)
  join: Parallel keyed array | Map ordered_collection / typed reducer (parent row 2)
```

### 2.1 One runner, two planners

Parallel (`parallel.rs:792` `drain_branches`) and Map (`map_reduce.rs:338` `execute_outcome`) are two copies of the
same scheduler: freeze, bounded `FuturesUnordered` (`parallel.rs:815`, `map_reduce.rs:419`), fail-after-drain,
ordered collect. V2 keeps one.

| Part | Owner | Notes |
|---|---|---|
| `FanoutPlan{children:[{key, worker, input, input_digest}]}` | planner | Parallel: one child per branch, key = branch id. Map: one child per item, key = decimal 0-based index. |
| Join | planner | Parallel: keyed array in declared YAML order. Map: `ordered_collection` or a typed reducer (Track B). |
| Occurrence, scheduling, cards, apply, park, failure | `FanoutRunner` | Single-owner task. Children are futures in one bounded `FuturesUnordered`. One core per activation. |

The runner has no polling sleeps. Today's `sleep(10 ms)` (`parallel.rs:840`) and 50 ms cancel poll
(`map_reduce.rs:811-815`) are replaced by one `select!` over:
- a child settling;
- the decision tick, armed **only while at least one card is open** (section 5.4);
- a cancel/deadline notify: `sleep_until(deadline)` plus a cancel `Notify`.

Serialization of payloads of 256 KiB or more moves to `spawn_blocking` in `PostgresCheckpointer` only if the Track C
numbers justify it. The runner itself stays on one task.

Planner limits: Parallel 16 branches. Map 64 items, at most 8 running. Fan-out nested inside a fan-out worker is
rejected at compile time (prevents child-permit deadlock and unbounded depth).

### 2.2 Worker families and order

Order: **Agent, then LLM, then Pipeline, then Code**. Code is last: it needs Gate 6 effect receipts and the
Code-slot semaphore (section 9), and it waits for #1084 plus Code acceptance (Wave 3).

All adapters implement one `FanoutWorker` trait, the generic form of `ParallelBranchGraphFactory`
(`parallel.rs:130-171` region). A pause of any kind surfaces as a child `GraphError::Interrupted`, which the child
receipt checkpointer already captures (`parallel.rs:494` `BranchReceipt::Paused`).

| Family | Input and context | Caps | Notes |
|---|---|---|---|
| Agent (Application) | Own child thread and session history. Seeded only from the mapped input. No parent `messages`. | Agent path tiers ≤ 3 including the member tier; model-selected parallel calls stay at ≤ 8 | Already wired in Parallel (`parallel_compiler.rs:195`). May hold several cards over time; card identity is `(child_thread, interrupt_id)`. |
| LLM | Child-owned message list. Compaction per child thread. | Prompt ≤ 512 KiB per item | Chosen: no tools in the first cut; tools follow the Application rules once 5b classifies them. |
| Pipeline | Compiled child graph with its own checkpoint frontier. | Inherits recursion limit; **no fan-out inside** (rejected at compile time) | Static pause and HITL nodes inside are allowed (section 5.1). |
| Code | Per-item sandbox attempt. | `max_concurrency ≤ min(8, Code slots)`; `Busy` is child-local backoff, never a failure | Wave 3. After Gate 6 receipts and the Code-slot semaphore. |

Map V1 admits only StateModifier and Application workers (`map_compiler.rs:272`). V2 widens this per family as each
adapter lands. The member tier counts toward `MAX_AGENT_PATH_TIERS = 3` (`agents/events.rs:108`), so the compiler
rejects a worker whose nested agent depth would exceed it.

## 3. Child identity and checkpoints

### 3.1 Frozen identity (Track C1 rule)

Child thread ids are fixed **once, at occurrence creation**, and stored in the occurrence. They never derive from
`execution_id`, generation or claim.
- Wave 1 (Track C1) freezes `thread_id` into `FrozenBranchInput` and the Map equivalent. V2 keeps the rule: the
  lineage module derives children from the stored occurrence (a durable nonce or the frozen ids; both are
  equivalent) and rejects execution-bound hashes.
- A continuation, a worker restart, a lease reclaim and a node-recovery generation bump all re-derive the same
  children from the stored occurrence. The writer row already survives takeover across executions, so fencing is
  unchanged.
- **Rewind and regenerate create a new occurrence**, hence new children and new `interrupt_key`s. Old keys become
  `SUPERSEDED` in Main in the same transaction as the regenerate.

### 3.2 Parent rows: at most two per activation

| Row | When | Content |
|---|---|---|
| 1 freeze | first entry | Frozen child list, member order, input digests. |
| 2 join | all children terminal | The joined output. |

Nothing else is written to the parent. Interrupts, decisions, resume proofs and progress live on the **child**
thread. The runner never calls a per-card `record_decisions` and never does a parent CAS per card.

**Rejected explicitly: the durability expert's D4 apply point** (a CAS append of `decisions[interrupt_key]` into the
parent occurrence). PLAN §1.2 resolves this conflict for the child checkpoint: a parent row per card costs
O(interrupts × parent size) bytes and exhausts the 4096-row / 64 MiB per-thread cap
(`state/postgres_checkpointer.rs:28-29`; performance expert §1).
Re-apply is a no-op once the child checkpoint moved on, so the parent needs no record.

### 3.3 Hierarchy

Each member adds one tier to `parent_agent_path` with
`call_id = "fo1_" + hex(SHA-256(… child_thread))` and a 1-based `sibling_ordinal` (contract §3.3). Frozen child
threads make the call id stable across restarts. The parser accepts ordinals 1..16 today
(`agents/events.rs:5725-5759`, bound `MAX_TOOL_CALLS_PER_MODEL_TURN = 16` at `:50`). Wave 2 widens it to 1..64 for
fan-out tiers only.

## 4. UI rule

Fan-out members render exactly like current EliteaUI sub-agent instances. No new visual container exists.
- The Worker stamps `parent_agent_name`, `parent_agent_call_id`, `parent_agent_path` and `fanout_v1` on every
  member-scoped frame. Labels come out of the existing breadcrumb contract: `Parent (1) ▸ Sub`.
- `fanout_v1` is grouping metadata only (counters, and the Map status buckets for more than 16 items: Waiting,
  Running, Failed, Completed; PLAN §6 answer 5). That grouping is Web work.
- HITL, HITL-node and auth cards appear in `AnswerApprovals` below the thinking view, bucketed by
  `parent_agent_call_id`, as in the current app. Coordinator (root) cards have an empty hierarchy and no accordion.
- The Worker adds no UI behaviour. Only performance, durability and resilience differ from the current app.

## 5. Per-interrupt HITL

### 5.1 Card kinds

| Kind | Raised by | Child proof anchor |
|---|---|---|
| `tool_guard` | sensitive tool in an Agent/LLM child | child checkpoint `Paused` with the interrupt id and tool call id |
| `hitl_node` | HITL node, or static pause, in a Pipeline child | as above; static pause adds its pause metadata |
| `ask_user` | `ask_user` tool | as above |
| `delegated_auth` | MCP authorization required | as above; see 5.6 |

**Static pause and HITL node.** Today the proof for a pipeline static pause relies on `parent/node` thread naming:
the catalog lookup strips `root_thread + "/"` from the thread id (`agents/graph/static_pause.rs:185-190`). Inside a
fan-out the child thread is a frozen opaque id, not a path. The proof moves to the fan-out child identity: the card
is bound to `(child_thread, child_paused_checkpoint_id, interrupt_id)` through `interrupt_key`, and the static
pause metadata is checked against the child's checkpoint (`StaticPauseMetadata::checkpoint_matches`,
`static_pause.rs:57`), not against the session tail or thread naming. The static "type to continue" resume becomes
the decision action `continue` with the text as `value` (contract §1). The root path still resolves text through
`PipelineTextContinuation` (`static_pause.rs:213`) until root pauses move.

Why not the session tail: graph resume binds decisions to the last event (`agents/graph/resume.rs:116-122` finds the
`INTERRUPT_METADATA_KEY` event with `rposition` and requires it to be the tail; `:483` takes `events.last()`). With concurrent children the last event is not an authority.

### 5.2 Main is the decision ledger

Main stores cards and decisions (contract §5). The Worker holds no decision state. The parent stores none. The
child checkpoint is the proof that a decision was applied.

### 5.3 The apply path

One path. Two triggers (section 5.4).

```
apply(decision D for card C):
  1. load child latest checkpoint
  2. proceed only if it is Paused at exactly C.child_paused_checkpoint_id with C.interrupt_id
       - child latest has moved past the pause      -> no resume; ACK applied with the current checkpoint id
       - card cannot apply (wrong pause, child
         completed, occurrence superseded)          -> ACK stale
       - child is running (multi-card Agent child)  -> hold D until the child pauses again or finishes
  3. re-invoke the child with resume controls (is_resume_key set, parallel.rs:1053)
  4. wait for the child's resumed checkpoint to be saved
  5. ACK applied with that child_checkpoint_id   (outcomes: applied | stale)
```

Properties:
- **Re-apply is a no-op.** Step 2 fails the proof once the checkpoint moved. Duplicate fetches, restore fetches and
  retries are safe.
- **ACK after save.** The ACK carries the saved child checkpoint id. Main marks `CONSUMED` only then.
- No parent write happens anywhere in the path.
- Effects: an approved effectful tool that crashes between dispatch and its result checkpoint can be re-dispatched.
  This window exists for root HITL today and fan-out does not widen it. Effectful workers stay closed until Gate 6
  (section 11).

### 5.4 Two triggers

| Trigger | When | Behaviour |
|---|---|---|
| **Live** | The execution holds a claim and at least one card is open. | Claim-fenced `POST …/interrupts/fetch` every 1 s, backing off to 5 s while fetches return nothing new, reset to 1 s when a card opens. Zero fetches while no card is open. |
| **Parked** | A decision arrives after the execution parked. | Main admits one `fanout_wake` continuation (payload `meta.fanout_wake_v1`, no decisions). The runner re-enters the pending node, restores, **fetches immediately**, and runs the same apply path. |

A restore always fetches once immediately, including in a continuation. Routes, headers, digests and canonical forms
are in the contract §7.

### 5.5 Park and wake

The runner parks **immediately** when nothing can run: no running child, admission closed or exhausted, at least one
open card, and one final fetch that returned nothing new. It returns an interrupt carrying the open card list and
`decision_revision_seen`; the terminal frame carries `fanout_parked_v1`. The Worker never holds a claim for human
latency (no grace timer until measurements justify one).

Exactly one continuation is guaranteed by three pieces in Main (contract §8):
1. the response-row lock taken by both the paused-finalize transaction and the decision transaction;
2. the per-response `decision_revision` compared with `decision_revision_seen`;
3. a partial unique index allowing one active continuation per response.

Whichever of park settlement or decision commits second admits the continuation. A decision that lands while a
continuation is queued or running is picked up by that continuation's restore fetch. Idempotency key:
`continue-interrupt/{responseMessageID}/{decision_revision}`.

### 5.6 Delegated auth

Delegated auth uses the same path. It is behind a **Wave 2 spike** that must prove live credential
re-materialization: an `authorize` decision (token-store `credential_ref`, never a token) lets the paused child's
next MCP call obtain the credential under the **existing claim**, without a new execution. Fallback if the spike
fails: auth cards are applied only at the park boundary (a `fanout_wake` continuation); only auth is affected. The
spike is OPEN.

## 6. Failure handling

| Event | Behaviour |
|---|---|
| Child fails or is blocked (default) | **Fail-after-drain.** Close admission, drain running children, fail the node. Open cards (`PENDING`/`DECIDED`) become `CANCELLED`. Paused children are never resumed for a doomed activation. |
| Map `on_item_failure: collect` (opt-in, user answer 3) | A failed item does not stop the others. The join holds a typed failed entry at the item's position (below). |
| Lease lost | A **control stop**, never a business failure. No failure output. Writer takeover fences zombie writes; the replacement claim re-derives the same children (Track C1). |
| Cancel | Running children stop. Paused children have nothing to stop. Main cancels open cards in the stop transaction. |
| Deadline | If only paused children remain: park (the continuation gets a fresh deadline). If any child is still running: cancel it and fail. |
| 429 / provider limit | Until 5b-LLM ships the activation fails. Afterwards a **child-level** retry honours `Retry-After` and never re-runs siblings. |

**Collect entry shape** (chosen; the inputs only gave an example). Minimal and safe: no item data, no raw error text.

```json
{ "index": 3, "status": "failed", "error": { "code": "graph.node_execution_failed", "message_safe": "The item failed." } }
```

- `index`: the 0-based item index. `status` is the constant `"failed"`. `error.code` is a code from the existing
  validated failure-code set (`parallel.rs:1346` `valid_failure_code`). `message_safe` is a fixed catalog string per
  code, at most 200 characters, never model output, paths or item values.
- Successful items keep their normal value. Failed items add at most 512 bytes each, counted inside the 8 MiB joined
  bound.
- `collect` is allowed only with `reduction: ordered_collection`. A typed reducer cannot consume a failed entry, so
  the compiler rejects the combination (chosen).
- If every item fails, the node fails (chosen; avoids an all-error array posing as success).
- Open cards of a failed item are `CANCELLED`; its siblings keep their cards.

## 7. Sequence

Two branches pause; the user answers branch 2 first; the worker dies; a replacement continues.

```
t0  W1 claims E1 (root R). The runner freezes the occurrence: children c1,c2,c3 with frozen thread ids (parent row 1).
t1  c1 -> Paused(K1 tool_guard), c2 -> Paused(K2 ask_user): child receipts saved; agent_interrupt_pending frames
    -> Main rows K1,K2 PENDING. c3 keeps running. The 1 s decision tick is armed (open cards > 0).
    agent_fanout_member: c1,c2 paused.
t2  The user answers K2. Main: K2 PENDING->DECIDED (CAS), decision_revision=1, agent_hitl_resolved frame. Not parked,
    so no continuation.
t3  Tick: fetch -> [K2]. apply: c2 latest = Paused(K2) at the card's checkpoint -> proof ok -> re-invoke c2 with resume
    controls. The parent is not written.
t4  W1 dies (c2 mid-run before its next save; c3 mid-run). The lease expires; the job is redelivered.
t5  W2 claims E1 (same execution, attempt+1). The root re-enters the pending node; freeze returns the stored
    occurrence -> same threads. Restore: c1 Paused(K1), c2 still Paused(K2), c3 from its last checkpoint.
    Immediate fetch -> [K2] -> c2 is still Paused(K2) -> re-apply (exactly once).
t6  c2 saves its resumed checkpoint -> ACK applied (child_checkpoint_id) -> K2 CONSUMED. c2, c3 complete.
    Only c1 is paused, nothing runs, the final fetch is empty -> park: fanout_parked_v1{open:[K1], seen:1}.
    Main settlement under the response lock: revision 1 == seen -> record parked; E1 settles.
t7  The user answers K1: revision=2, response parked -> Main admits fanout_wake E2 in the same transaction.
    E2 claims -> the runner re-enters -> restore fetch -> [K1] (K2 is CONSUMED, not returned) -> apply -> c1 completes
    -> ACK -> join (parent row 2) -> the graph continues.
```

## 8. Crash-window matrix

The apply point is the **child checkpoint**, not a parent append (this adjusts the durability expert's matrix).

| Window | Durable state | Recovery rule |
|---|---|---|
| Decision recorded, execution running, not delivered | Row `DECIDED` | The next live tick or the restore fetch delivers it. If the Worker died, the replacement claim fetches it. |
| Decision recorded, execution parked | Row `DECIDED` + continuation + outbox row in one transaction | The outbox re-offers it (PostgreSQL is the authority). A duplicate wake collapses on the idempotency key and the partial unique index. |
| Fetched, not applied (`after_fetch`) | `DECIDED`; child still `Paused(K)` | Refetch under the new claim; apply once. A late write from the old worker fails on the writer lock (`state/postgres_checkpointer.rs:944-980`). |
| Applied, not ACKed (`after_apply`, `before_ack`) | Child checkpoint moved past the pause; row `DECIDED` | Refetch finds `DECIDED`; child latest ≠ `Paused(K)` at the card's paused checkpoint → ACK `applied` with the **current** child checkpoint id. No second resume. |
| Delivered, resumed, crash before the child save | Child still `Paused(K)` | Re-apply once (the proof still holds). An approved **effectful** tool may re-dispatch: Gate 6 receipts (section 11). |
| Child `Completed` receipt saved, parent join not saved (`after_child_receipt`) | Child receipt | Replay from the receipt; no re-invoke (`parallel.rs:494` area). |
| All children done, join not saved (`before_join`) | Receipts | Re-enter the node at the same step; freeze returns the stored occurrence; the join is deterministic. |
| Card or terminal frame published, not acked | Encrypted output spool (`execution/output_delivery.rs`) | Replay the frame. The raise is idempotent on `(root_response_id, interrupt_key)` with byte equality. |
| Lease lost mid-child | Nothing new (writes fail closed) | Control stop (Track C1). The replacement restores everything. |
| Decision for an older pause, foreign response or wrong actor | — | `child_paused_checkpoint_id`/key mismatch → ACK `stale` → `SUPERSEDED`, or 409, or 404. |
| Duplicate or multi-tab submit | Row locked | Same `request_id` and bytes: replay 200. Otherwise 409 `agent_interrupt_already_resolved`. `agent_hitl_resolved` drops the card in other tabs. |
| Stop, regenerate, new turn | — | Same transaction: open rows → `CANCELLED`/`SUPERSEDED`. The dead fence blocks a late ACK. |

## 9. Budgets and capacity

### 9.1 Bounds

| Bound | Value |
|---|---|
| Parallel branches | 16 |
| Map items; running at once | 64; ≤ 8 |
| Open cards per response (`PENDING` + `DECIDED`) | 16 (`MAX_PAUSE_CARDS`, `parallel.rs:38`). The runner stops admitting work at 16. |
| One item or branch value | 512 KiB |
| Joined output | 8 MiB |
| Parent rows per activation | ≤ 2 |
| Transactions to prepare N children | 2 (one batched activation, one batched receipt read), independent of N |

### 9.2 Progress coalescing

Child text and tool deltas are buffered per child and flushed as **one aggregated frame per activation every 250 ms
or at 64 KiB**. Append-only deltas concatenate losslessly. Lifecycle frames are never coalesced and go out
immediately: `agent_interrupt_pending`, `agent_fanout_member` (start, end, paused, failed, cancelled),
`fanout_parked_v1`, failure frames. Target: ≤ 4 progress frames/s per activation. The coalescer lives behind the V2
flag; the single-agent path is unchanged. Decision: children stream, coalesced (PLAN §1.2).

### 9.3 Worker-wide limits

| Limit | Value | Notes |
|---|---|---|
| Child permits per worker | 64 (2 × delivery cap) | Acquired at child admission in ordinal order. **Released while a child is paused**, otherwise per-interrupt HITL starves siblings. Paused children per activation are bounded by N (16/64). |
| Streams per (worker, model) | 16 | New per-model semaphore. |
| Agentstate pool | separate `agentstate_max_connections`, default delivery cap + 16 | Today the pool is `delivery_max_concurrency` (32) and is shared with sessions and the sandbox ledger (`bootstrap.rs:182-186`, `agents/session.rs:967-968`). Fan-out multiplies demand. |
| Code slots (Wave 3) | semaphore sized to Supervisor capacity, acquired before submit | Admission rejects when `ceil(N / capacity) × item timeout` exceeds the remaining deadline. |

### 9.4 Rate limits

HTTP 429 maps to `model_gateway.rate_limited` with no `Retry-After` today (`transport/openai_compatible_facade.rs:1350-1355`). Until 5b-LLM, one 429 among siblings fails
the activation (fail-after-drain). After it, the facade surfaces `Retry-After` and the **child** retries with bounded
backoff.

## 10. Delete-list

Verified to exist on this branch.

| Delete | Where | Replaced by |
|---|---|---|
| `parallel_published_resume.rs` (256 lines) | `agents/graph/parallel_published_resume.rs` | Per-interrupt apply path (5.3) |
| Duplicate Map scheduler (runner loop) | `agents/graph/map_reduce.rs:338-497`, `:811-815` | `FanoutRunner` |
| `FrozenOccurrence.decisions`, `.resume_inputs`, `.cards`; `record_decisions` | `agents/graph/parallel_checkpoint.rs:34-39,121` (call site `parallel.rs:618`) | Child checkpoint proof; Main ledger |
| Map resume refusal | `agents/graph/map_turn.rs:42` (`\|\| resume`) | Map supports resume through the runner |
| Execution-bound child hashes | `state/postgres_checkpointer/parallel_children.rs:113,117`; `map_children.rs:110-111` | Frozen identity (3.1) |
| Aggregate interrupt schema `elitea.graph.parallel-interrupt.v1` | `agents/graph/parallel.rs:41` (`PARALLEL_INTERRUPT_SCHEMA`) | Per-card `agent_interrupt_pending` (Web treats legacy frames as reconcile-only) |
| Polling sleeps | `parallel.rs:840`, `map_reduce.rs:811-815` | `select!` with deadline and notify |
| Complete-set checks in Main | `continue.go:409-432`; `agent_chat.sql:2145,2153,2368,2380` | Removed in the same change that moves root pauses (contract §10) |

The test `partial_duplicate_stale_foreign_and_changed_input_decisions_run_no_child` (`parallel_tests.rs:1096`)
enforces the rejection that V2 removes. It is rewritten to the per-interrupt rules (stale, foreign and duplicate
cards still run no child).

## 11. Effect safety

Stay closed until Gate 6 receipts, as fan-out workers and as retryable nodes:
- LLM or Agent children with any tool not platform-classified read-only;
- MCP-backed tools;
- toolkit writes and database writes;
- HTTP until the Main executor is wired;
- Code (Wave 3, governed by the sandbox receipts of #1084).

The approval-replay window (section 8, row "Delivered, resumed, crash before the child save") is the only place
where a decision can be applied twice at the effect layer.

## 12. Wave 2 task list

Wave 2 starts after #1084 merges and Code acceptance closes. Sizes: S/M/L.

| # | Task | Files | Size | Depends |
|---|---|---|---|---|
| 1 | PG test: ADK re-enters a dynamically interrupted node at graph level (proves the park/wake model before adapters) | `state/postgres_checkpointer_tests/fanout_resume.rs` | S | Track C1 |
| 2 | Unified lineage module (frozen ids); remove execution-bound hashes | new `state/postgres_checkpointer/fanout_children.rs`; delete derivations in `parallel_children.rs`, `map_children.rs`; adapt `application_children.rs` | M | 1 |
| 3 | `FanoutRunner`, occurrence, plan types; Parallel and Map as planners; delete the duplicate scheduler, `parallel_published_resume.rs`, Map resume refusal | new `agents/graph/fanout/{runner,occurrence,plan}.rs`; shrink `parallel.rs`, `map_reduce.rs`, `map_reduce_checkpoint.rs` | L | 2 |
| 4 | Per-interrupt proof from the child checkpoint for all four kinds; static-pause proof off thread naming | `agents/graph/resume.rs`, `static_pause.rs`, `static_tool_pause.rs`, `hitl.rs` | M | 3 |
| 5 | Proto: fetch and ACK client, `fanout_wake` continuation kind; widen member-tier ordinal to 1..64 | `libs/proto/elitea/runtime/v1/{control,input}.proto`, `protocol/control`, `agents/request`, `agents/events.rs:5725` | M | contract |
| 6 | Worker decision client and live tick (1 s to 5 s, only while cards open); restore fetch; final fetch before park | new `execution/fanout_decisions.rs`, runner loop | M | 3, 5 |
| 7 | Main: ingest `agent_interrupt_pending`, park/decide race under the response lock, private routes, `agent_hitl_resolved` (Track M2 builds the ledger; this wires it) | `services/elitea-main/internal/…` | L | M2, 5 |
| 8 | Delegated-auth spike (live credential re-materialization under one claim); fallback wiring | spike notes + runner switch | S | 6 |
| 9 | Adapters: Agent, LLM, Pipeline (in this order) | `agents/graph/fanout/workers/*.rs`; `parallel_compiler.rs`, `map_compiler.rs:272` | M each | 3, 4 |
| 10 | Failure modes: fail-after-drain, `on_item_failure: collect`, lease-loss stop, deadline park | runner, `map_yaml.rs`, `map_compiler.rs` | M | 3 |
| 11 | Progress coalescer; child permits; per-model limiter; separate agentstate pool | `execution/native_agent_lifecycle.rs`, `agents/graph/node_events*.rs`, new `execution/fanout_admission.rs`, `config.rs`, `bootstrap.rs` | M | 3 |
| 12 | Root (non-fan-out) pauses onto the same ledger; remove complete-set checks | Main + `resume.rs` | L | 7 |
| 13 | Web wiring (Track W, PR-touched files) | `apps/elitea-web` | L | 7 |
| 14 | Gate flips, one at a time after deployed acceptance: `split_out`/`aggregate`, Parallel, Map V2 | the constants above, `CompilerAdmittedNodeTypes` | S | all |

Wave 3: Code workers inside fan-out (Code-slot semaphore, deadline admission, Gate 6); database actions.

## 13. Acceptance and budgets

**Unit (fake workers, 4-thread Tokio runtime, counting in-memory checkpointer):**
- Two cards, answer #2 first: only c2 resumes; c3 keeps running; c1 stays paused.
- Duplicate decision applies once. A stale or foreign card runs no child.
- A decision for a running multi-card Agent child is held until it re-pauses.
- Cancel with paused children: zero invocations. Fail-after-drain with a paused sibling.
- 16 open cards stop Map admission. Permit released on pause. Nested fan-out rejected at compile time.
- Parent appends ≤ 2 per activation, including 16 cards answered in reverse order.
- Zero fetches with no card open; ≤ 1 fetch/s while cards are open and fetches return new data (5 s at idle backoff).
- Coalescer: ≤ 4 progress frames/s, lifecycle frames immediate, deltas byte-exact after concatenation.
- Collect entries match the shape in section 6; a typed reducer with `collect` is rejected.

**PostgreSQL:**
- Lineage identical across a new execution id, claim attempt + 1 and a generation bump; rewind gets fresh children.
- Two children pausing within 5 ms keep both receipts. Writer takeover rejects a zombie child save.
- Restore of 64 children p95 ≤ 500 ms. Prepare of N children in 2 transactions.
- 32 activations × 8 children, 512 KiB parent: zero pool-acquire timeouts, pool wait p99 ≤ 50 ms.
- Two-claim race: exactly 1 apply. Decide vs park: exactly 1 continuation. 50 parallel POSTs from two tabs: exactly
  1 `DECIDED`.

**Process-loss seams** (one test each): `after_fetch`, `after_apply`, `before_ack`, `after_child_receipt`,
`before_join`. Each ends with one final answer, no duplicate cards, no orphan rows, and 0 model calls for completed
children.

**Latency:** decision POST p95 ≤ 150 ms; live pickup p95 ≤ 2 s (≤ 6 s worst case with backoff); parked decision to
continuation running p95 ≤ 5 s; another tab drops the card ≤ 3 s. Per-child overhead p99 ≤ 30 ms on PostgreSQL.

**Browser (TG-02/03/04, no response mocks, reload, recorded evidence):**
- 3 branches pause and are answered in reverse order across two tabs; siblings keep streaming while a card is open.
- Reload keeps the pending cards. A Worker kill at each seam gives one final answer.
- A 64-item Map shows a bounded status view, never a wall of cards. Members read `Parent (1) ▸ Sub`.

## 14. Risks

| Risk | Mitigation |
|---|---|
| ADK re-entry of a dynamically interrupted node at graph level is only proven at node level (`parallel_tests.rs`) | Task 1 proves it first. ASSUMPTION until then. |
| Approval-replay effect window | Effectful workers stay closed until Gate 6. |
| Delegated auth cannot apply live | Spike (task 8); fallback applies at the park boundary. OPEN. |
| Retained Agent re-pauses must keep the same `interrupt_id` | Explicit test. ASSUMPTION based on the retained-pause identity. |
| Continuation churn while parked | Revision coalescing; a grace timer only if measured. |
| Permit release on pause allows many paused children | Bounded by N per activation and the 16-card cap. |
| 64 MiB per-thread cap still reachable on long pipelines (no retention) | Out of scope; flagged to the durability owner. |
| Decision value bound (8 KiB) vs today's 256 KiB edit values | Contract §6 open point. |
| Merge conflicts with #1084 | Wave 2 starts after it merges. |

## 15. Performance, durability, resilience and security

Mechanisms marked (Wave 2) are new code with the proposed name and location; the others exist on `main`. All
fan-out limits live in one module, `src/agents/graph/fanout/limits.rs` (Wave 2), and the test
`fanout_limits_match_contract_schemas` asserts they equal the bounds in `libs/jsonschema/runtime/v1/fanout-*.schema.json`,
so the Worker and the wire contract cannot drift.

### Performance

Budgets (PLAN §4; measured in Wave 2 and recorded in the source mapping):

| Budget | Limit | Where it is enforced or measured |
|---|---|---|
| Parent rows per activation | ≤ 2 (freeze, join), independent of N and of card count | §3.2; counting checkpointer unit test |
| Transactions to prepare N children | 2 | §9.1; PostgreSQL test |
| Progress frames per activation | ≤ 4/s plus lifecycle frames | §9.2 coalescer |
| Per-child overhead | p99 ≤ 30 ms on PostgreSQL; p50 ≤ 1 ms in memory | §13 bench |
| Restore of 64 children | p95 ≤ 500 ms | §13 PostgreSQL test |
| Pool | zero acquire timeouts at 32 executions × 8 children; wait p99 ≤ 50 ms | §9.3; load test |
| Decision fetches | 0 while no card is open; ≤ 1/s otherwise, 5 s at idle backoff | §5.4 |
| HITL latency | POST p95 ≤ 150 ms; live pickup p95 ≤ 2 s (≤ 6 s worst); parked → running p95 ≤ 5 s; other tab ≤ 3 s | §13 |

No per-chunk or per-item Main transaction: progress is coalesced per activation, and decisions never write the parent.

| Requirement | Enforced by (code mechanism) | Proven by (test) |
|---|---|---|
| ≤ 2 parent rows per activation | `FanoutRunner` writes the parent only in `freeze()` and `join()`; no other method takes the parent checkpointer (Wave 2, `fanout/runner.rs`); `record_decisions` deleted | `parent_rows_bounded_with_16_reverse_answered_cards`: counting checkpointer, 16 cards answered in reverse order → exactly 2 parent appends (budget) |
| 2 transactions to prepare N children | batched `activate_children(&[ChildIdentity])` (`INSERT … SELECT unnest … ON CONFLICT`) + one `DISTINCT ON (thread_id)` receipt read (Wave 2, `state/postgres_checkpointer/fanout_children.rs`) | `prepare_64_children_uses_two_transactions` (PG, statement counter: 2 at N=1, 16, 64) |
| ≤ 4 progress frames/s per activation | `ProgressCoalescer` with `COALESCE_INTERVAL = 250 ms`, `COALESCE_MAX_BYTES = 64 KiB` (Wave 2, `execution/fanout_progress.rs`); lifecycle frames bypass it by type | `coalescer_caps_frames_and_keeps_deltas_exact` (8 streaming children, 10 s paused clock → ≤ 41 progress frames, concatenated deltas byte-equal) |
| No fetch while no card is open; 1 s → 5 s backoff | `DecisionTick` armed only when `open_cards > 0`; `DECISION_TICK_MIN = 1 s`, `DECISION_TICK_MAX = 5 s` (Wave 2, `fanout/runner.rs`) | `no_fetch_without_open_cards_and_backoff_to_5s` (paused clock, fetch counter: 0 with no cards; 1,2,4,5,5 s intervals) |
| Per-child overhead p99 ≤ 30 ms; restore 64 children p95 ≤ 500 ms; pool wait p99 ≤ 50 ms | the batching above; `spawn_blocking` for ≥ 256 KiB payloads only if Track C numbers require it | `fanout_load` (PG, ignored by default, `ELITEA_TEST_DATABASE_URL`): 32 activations × 8 children, asserts the three percentiles |

### Durability

Every crash window this design creates or touches, with its recovery rule, is in §8. The ones that must be proven on
real PostgreSQL with process replacement or second-worker takeover are: cross-execution restore with 0 re-invocations
of completed children; `after_fetch`, `after_apply`, `before_ack`, `after_child_receipt`, `before_join`; the two-claim
apply race (exactly 1 apply); decide vs park (exactly 1 continuation). The only window that can repeat an external
effect is the approval-replay window, and effectful workers stay closed until Gate 6 receipts (§11).

| Requirement | Enforced by (code mechanism) | Proven by (test) |
|---|---|---|
| Child identity survives continuation, reclaim, generation bump | frozen child thread ids stored in the occurrence (Track C1); `fanout_children.rs` derives nothing from `execution_id`/generation (Wave 2) | `lineage_identical_across_execution_claim_and_generation` (PG); `rewind_creates_fresh_children` (PG) |
| Decision applied at most once per pause | `apply_decision` proves `child latest == Paused(interrupt_id)` at `child_paused_checkpoint_id` before resuming; typed `ApplyOutcome::{Applied, AlreadyApplied, Stale, Held}` (Wave 2, `fanout/apply.rs`) | crash seams `after_fetch`, `after_apply`, `before_ack` (PG + `writer_at` later claim): exactly one resume, ACK replay accepted |
| Zombie writes fenced | `lock_current_writer` on every save (`state/postgres_checkpointer.rs:944`), `PostgresCheckpointError::WriterNotCurrent` (`:112`) | `two_claims_apply_same_key_exactly_once` (PG, second claim; stale claim gets `writer_not_current`) |
| Completed children never re-run | child `Completed` receipt short-circuits invocation (receipt read in `restore`, Wave 2) | `cross_execution_restore_zero_reinvocations` (PG: pause 2/4 under E1, decide under E2 → 0 model calls for completed children) |
| One continuation per parked response | Main: response row lock + `decision_revision` + partial unique index (contract §8, Track M2 / Wave 2) | `decide_vs_park_exactly_one_continuation` (Main PG barrier test) |
| Join after crash is deterministic | `freeze()` returns the stored occurrence at the same step; join reads receipts only | crash seams `after_child_receipt`, `before_join` (PG) |

### Resilience

- **Inputs and outputs:** 16 branches; 64 items; 512 KiB per item or branch; 8 MiB joined; 16 open cards per
  response; 8 KiB per decision; collect entries ≤ 512 bytes each.
- **Concurrency:** ≤ 8 running children per activation; 64 child permits per worker (released while paused); 16
  streams per (worker, model); no nested fan-out; agent path ≤ 3 tiers.
- **Time:** the original execution deadline bounds the activation (only paused children left → park, else fail);
  private fetch/ACK calls are one bounded attempt with no automatic network retry; the live tick backs off to 5 s.
- **Control:** cancel stops admission and drops running children; lease loss is a control stop with no failure
  output; regenerate supersedes cards in the same transaction.
- **Failures are typed** (`graph.*` codes, fixed catalog messages). Nothing is coerced: a stale or unprovable decision
  is ACKed `stale`, never applied to a different pause.

| Requirement | Enforced by (code mechanism) | Proven by (test) |
|---|---|---|
| 16 branches, 64 items, ≤ 8 running | `MAX_PARALLEL_BRANCHES = 16`, `MAX_MAP_ITEMS = 64` (today `map_reduce.rs:22`), `MAX_RUNNING_CHILDREN = 8` (today `map_reduce.rs:23`) in `fanout/limits.rs`, checked by the planners at compile/freeze before any child starts; `FanoutError::ResourceExhausted` | `planner_limits_at_bound_and_bound_plus_one` (16/17 branches, 64/65 items, 8/9 concurrency) |
| 512 KiB per child value, 8 MiB joined | `MAX_CHILD_VALUE_BYTES` (today `MAX_BRANCH_RESULT_BYTES`, `parallel.rs:36`), `MAX_JOINED_BYTES` (today `parallel.rs:37`, `map_reduce.rs:26`), checked before the state update | boundary tests at 512 KiB / +1 and 8 MiB / +1 → typed `resource_exhausted`, parent state unchanged |
| 16 open cards | `MAX_OPEN_CARDS = 16` (today `MAX_PAUSE_CARDS`, `parallel.rs:38`); admission closes at 16 | `map_admission_stops_at_16_open_cards` |
| 64 child permits, released while paused; 16 streams per model | `FanoutAdmission { child_permits: Semaphore(64), model_streams: per-model Semaphore(16) }` (Wave 2, `execution/fanout_admission.rs`); permit dropped on `Paused` | `paused_child_releases_permit`; `model_stream_cap_16` |
| No nested fan-out; ≤ 3 agent tiers | compiler rejects a fan-out inside a fan-out worker and a worker whose depth + member tier exceeds `MAX_AGENT_PATH_TIERS` (`agents/events.rs:108`) with `graph.fanout.invalid_worker` (Wave 2) | `nested_fanout_rejected_at_compile`; `member_tier_counts_toward_three_tiers` |
| Deadline, cancel, lease loss | `select!` over `sleep_until(deadline)` + cancel `Notify` (no polling); lease loss maps `WriterNotCurrent` to `FanoutStop::LeaseLost` (control stop, no failure output) | `cancel_latency_under_100ms` (4-thread runtime, barriers); `deadline_with_only_paused_children_parks`; `lease_revoke_mid_child_emits_no_failure` (`TestStateWriterLease::revoke`) |
| Collect entries safe and bounded | `CollectedFailure { index, status, error: { code, message_safe } }` built only from `valid_failure_code` (`parallel.rs:1346`) and a fixed catalog; ≤ 512 bytes | `collect_entry_contains_no_item_data` (event + state capture) |

### Security

- **Authority inside the effect transaction.** A decision is authorized by Main for the exact actor, project,
  conversation and response inside the decision transaction (contract §6). The Worker applies only decisions it
  fetched over the claim-fenced private route (mTLS, claim id, fence, rechecked under the job lock with the DB clock;
  contract §7). The Worker never trusts an identifier from YAML, the browser or the model: `interrupt_key` is
  re-derived from the frozen child identity and the child's own checkpoint, and a decision the checkpoint cannot prove
  is refused (ACK `stale`).
- **Consume-once and multi-tab.** CAS on the row revision, `request_id` replay with byte equality, 409 for everything
  else, ACK after the child checkpoint is saved, `agent_hitl_resolved` to other tabs (contract §5–§7).
- **Rejection is never permission.** `reject`, `block_with_comment` and auth `skip` are applied exactly as decided; a
  missing, stale or failed decision never defaults to approve.
- **Credentials by reference only.** `authorize` carries a token-store `credential_ref`. The token is resolved
  server-side for the original actor and never enters a decision body, frame, log, child checkpoint or resume control
  (§5.6 spike must preserve this).
- **No secrets or data in events and logs.** `fanout_v1` carries no item content; member names are worker display
  names; logs and spans carry counts, ordinals, codes and digests only, never prompts, item values, tool arguments,
  decision values or URLs. Card `display` is a closed schema without headers, tokens or checkpoint ids.
- **No arbitrary code.** Workers are compiled nodes; Code workers run only in the Code sandbox (Wave 3).
- **Supply chain.** The runner uses only crates already in the Worker (`tokio`, `futures`, `serde_json`, `sha2`). Any
  new dependency in Wave 2 must be justified in its source mapping and pass `cargo deny`/`cargo audit` with
  `--locked`.

| Requirement | Enforced by (code mechanism) | Proven by (test) |
|---|---|---|
| Decision authorized for the exact actor/project/response inside the decision transaction | Main decide transaction locks the response and card rows, then rechecks ownership, membership and RBAC before the CAS (Track M2 `Decide`) | `decide_foreign_actor_refused` and `decide_foreign_project_refused` (Main PG, negative authz: 403/404, row unchanged) |
| Worker applies only fenced, fetched decisions | private fetch/ACK require mTLS + `X-Elitea-Claim-Id` + `X-Elitea-Fence`, rechecked under the job lock with the DB clock; `NODE_RECOVERY` claims refused (Wave 2 Main route) | `fetch_with_stale_fence_refused`; `ack_from_old_claim_refused` (Main PG, second claim) |
| Identifiers from YAML/browser/model never authorize | Worker re-derives `interrupt_key` from the frozen child identity and its own checkpoint (`fanout/apply.rs`); a non-provable key → `ApplyOutcome::Stale` | `foreign_key_decision_runs_no_child` (unit) |
| Reject and auth Skip are never permission | `DirectHitlAction` is applied exactly (`agents/direct_hitl.rs:132-142`); missing/stale decision has no default action | `reject_and_skip_never_execute_tool` (unit, tool-call counter = 0) |
| No tokens, prompts, item data, tool args or values in logs/events | safe-field-only log types `FanoutActivationLog { node, activation_label, total, running, paused, failed }` and `FanoutChildLog { ordinal, status, code }` (Wave 2); card display built only from the closed `fanout-interrupt-card` schema type; `credential_ref` is an opaque newtype without `Display` | `fanout_logs_and_events_carry_no_sensitive_fields` (tracing capture + frame capture: no prompt/item/arg/value/token substrings, schema-valid frames only) |
| Effectful workers closed until Gate 6 | compiler admits only platform-classified read-only tools in fan-out workers (Wave 2, `parallel_compiler.rs`, `map_compiler.rs`) | `effectful_worker_rejected_at_compile` |
| Supply chain | no new crate; `cargo deny --all-features check advisories` and `cargo test --locked` on the implementing PR | CI + recorded audit output in its source mapping |

## 16. Rejected alternatives

- **Async supervisor, or parent parks children as durable tasks with a reconcile replay.** This is the legacy design
  and the source of concurrent pause loss.
- **NATS wake subject.** The WORKER account is denied runtime subjects, and core NATS is advisory and lossy.
- **Decisions in the continuation payload.** Couples to the complete set and gives two delivery paths.
- **A parent decision ledger** (durability expert D4, parent CAS per card). Two authorities; O(interrupts × parent
  size) bytes. See 3.2.
- **Holding the claim while waiting for a human.** Pins capacity for hours.
- **A parent row per card.** Exhausts the thread cap.
- **Execution-bound child hashes**, and path-only child threads (they reuse stale children after a rewind).
- **Piggybacking on the 10 s lease poll** (`ObserveDesiredState`): too slow. An optional field-3 hint may come later;
  PostgreSQL stays the authority.
- **Two runners** (one per node type): doubles the HITL work.
