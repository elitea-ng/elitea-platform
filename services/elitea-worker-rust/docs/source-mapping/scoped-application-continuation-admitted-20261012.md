# Scoped application continuation always on (2026-10-12)

Point 5 Wave 2a, PR 2a-1. Branch `feat/pipeline-scoped-continuation-admitted`, first based on `origin/main` `c79fe73d2`, then merged with
`origin/main` `dd0bbc170` and later (merge `0924b621f`). Line numbers below are for the branch head unless marked
"on `main`"; those refer to `c79fe73d2`. Brief: `handoffs/point5-wave1-20261008/T-2A-fanout-runtime.md` §2.5, §4 "2a-1".

User rule (PLAN §8.1, 2026-10-12): no feature flags for obvious functionality; a switch is deleted, not
defaulted; there is no merged-but-disabled state. This PR deletes `SCOPED_APPLICATION_CONTINUATION_READY`
(`agents/pipeline.rs:75-76` on `main`) and every parameter it fed. It adds no flag, build argument,
environment variable, cargo feature, Helm value or compose setting. Scoped application continuation is how
every graph conversation this Worker starts or continues runs.

## Business behaviour

| Topic | Current platform (reference only) | New platform after this change |
|---|---|---|
| A pipeline `agent` node calling a saved Agent that pauses (sensitive tool, `ask_user`, delegated authorization) | The SDK supervisor parks the run, and a replay re-executes the node. | The call owns an **original-call receipt** in the graph checkpoint (`started`, `paused`, `completed`). A resume continues that call; the original start and result events are re-sent, never recomputed. |
| An Agent inside a saved child pipeline (pipeline → `agent` node → pipeline → `agent` node → Agent) | Works through nested LangGraph runs. | Works: the child pipeline binds its own scope, call owners and event receivers; pause cards carry the full path (`child-flow ▸ worker`). Before this PR the Worker refused it with `unsupported_capability`. |
| A nested Agent that has a saved pipeline attached as a tool | The tool is offered and runs. | Offered and run. Before this PR the attachment was dropped without a word. |
| A tool-invoked saved pipeline with a static pause (`interrupt_after`) | The user continues the pause. | Continues once through the typed coordinator under an ordinary Agent. Under a pipeline (graph) root it is refused before any model call (see Resilience). |
| Hop and version-byte limits | None. | 25 saved-application hops, 4 MiB of saved versions and 2 ordinary graph scopes per assembly, always enforced. |
| A conversation an older Worker left paused inside an application call | n/a | Continues on the resolver that paused it (data compatibility, below). |

Deliberately **not ported**: the legacy park/reconcile/replay supervisor, "lost, run again from scratch" on a
mid-run restart, and silently dropping an attachment the Worker cannot run.

## What was deleted

| Deleted | Where (on `main` at `c79fe73d2`) |
|---|---|
| The constant | `agents/pipeline.rs:75-76` |
| `scoped_applications: bool` on `bind_node_runtimes`, replaced by the typed `ApplicationContinuation` | `agents/pipeline.rs:1140-1156,1187` |
| `CompositionAdmission::scoped_ready`, the `ordinary_agents_supported` parameter and the `unsupported_pipeline_runtime` refusal of a nested ordinary Agent | `agents/pipeline/composition.rs:15,63,131,180-183,215` |
| `admit_tool_composition_with_scoped` (folded into `admit_tool_composition`) | `agents/pipeline/composition.rs:235-269` |
| `ApplicationMaterializationPath::scoped_ready`, its `bool` constructor argument and the three `if scoped_ready` budget/ownership branches | `agents/application_tools/materialization.rs:22,28,33-53,103,119` |
| The refusal that hid nested pipeline attachments | `agents/application_tools.rs:3083` |
| The "static continuation" and "ordinary child continuation" refusal keyed to the flag, and the flag-keyed call-owner insertion | `agents/pipeline.rs:1717-1740,1786,1854` |

`build_application_resolver` is still used, by exactly one branch, and is renamed
`build_legacy_unscoped_application_resolver` (`agents/pipeline.rs:754`).

### The one thing kept: older conversations (data compatibility, not a switch)

`ApplicationContinuation` (`agents/application_tools/materialization.rs:23`) has two values. Everything this
Worker starts (`Fresh`, `Regenerate`) is `Scoped`. A resume is `LegacyUnscoped` only when the checkpoint **has no
graph-call receipt and its frontier is an application node** (`continuation_for_checkpoint`,
`agents/pipeline.rs:1854`): that is a call an older Worker left in flight, and there is no receipt to prove it
against. Brief §4 keeps `has_graph_call_receipts` as the discriminator; the frontier conjunct is the one addition, and only a
start that continues a decided pause is inspected (`continues_a_decided_pause`, `agents/pipeline.rs:1230,1837`). A recovery
start (`Checkpoint`) is always scoped: a missing receipt there means a crash between the frontier save and the first
receipt revision, and taking the legacy resolver would downgrade the conversation for good (found in review; test
`a_recovery_start_never_takes_the_older_conversation_resolver`).
Receipts alone mis-classify a new conversation that paused at a HITL gate or printer before its first Agent call:
it has no receipt either, so every Agent call after the gate would have run unscoped. Test
`a_new_conversation_paused_before_its_first_agent_call_continues_scoped` fails if the conjunct is removed (checked).

## Rust code layout (ADR-0027)

Everything here is Worker wiring or Worker storage: assembly (`agents/pipeline*`, `agents/application_tools*`,
`agents/session.rs`) and the PostgreSQL stores (`state/postgres_session.rs`). Nothing reusable was added, nothing
moved, and `libs/rust` is unchanged (no `libs/rust` file differs from `main`). No dependency was added;
`Cargo.toml` and `Cargo.lock` are unchanged in both workspaces.

## Defects found by running the newly enabled path on real PostgreSQL

The in-memory doubles hid both. They surfaced as soon as the nested-Agent-in-a-saved-child scenario ran against
the production stores across a process replacement, and are fixed with targeted tests.

1. **`session.event_conflict` on resume.** A resumed graph re-sends the original application start and result
   events (same event id, content and timestamp) routed to the **current** invocation: a `root-projection` stamp and
   a descendant container edge naming it. The exact append check called every such replay a conflict, so the first
   resume of a nested Agent failed with "the durable session state is inconsistent". Fix:
   `PostgresSessionService::with_replay_equivalence` (`state/postgres_session.rs:458`) with
   `canonical_replay_form` (`agents/pipeline/scoped_applications.rs:534`), applied to the pipeline session at
   `agents/session.rs:1242`. A re-send is the stored event only when the two agree **after removing those two
   routing keys**; id, author, branch, invocation, content, timestamp and every other metadata key must match, and
   the stored row is never rewritten. Proofs: `a_routing_only_replay_of_a_stored_event_is_idempotent_and_any_other_change_conflicts`
   (PG; content, timestamp and a forged lineage key still conflict), `replay_canonical_form_removes_the_current_routing_and_nothing_else`.
2. **`checkpoint.conflict` on the first receipt revision of a resumed execution.** The turn checkpointer restamps
   every append with the executing turn (`elitea.pipeline.execution.v1`), so the first receipt revision of a thread
   whose latest checkpoint an earlier execution wrote differed from its stored parent in that stamp alone, and the
   fenced frontier comparison refused it. Fix: the stamp is no longer part of `graph_frontier`
   (`agents/pipeline/scope_receipts.rs:156`), like the revision id and time. Proof:
   `postgres_graph_receipt_append_ignores_the_turn_stamp_and_nothing_else` (a forged `route` key still conflicts).

3. **`agent_progress.invalid_state` after a static pause beneath an ordinary Agent root.** Found in browser
   journey (c): when an Agent's saved pipeline tool paused on a static `interrupt_after`, both the paused child and
   the root emitted a result-bearing `full_message`, and the publisher refuses a second result event after the
   first durable acknowledgement ("the durably acknowledged result must remain the last progress event"). Fix: the
   immediate-descendant projection drops the child's `full_message`; the root emits the one result
   (`agents/events.rs:1568`). Proof: the assertion added to
   `static_saved_pipeline_produces_authorized_inventory_and_resumes_after_once` (red before, green after).

### FU-226: approving a sensitive tool that changes data, inside a pipeline `agent` node

Symptom (browser, pipeline 158 with agent 157, chats 886–897): the card shows, Approve returns 200, and the resumed
execution fails with the generic "The runtime operation failed." (`INTERNAL`). Agent 157's mock MCP tools publish no
`readOnlyHint`, so they are effectful, and the nested Agent refused an approved effectful replay. PR #1321 (FU-226,
merged into this branch) removes that refusal: an approved call runs exactly once, through the root dispatch journal.
This branch makes the scoped `agent` node follow #1321's rules. It ports nothing of its own earlier refusal.

- **An approval marks its receipt `Resuming` before the child starts** (`pipeline/scoped_runtime.rs:220-241`). A copy
  of the paused receipt can never drop the approval. The marker writes one receipt revision of the pause checkpoint.
- **A replacement continues from that revision.** A decided pause continues from its own checkpoint or from the one
  revision that turns its call `Paused` → `Resuming` and changes nothing else
  (`pipeline/scope_receipts.rs:175` `continues_pause_checkpoint`; used by the resolver `graph/resume.rs:651,671` and by
  the scoped family proof `pipeline/scoped_applications.rs:1289,1318`). Any later revision (a re-pause, a finished
  call) is an advance, and the decision is stale. Whether the dead continuation belonged to this execution is
  #1321's `continuation_pause` check: the continuation writes its owner-bound `continuing` record before the
  Runner starts, so another execution's revision is refused as stale.
- **Inside the continuation, #1321's journal decides.** A recorded outcome is served (R). A dispatch without one is
  refused as `RECOVERY_REFUSED` (C). A call that never started runs once.
- **The scoped node keeps the registered stop codes.** A child that stops with `agent.recovery_refused` or
  `agent.dispatch_unrecorded` ends the run with that reason, whether the stop arrives as the child's result or on its
  event stream (`scoped_runtime.rs:336,401,576-600` `ChildFailure::ApprovedCallStop`). Before, the stream copy was a
  scope failure (`agent.legacy` → `INTERNAL`), depending on which arrived first. A decided call that cannot run is
  still `pipeline.tool_unavailable`. Other `direct_hitl.*` refusals are a node failure. Every other child error keeps
  the scope failure.

- **The child is rebuilt from the session up to the bound pause** (`session.rs` `events_through_pause`, used by the
  Hitl and static installers). This matches #1321's ordinary-root rule (`live_nested_events`). A dead continuation
  persists the child's forwarded events after the pause. When its tool result was persisted before the crash, the
  rebuilt child saw the decided call as finished. The replay then failed ("unavailable for exact replay" → scope
  failure → `INTERNAL`), although the journal held the outcome. Whether the result was persisted depends on timing,
  so this showed only under load. The journal facts are already in the resolved decisions.

Proofs, real PostgreSQL with each phase in its own OS process and claim:

- `pipeline_scoped_pg_tests.rs`: approve runs once and never again; a later turn and an older-Worker conversation
  each run once; dispatched before a crash is refused, not repeated; block resumes once; a three-call batch resumes
  once.
- #1321's `pipeline_effect_pg_tests.rs` (11 scenarios), which run on the scoped path here: crash before dispatch,
  after forwarding, after dispatch and after the outcome; a failed dispatch record; a late duplicate.
  - Before the `Resuming`-revision rule, 5 of these were refused as stale or as a scope mismatch.
  - Before the stream mapping, the failed-dispatch-record scenario ended `agent.legacy` in either arrival order.
  - That scenario then passed 10/10 repeated runs.
- `a_decided_pause_continues_only_from_itself_or_its_resuming_revision` (unit) pins the acceptance and the advances.
- Load proof for the crash-after-outcome window. Eight concurrent copies of the effect PG suite (the test binary run
  directly, 5–10 rounds each):
  - before `events_through_pause`: 26/80, 10/40 and 12/40 runs failed, all in the two crash-after-outcome
    scenarios;
  - after it: 0/40;
  - #1321 alone, same load: 0/40, so the gap was the scoped installer's;
  - `scoped_pg_tests` plus `restart_pg_tests` at the same load: 0/24.

A further change closes a gap rather than a defect: a **partial decision set** for paused siblings was refused with
the typed input error on the legacy resolver, but on the scoped resolver it ran the approved siblings and then failed
mid-stream on the rest. The all-decisions rule now runs before either resolver
(`agents/session.rs:2077-2093`). The existing tests that assert it
(`pipeline_agent_node_resumes_parallel_nested_*`) pass on the scoped resolver.

### An ordinary Agent's saved pipeline that stops at a static pause (journey (c))

This branch makes a static saved pipeline reachable as an ordinary Agent's tool. Before, the pipeline was skipped and
not offered. The path must work end to end (PLAN §8.1), not sit behind a notice.

Symptom (browser, agent 171 calling static pipeline 170, chat 920, Worker `2a1-99c90e178`): the pipeline paused at
`interrupt_after`. The Worker then closed the turn with `agent_pause.invalid_state` (execution `1a0ad8011d7b`,
FAILED/INTERNAL). Main refused the follow-up message ("application turn row not found"), because no pause had been
stored.

Root cause (pre-existing since the static inventory landed, unreachable until this branch):
- The projector counted the Agent's own static tool inventory as a pause (`is_paused`).
- The lifecycle's pause aggregator (`native_agent_lifecycle.rs` `AgentPauseAccumulator`) knows only HITL and
  authorization cards, so it could never finish such a pause.
- Main expects this pause as a COMPLETED `full_message` carrying `pipeline_static_tools_v1`
  (`elitea-main/.../repos/agent_execution_results.go:374,406`). It continues from that stored inventory
  (`repos/agent_start.go` `ResolveCurrentStaticToolContinuation`), so Main needs no change.

Fix (Worker only):
- `events.rs:2530-2546`: `is_paused` no longer counts the Agent's own inventory. A descendant's inventory still
  suspends its parent (`is_suspended`). `has_static_tool_pause` names it.
- `events.rs:1272`: the inventory is collected; the mid-stream result message is removed.
- `events.rs:3443,3578`: `finish_after_eos` ends the turn with the static pause message and the inventory on the
  one terminal `full_message`.
- `runtime.rs:146`, `session.rs:1787`, `agent_preparation.rs:1134`, `native_agent_lifecycle.rs:605`: the completion
  is selected as a static pause. Content comes from the projector; the cloud workspace is published as paused.
- `native_agent_lifecycle.rs:520`: a static inventory mixed with a pause card is refused (Main refuses that mix
  too), never published half.

Proofs:
- `static_tool_pause_is_the_acked_completed_terminal_with_its_inventory` (`execution/output_delivery_tests.rs:3605`):
  the production `OrdinaryNativeAgentAssembler` runs through `NativeAuthorizedAgentLifecycle`. It ends as the
  acknowledged COMPLETED terminal with one `full_message` carrying the inventory. **Red without the fix**: no
  acknowledged settlement, as live.
  - The test runs on a 4 MiB thread. In a debug build, the real assembler with any nested saved pipeline (a plain,
    non-pausing one too) exceeds the 2 MiB the fake-agent lifecycle cases use (follow-up below).
- Real PostgreSQL, each phase its own OS process and claim:
  `postgres_ordinary_agent_static_saved_pipeline_pause_completes_and_continues_once`
  (`pipeline_scoped_pg_tests.rs:314`).
  - `static_start`: the terminal message carries the inventory.
  - `static_continue`: a replacement Worker continues the pipeline once after its tick ("COUNT 1", never "COUNT 2").
  - `static_repeat`: a second continuation of the consumed pause is refused before any model call.
- In-process: `an_ordinary_agent_offers_a_static_saved_pipeline_and_continues_it_once` and
  `static_saved_pipeline_produces_authorized_inventory_and_resumes_after_once` assert the new terminal shape.

## Changed paths

| Path | Change |
|---|---|
| `src/agents/pipeline.rs:73,459-468,496,694-715,754,1220-1251,1799-1830,1837,1854,1893,1979` | Constant deleted; `ApplicationContinuation` threaded through `bind_node_runtimes`; resolver chosen by `match`; legacy resolver renamed; `continuation_for_checkpoint`; static pause under a graph root not offered (degrade); call owners keyed to the path, not a constant. Test seam `with_fresh_start_continuation` (`#[cfg(test)]`). |
| `src/agents/application_tools/materialization.rs:23-80,127-150` | `ApplicationContinuation`; `native_graph_root`; budgets unconditional. |
| `src/agents/application_tools.rs:81-88,3084-3088,5125-5149` | Re-exports; nested pipeline attachments materialized for scoped paths; typed nested replay refusal (FU-226). |
| `src/agents/pipeline/scoped_runtime.rs:220-241,324-350,401-425,570-600` | `Resuming` marker before an approved child starts; `ChildFailure` classification of a child's result and of its stream (registered approved-call stops, `pipeline.tool_unavailable`, node failure). |
| `src/agents/pipeline/scope_receipts.rs:175` | `continues_pause_checkpoint`: the pause or its one `Resuming` revision. |
| `src/agents/graph/resume.rs:651,671`, `src/agents/pipeline/scoped_applications.rs:1289,1318` | The resolver and the scoped family proof accept that revision. |
| `src/agents/direct_hitl.rs:1309` | `approves_sensitive_call` (the decision that writes `Resuming`). |
| `src/agents/events.rs:1561-1570` | A paused descendant's `full_message` is not re-emitted; the root owns the one result event. |
| `src/agents/pipeline/composition.rs:11-232` | Flag, parameter, refusal and `*_with_scoped` removed. |
| `src/agents/pipeline/scope_receipts.rs:30,156` | Turn stamp ignored by the frontier comparison. |
| `src/agents/pipeline/scoped_applications.rs:534` | `canonical_replay_form`. |
| `src/agents/session.rs:1328,2038,2077-2093,2104,3487` | Replay equivalence on the pipeline session; all-decisions rule before both resolvers; scoped installers rebuilt from the session up to the bound pause (`events_through_pause`). |
| `src/state/postgres_session.rs:326,446-512,830-918` | `with_replay_equivalence`, `is_stored_replay`, `is_already_stored` (extracted so `append` stays under the line limit). |
| Tests | `src/agents/pipeline_scoped_tests.rs` (new, 12), `pipeline_scoped_pg_tests.rs` (new, 6 scenarios), `pipeline_tests.rs`, `pipeline_restart_pg_tests.rs`, `application_tools/materialization_tests.rs`, `state/postgres_session_tests.rs`, `state/postgres_checkpointer_tests/graph_receipts.rs`, `pipeline/scoped_applications.rs` (unit). |

## User-visible differences to know about

- **Graph Agent call ids** are now `pipeline-v2:<node>:<digest>:<step>` instead of `pipeline:<node>:<step>`. They are
  opaque correlation ids; the projector already understood `pipeline-v2` (`agents/events.rs`) and no Web or Main
  code parses either form (searched `apps/elitea-web/src`, `services/elitea-main`).
- **A nested Agent whose saved pipeline attachment the Worker cannot build** (a deleted frozen version, a third
  ordinary scope, a pause kind the coordinator does not support) is skipped and named in the browser notice, as a
  root Agent's attachment is (#990); the turn still answers. Review round 1 reverted an earlier draft of this PR that
  failed every turn at assembly.
- **Static pause card text** (Web): "Execution paused at static interrupt. Choose the action to proceed or type the
  continuation message in chat." (was "Execution paused at {{node}}. Choose the action to proceed.").
- **Typed continuation** (Web): a message typed in the chat input while a static pause is current continues that pause
  with the text (`agent.continue.static.v1`; every paused pipeline of a saved-pipeline set), instead of starting a new
  turn that paused again. `ChatBox.tsx` routes `onSendNow` to `StaticPipelineContinueControls.tsx` `onSendText`;
  proven by `useStaticPipelineContinuation.test.tsx` "continues every paused leaf with a message typed in the chat
  input".
- **One extra bounded `SELECT`** per non-fresh resume (the root checkpoint read that decides the continuation).

## Tests

Counts are from `cargo test --offline --locked --no-fail-fast --all-targets --all-features` in
`services/elitea-worker-rust` against a disposable `postgres:18` with the CI environment
(`ELITEA_TEST_DATABASE_URL`, `ELITEA_REQUIRE_POSTGRES_RECEIPT_TESTS=1`, `ELITEA_TEST_DATABASE_GUARD=disposable-pg18`).
**2608 passed, 0 failed, 103 ignored** across all targets after review round 1 (2602 before it; the six new tests are
listed in the recovery rows), on the head that merges `main` `d5e966074` (with #1321), no PostgreSQL skips. The ignored tests are the
supervisor, preparation and hydration `#[ignore]` suites CI runs separately; this PR does not touch them, so they
were not run. `ELITEA_REQUIRE_NATS_SECURE_TEST` was not set locally (the secured-NATS permission test needs
`natstest-serve`), which CI provides.

Assembled through `PipelineNativeAgentAssembler::assemble` (the real `bind_node_runtimes`, composition admission and
model-scope sessions; only the platform RPC, keyed by saved identity, and the model gateway are scripted):

| Brief §2.5 behaviour | Test | Fails on `main` because |
|---|---|---|
| Scoped resolver, call owners and receipts | `a_graph_agent_node_runs_on_the_scoped_resolver_and_owns_original_call_receipts` | the checkpoint holds no graph-call receipt |
| Nested ordinary Agent in a saved child pipeline, ask, replace the Worker, resume once, answered card consumed | `an_ordinary_agent_in_a_saved_child_pipeline_asks_and_resumes_once_after_process_replacement` | assembly refused: `unsupported_capability` |
| Nested pipeline child offered to an Agent tool and run | `a_nested_agent_offers_its_saved_pipeline_child_as_a_tool_and_runs_it` | the model request carries no tool |
| Static continuation in a tool-invoked saved pipeline, once | `an_ordinary_agent_offers_a_static_saved_pipeline_and_continues_it_once` | the pipeline is skipped, not offered |
| A static-pausing pipeline attached to a graph `agent` node's Agent is not offered; the node answers | `a_graph_root_does_not_offer_a_static_saved_pipeline_tool_and_the_node_answers` | degrade (the projector cannot name two owners), review round 2 |
| A pipeline `agent` node's unbuildable nested attachment is skipped; the node answers | `an_unbuildable_attachment_of_a_pipeline_agent_node_is_skipped_and_the_node_answers` | review round 2 |
| Two sub-Agents in one step: a static pause beside a nested card ends typed, settled and acked | `a_static_pause_beside_a_nested_card_fails_typed_settled_and_acked` | review round 2 |
| Hop budget: 25 admitted (25 version reads), 26 refused | `the_application_hop_budget_admits_the_limit_and_refuses_one_more` | 26 assembles |
| Version-byte budget: exactly 4 MiB (five versions) admitted, +1 byte refused | `the_version_byte_budget_admits_four_mib_and_refuses_one_more_byte` | +1 byte assembles |
| Ordinary graph scopes: 2 admitted, 3 refused | `the_ordinary_graph_scope_budget_admits_the_limit_and_refuses_one_more` | the limit itself is refused (nested Agents unsupported) |
| Negative authorization: a saved participant of another project is refused before any version read or model call | `a_saved_participant_from_a_foreign_project_is_refused_before_any_effect` (pipeline and Agent) | passes on `main` too: this PR makes the check apply to nested scopes as well |
| New conversation paused before its first Agent call continues scoped | `a_new_conversation_paused_before_its_first_agent_call_continues_scoped` | no receipt is written |
| Older conversation paused in an Agent node continues unscoped, writes no receipt | `a_conversation_an_older_worker_paused_in_an_agent_node_continues_unscoped` | new seam |
| Older conversation paused in a saved child pipeline continues unscoped | `a_saved_child_pipeline_an_older_worker_paused_continues_unscoped` | new seam |

The first eight tests written (receipts, nested ask/resume, nested attachment, a graph-root variant of the static
test, hop, version bytes, scopes, foreign project) were run against `main` before any production change: all eight
failed (`bind_node_runtimes` took its unscoped branch; the foreign-project one only on a wrong expected error code of
mine, and it passes on both once corrected). The ordinary-root static test, the graph-root refusal and the three
older/new-conversation tests were written afterwards; the last three need the `ApplicationContinuation` seam and cannot
compile on `main`, and the first two assert behaviour `main` does not have (the pipeline is skipped, not offered).

Real PostgreSQL, each phase in its own OS process and claim (`pipeline_scoped_pg_tests.rs`, same harness as
`pipeline_restart_pg_tests.rs`):

- `postgres_nested_ordinary_agent_in_a_saved_child_resumes_once_after_process_replacement`: `start` pauses a nested
  `ask_user` card (path `child-flow ▸ worker`); `answer` (replacement Worker) resumes and finishes with exactly one
  model call; `repeat` (another replacement Worker) sends the same answer and resumes nothing (0 model calls). The
  root checkpoint carries the graph-call receipt.
- `postgres_conversation_an_older_worker_paused_continues_unscoped_without_receipts`: the first leg runs on the legacy
  resolver, the current Worker answers, and no checkpoint row ever carries a receipt.
- The existing `postgres_two_nested_agent_pauses_resume_across_process_replacement` (two sequential asking nodes, three
  processes) now runs on the scoped resolver. Its platform fixture was queue-ordered; it is now keyed by saved
  identity because the scoped resolver binds one Agent per node.

Existing assertions that changed: the nine pipeline tests that compared `pipeline:delegate:0` now compare the
deterministic scoped id (`graph_call_id` in `pipeline_tests.rs`); `production_gate_keeps_ordinary_graph_descendants_closed`
was deleted with the gate; `legacy_plain_saved_graph_remains_materialized_without_scoped_activation` now names the
older-conversation case.

Other gates: `cargo fmt --all -- --check` clean; `cargo clippy --offline --locked --all-targets --all-features -- -D warnings`
clean; `RUSTDOCFLAGS="-D warnings" cargo doc --offline --locked --no-deps --all-features` clean;
`libs/rust` is unchanged by this branch (identical to `main`). `cargo deny --all-features check advisories`: two **pre-existing** findings,
RUSTSEC-2023-0071 (`rsa`, Marvin attack) and RUSTSEC-2024-0449 (`unsafe-libyaml` unmaintained); this PR changes no
dependency, so it adds none.

## Performance

| Budget | Mechanism | Proof / measurement |
|---|---|---|
| Saved-version reads per assembly ≤ `MAX_APPLICATION_HOPS` (25); one read per application node | `ApplicationMaterializationPath::enter` (`materialization.rs:127`) | the hop test asserts exactly 25 reads at the limit and no further read for node 26 |
| Saved versions ≤ 4 MiB per assembly | `admit_version` (`materialization.rs:136`) | exactly 4 MiB (5 versions) admitted; +1 byte `resource_exhausted` |
| One extra bounded `SELECT` per non-fresh resume | `state.application_checkpoint` (`pipeline.rs:1232`) | by construction (one `load` of the root thread) |
| Replay equivalence reads the stored row only on the conflict path | `is_stored_replay` | a normal append is unchanged (one lookup, as before) |

The scoped resolver binds one Agent per application node, where the legacy resolver built one per alias, so an
assembly with N nodes calling one alias reads N versions instead of 1, bounded by the 25-hop budget above. The
platform caches nothing across nodes here; a per-alias cache is a follow-up if browser timing shows it matters.

## Durability

Crash windows touched, and the rule for each:

| Window | Rule | Proof |
|---|---|---|
| Between a graph call's receipt `started` and its result | Resume continues the receipt; the node never re-plans the call | PG `nested` scenario, `answer` phase: 1 model call |
| Pause → Worker replaced → answer | One continuation; the original start/result events are replayed, not duplicated in the session | PG `nested`: session append idempotent (defect 1), receipt revision accepted (defect 2) |
| Answer sent twice (second Worker) | The consumed card resumes nothing | PG `nested`, `repeat` phase: 0 model calls |
| An older conversation after deploy | Resumes on the resolver that paused it; no receipt is invented | PG `older` scenario and the in-memory twins |
| A re-sent event differing outside routing | Still `session.event_conflict` (history is never rewritten) | `a_routing_only_replay_of_a_stored_event_is_idempotent_and_any_other_change_conflicts` |

## Resilience

- Everything is bounded: 25 hops, 4 MiB of versions, 2 ordinary graph scopes, composition depth 3 and 25 composition
  hops (the existing `composition.rs` limits), now for every pipeline run.
- Typed, readable refusals before any model call: nested attachment limits and the static-under-graph-root refusal
  return `unsupported_capability`; a partial decision set returns `invalid_input`.
- Cancellation, lease loss and deadlines are unchanged: the receipts and session writes keep the existing writer
  fencing (`lock_current_writer` in the same transaction as the append).
- No silent coercion: the legacy branch is chosen from the checkpoint itself, never from configuration.

## Security

Checked against `.claude/rules/security.md`:

| Category | Applies | How checked |
|---|---|---|
| Trust boundaries and identity | No change | No header or peer identity is read; the claim-bound context is the only authority (unchanged). |
| Object-level authorization | Yes (nested scopes now reachable) | Foreign-project participants are refused before any version read or model call, for a pipeline child and an Agent. Project scope is checked in `composition.rs` (`saved`) and `application_tools.rs` (`build`) for every nested participant. |
| Input, parsing and amplification | Yes | Hop, version-byte, scope and depth budgets are unconditional and tested at limit and limit+1. |
| Injection and construction | No new SQL string building | The new query is parameterized (`is_stored_replay`); no template, shell or URL construction. |
| Egress and SSRF | No new egress | |
| Secrets | No new secret handling | Test fixtures carry the placeholder `ephemeral-scoped-token` only; the staged diff was scanned for key, token and private-key patterns. |
| Supply chain | No dependency change | Pre-existing `cargo deny` advisories listed above; none new. |
| Replay equivalence weakens nothing | Yes | Only the two named routing keys are ignored, both described in code as presentation routing; every identity and authority field and every other metadata key must match, proven with forged-content, forged-timestamp and forged-lineage conflicts. |

Logs and errors: no prompt, argument, token or URL is added to any log or error; the new refusals use fixed strings.

## Recovery rows (replatform-delivery-gate §2)

| Component × phase | Class | Enforcing code | Proof |
|---|---|---|---|
| Worker × P12 HITL pause and decision, **nested ordinary Agent in a saved child pipeline** | **R** | `scoped_runtime.rs` call owners, `scope_receipts.rs:156` (frontier), `postgres_session.rs:832` (idempotent replay) | PG `postgres_nested_ordinary_agent_in_a_saved_child_resumes_once_after_process_replacement`; browser journey (b) below |
| Worker × P12, graph Agent node with a sensitive-tool card | R | same | existing PG `postgres_two_nested_agent_pauses_resume_across_process_replacement` on the scoped resolver; browser journey (a) |
| Worker × approved effectful call in a scoped `agent` node, Worker replaced before dispatch or after forwarding | R (runs once) | `scope_receipts.rs:175`, #1321 `dispatch_journal.rs` | `pipeline_effect_pg_tests.rs` (crash before dispatch, after forwarding); `pipeline_scoped_pg_tests.rs` approve-once; browser journey (a) |
| Worker × same, after dispatch without a recorded outcome | C (`RECOVERY_REFUSED`, never repeated) | #1321 journal; `scoped_runtime.rs:336` | `postgres_worker_replaced_after_dispatch_refuses_as_may_have_started`, `postgres_effectful_approval_dispatched_before_a_crash_is_refused_not_repeated` |
| Worker × same, after the recorded outcome | R (the recorded result is served) | #1321 journal | `postgres_worker_replaced_after_the_recorded_outcome_resumes_with_it` |
| Worker × same, the dispatch record cannot be written | F (`agent.dispatch_unrecorded`, nothing ran) | `scoped_runtime.rs:401-425,576-600` | `postgres_failed_dispatch_record_ends_the_run_typed_and_never_runs_the_call` |
| Worker × same, a late duplicate decision | I (stale, no effect) | `graph/resume.rs:651` + #1321 `continuation_pause` | `postgres_late_duplicate_approval_after_a_dispatching_continuation_is_stale` |
| Worker × P12, conversation paused by an older Worker | R (continues on the resolver that paused it) | `pipeline.rs:1859` `continuation_for_checkpoint` | PG `postgres_conversation_an_older_worker_paused_continues_unscoped_without_receipts` |
| Worker × P12, older-Worker conversation, Worker killed **mid-continuation** (or an old Worker killed during the deploy) | R (Main re-claims the same decided-pause start, which reads its resolver from the checkpoint: no receipt + application frontier → the older resolver; no fresh `Started` receipt, the child is not re-run from its task input) | the decided-start branch `pipeline.rs:1234` (`continues_a_decided_pause`, `:1844`) → `continuation_for_checkpoint`; a recovery start never carries an application frontier (`graph/compiler.rs:978` `recovery_frontier_supported`, checked at `session.rs:1471`) | PG `postgres_older_conversation_recovers_mid_continuation_on_its_own_resolver` (start → crash with `ExitProcess` inside the continuation → re-claim answers once, 0 receipt rows); `a_recovery_start_never_takes_the_older_conversation_resolver`. This proves behaviour the branch already had; no code change was needed |
| Worker × P12, older conversation beyond the hop or version-byte budget (> 25 application hops, > 4 MiB of frozen versions) | F (`InvalidConfiguration`, refused before any model call or effect; partial results stay in the stored conversation). Justified: the budgets bound untrusted, stored nesting and now apply to every conversation; such a conversation could not have been built through the current editor limits | `application_tools/materialization.rs:123-150` | `the_application_hop_budget_admits_the_limit_and_refuses_one_more`, `the_version_byte_budget_admits_four_mib_and_refuses_one_more_byte` |
| Worker × assembly, a nested Agent's attached pipeline the Worker cannot build (deleted frozen version, unresumable pause kind, > 3 scopes) | R for every turn that never calls it (named skip in the browser notice, as for a root Agent's attachment, #990); the call is not offered | `application_tools.rs:3377-3430`, `:3175` | `an_unbuildable_pipeline_of_a_nested_agent_is_skipped_and_the_root_still_answers` (red on the old `UnsupportedCapability` assembly failure) |
| Worker × assembly, a **pipeline** `agent` node whose Agent has an attachment the Worker cannot build (including a static-pausing saved pipeline, which a graph root cannot project) | R for every run: the attachment is not offered and the node answers; a pipeline root has no skipped-capabilities notice, so the skip is logged (`pipeline_agent_attachment_skipped`, a count, no names) | `pipeline.rs:2108-2119` (`bind_scoped_agent`), `:807-809` | `an_unbuildable_attachment_of_a_pipeline_agent_node_is_skipped_and_the_node_answers`, `a_graph_root_does_not_offer_a_static_saved_pipeline_tool_and_the_node_answers` (both red on the old assembly refusal) |
| Worker × P12, decided card a scoped Agent node's child refuses | F, registered public code per refusal: `pipeline.tool_unavailable` (replay unsupported, nothing ran), `pipeline.input_invalid` (stale / invalid / corrupt decision), `pipeline.input_limit` (exhausted budget), `agent.recovery_refused` / `agent.dispatch_unrecorded` (#1321); a broken scope keeps the scope failure | `scoped_runtime.rs:324-350,588-616` | `every_child_refusal_reports_its_registered_public_code` |
| Worker × tool call, static pause of a tool-invoked saved pipeline (ordinary Agent) | R: the turn completes with the inventory; a replacement continues once after the tick | `events.rs:2530-2546,3443,3578`, `native_agent_lifecycle.rs:520,605` | lifecycle `static_tool_pause_is_the_acked_completed_terminal_with_its_inventory`; PG `postgres_ordinary_agent_static_saved_pipeline_pause_completes_and_continues_once`; browser journey (c) |
| Worker × tool call, static pause of a **sub-Agent's** saved pipeline (Agent → sub-Agent → static pipeline, depth 2) | R: the root turn completes with the nested pause in its inventory; one continuation resumes the pipeline, then the sub-Agent and the root answer once | `events.rs:1265-1275,1727-1772` | lifecycle `a_sub_agents_static_pipeline_pause_is_the_acked_completed_terminal`; `a_static_pause_of_a_sub_agents_pipeline_completes_and_continues_once` |
| Worker × tool call, one model step calls a static-pausing pipeline tool **and** a pause card (`ask_user`, sensitive tool) | R on the card: the turn ends as the acknowledged PAUSED terminal with one card and no inventory; a static pause recorded beside a card can never be published as half a result (typed `UnsupportedCapability`, settled and acknowledged) | `native_agent_lifecycle.rs:517-525` | lifecycle `a_step_calling_a_static_pipeline_and_ask_user_pauses_once_on_the_card` |
| Worker × tool call, one model step calls two sub-Agents: one reaches a saved pipeline's static pause, the other raises a card (`ask_user`, sensitive tool) inside itself | F (`UnsupportedCapability`, settled and acknowledged; no result message, no PAUSED terminal; the card streamed as live progress is never stored, because Main stores a pause only from a PAUSED terminal bound to its `hitl-interrupt` artifact, `agent_execution_results.go:290`). Justified: one terminal carries either a static inventory (COMPLETED) or cards (PAUSED), and Main refuses the mix; supporting both needs a combined terminal contract, listed as a follow-up. The user re-asks; nothing ran past the two pauses | `native_agent_lifecycle.rs:517-525` | lifecycle `a_static_pause_beside_a_nested_card_fails_typed_settled_and_acked` (red without the guard: a PAUSED terminal is published beside the inventory) |
| Worker/Main × output delivery + ACK of the COMPLETED static-pause terminal (Worker crash after publish, before ACK) | I: the terminal is an ordinary bound `full_message` artifact; recovery reopens and replays the exact durable frame (same event id, payload digest and artifact), Main persists it idempotently and refuses a changed replay; a static inventory mixed with pause authority is refused by Main | Worker spool replay (`agent_output` recovery); Main `agent_execution_results` static persistence | Worker `uncertain_live_progress_reopens_and_replays_the_exact_durable_frame`, `restored_full_message_ack_retains_the_same_terminal_artifact_authority`, `changed_durable_progress_is_rejected_before_a_replay_session_starts`; Main `TestStaticFullMessagePreservesTheValidatedPauseAndRejectsMixedTerminal`, `TestStaticFullMessagePersistenceAndOrdinaryCompletionClearing` |

## #1321 post-merge verification

#1321 (approved sensitive calls run once, FU-226/FU-236) merged into `main` after this branch took its last copy of
it. This branch merges `main` `d5e966074`, which contains #1321 (`0d2bd889d`). Everything below this point also runs
#1321's code as merged:
- the full Worker suite on real PostgreSQL (#1321's `ordinary_effect_pg_tests` and `pipeline_effect_pg_tests`
  included);
- the image build;
- the browser journeys.

It therefore stands as #1321's post-merge check on `main`. The Main side of #1321 (`execution_settlements.error_code`)
runs in the Main image built from `d5e966074`.

## Browser evidence

Stack `twoaone` (2026-10-23), Chrome (Claude in Chrome), real models (`gpt-5.4-mini`, Claude Haiku), every case
reloaded before it was read.

**Stack and images.** Brought up with the updated local stack tool on the checkout at this branch's head. That
checkout is `main` `d5e966074` merged; Traefik is removed, so `elitea-main` is the edge. `verify-profile`:
**50 passed, 0 failed**.

| Service | Image | Note |
|---|---|---|
| Worker | `elitea-worker-rust:2a1-590117ade` (`sha256:15ab22885a1d…`) | §3b check below |
| Code supervisor | `elitea-sandbox-supervisor:2a1-1f617cfdb` (`sha256:6197f66ec44f…`) | |
| Main | `elitea-main:main-d5e966074-verify` (`sha256:ea25de4e1bf3…`) | |
| Web | `elitea-web:main-d5e966074-verify` (`sha256:3d31080c7772…`) | |
| gateway, scheduler, subapp-host, vector, doc-extractor, both engines, mocks, runners | `main-d5e966074-verify` | built from sources identical to `d5e966074` |

§3b check of the Worker binary (`docker create` + `docker cp` + `strings`):
- `2a1-590117ade` carries `events_through_pause`, `is_resuming_revision_of` and `has_static_tool_pause`. The first two
  are absent from #1321's image.
- Its binary differs from `2a1-1f617cfdb` (sha256 `49f2eeceab42…` against `8b4ff0d09992…`).
- It has 4 `OrdinaryAgentCompletion…select_static_pause` symbol references, against 2: the new forwarding path.

**Data.** The real-model dump `dd0bbc170` migrated forward with `main`'s own migrations: Main shared 172 and tenant
154, then deepwiki and inventory, which drop the vector objects. The result is the new runbook dump
`product-real-models-main-d5e966074.dump`.

Fixtures (stated honestly):
- Agent 157 got its instructions, the `gpt-5.4-mini` model and its single MCP toolkit through the UI.
- The static pipeline (168) and its caller agent (169) were created through the UI.
- The caller's pipeline tool was attached with a **database insert** that mirrors Main's own insert minus
  `owner_id`. Main's attach route answers HTTP 500 on legacy-created projects, because it writes
  `elitea_tools.owner_id`, which they lack. That is a pre-existing Main defect, listed under follow-ups.
- The `sensitive_tools` platform setting `{"mcp": ["reverse"]}` came with the dump.

| Journey | Chat | Executions (all SUCCEEDED, no settlement error) | Evidence |
|---|---|---|---|
| (c) ordinary Agent 169 → static saved pipeline 168: pause, reload, Continue | 883 | `76d5a887ea6b` (pause), `36fd57626d60` (continue) | After reload the turn shows "Pipeline paused …" with "Execution paused at tick" and Continue. After Continue the Agent answers: The pipeline result is "COUNT 1" (the tick ran once). |
| (a) pipeline 158 (`agent` node → Agent 157, sensitive MCP `reverse`), Authorize | 884 | `53cb3a55aa74`, `a1ba3f2408d3` | Card survives reload; "reverse returned ESREVER-1C."; mock MCP `tools/call` 0 → 1 |
| (a) pipeline 158, Block | 885 | `1e4c8ce10a59`, `721e46fcc1f8` | "The call was declined."; mock MCP stays 1 |
| (d) pipeline 138: two `agent` nodes, each a nested run of Agent 137 that asks once; Worker restarted (`docker restart`) while card 2 was pending | 886 | `7ed1082e7d5a`, `1039f10de51e`, `0c93b6b34669` | Both cards answered; after reload the report shows both calls' jokes once |

Before the wrapper fix, (c) failed on `2a1-1f617cfdb` as `native_agent.invalid_result`: chats 881 and 882, executions
`33879cd3b833` and `05f76d96e0e2` FAILED/INTERNAL. That run is how the production-wrapper defect was found.
- During diagnosis Agent 169's type was briefly set to `agent` in the database; it made no difference.
- It was reset to the UI's `openai` before the passing run, so the passing run uses the Agent as the UI created it.

Worker logs: no ERROR in the window. One WARN on the (c) continuation, `agent_session_terminal_completion_unavailable`
(best-effort enrichment; the result is correct).
- Gateway: no `llm_provider_refusal`.

UI note (parity, not a defect of this branch): the final message of (d) shows one answered-question summary (the
first `ask_user`). Web's `findAskUserAction` returns the first completed `ask_user` call, ported unchanged from
EliteaUI's `hitl.helpers.js`. Main stores both resolved question ids on the message.

Earlier evidence on the previous stack `scoped` (old platform, Worker `2a1-99c90e178`, now torn down): (a) chats
918/919 and (d) chat 921 passed the same way.

**Review round 1 re-check** (same stack, 2026-10-23, Chrome, real `gpt-5.4-mini`, every case reloaded before it was
read). Only the Worker and Web were swapped, both built from this branch:

| Service | Image | §3b check |
|---|---|---|
| Worker | `elitea-worker-rust:2a1-a1f21fd41` (`sha256:8c732cd720a5…`) | `docker cp` + `strings`: tracing markers `application_tools.rs:3059` and `:4270` match this head's source lines; the previous image carries `:3047` and `:4239` |
| Web | `elitea-web:2a1-a74ed37e4` (`sha256:c11c67f1586a…`) | the bundle contains "Execution paused at static interrupt" (3 occurrences) |

`verify-profile` after the swap: **50 passed, 0 failed**.

| Journey | Chat | Executions (all SUCCEEDED, no settlement error) | Evidence |
|---|---|---|---|
| (c) typed continuation: Agent 169 → static pipeline 168, reload, type "go ahead" in the chat input | 888 | `6b7326670280` (pause), `9f5679e30d84` (continue) | After reload the card reads "Execution paused at static interrupt. Choose the action to proceed or type the continuation message in chat."; the typed message continues the pause (no second user turn, no new pause; before this fix it started a new turn that paused again); answer "The pipeline result is "COUNT 1"", unchanged after a second reload |
| (c) Continue button, same fixture | 889 | `4a4d73f558d6`, `0fb60d71f29f` | "COUNT 1" after reload |
| (a) pipeline 158, sensitive MCP `reverse`, Authorize | 890 | `5697b273fd0b`, `e5532ad7cd3a` | card survives reload; "reverse returned ESREVER-1C"; mock MCP `tools/call` 1 → 2 (runs once) |
| nested (depth 2): Agent 170 "S1 nested caller" → Agent 169 → static pipeline 168, reload, typed "continue please" | 891 | `a5f51282d31d` (pause), `4cf89a602614` (continue) | the root turn shows the static pause card after reload; the typed continuation resumes the sub-Agent's pipeline; the root quotes the sub-Agent once: "The pipeline result is "COUNT 1"." |

Fixtures: Agent 170 was created through the UI (name, description, instructions, `gpt-5.4-mini`). Its tool (Agent 169)
was attached with a **database insert** mirroring Main's insert minus `owner_id` (`elitea_tools` 104,
`entity_tool_mapping` 123), for the pre-existing attach defect listed under follow-ups. Worker logs: no ERROR; nine
`agent_session_terminal_completion_unavailable` WARNs (best-effort enrichment, as before). Gateway: no
`llm_provider_refusal`.

## Follow-ups

- #1084 has merged, so the stale `docs/remaining-gates.md` sentence ("Scoped continuation admission remains
  disabled") is updated here. `docs/testing-gaps.md` has no 2a-1 entry. The historical packets
  `scoped-parallel-authority-20261004.md` and `code-assembled-acceptance-20261002.md` carry a superseded note.
- Static pause of a tool-invoked saved pipeline beneath a graph root: needs the projector to accept a second checkpoint
  owner; today the attachment is not offered to the graph `agent` node's Agent (logged, not named in a notice: a
  pipeline root has no skipped-capabilities notice yet).
- A combined terminal for one step that stops on both a static inventory and a card (today a typed failure).
- Main: attaching an Agent or pipeline as a tool (`api/v2/eliteacore/application_relation.go`
  `insertApplicationToolReference`) writes `elitea_tools.owner_id`, which legacy-created project schemas lack. HTTP 500
  on every project of the real-model dump (pre-existing; not this branch).
- `NativeRuntimeCompletion` does not forward `wrap_up` either (the cloud executor take-back); pre-existing, outside
  this change.
- A debug build of the real ordinary assembler with a nested saved pipeline exceeds a 2 MiB thread stack (the
  lifecycle test runs at 4 MiB). Release frames are smaller and the live Worker runs this path; measure release stack
  depth before tightening.
- A repeated static tool continuation refuses as `InvalidConfiguration`, not a typed stale code (Main never sends one).
- Web: the answered-question summary shows only the first `ask_user` of a message (EliteaUI parity); a product
  decision whether to list all.
- FU-226 follow-ups are #1321's (its source mapping). The legacy unscoped resolver still reports a generic node
  failure for a conversation an older Worker paused (the scoped resolver now sends registered codes).
- Web: after a reload, a message that is still at a static pause shows "Thought for -1 h -1 min and -2 sec" (the
  thinking header's duration); once continued it reads normally. Not introduced here (this branch does not touch
  thinking timestamps); a display follow-up.
- Web: the nested static pause's accordion is titled "Pipeline" without the sub-Agent level (the known one-level
  nesting render limit).
- Web: `src/shared/api/themePreference.test.ts` "keeps the mark in memory when storage refuses the write" fails
  locally on Node 24 (unrelated file, untouched here); CI runs Node 26.
- Review notes accepted as limits: replay equivalence removes the two routing keys rather than requiring equal presence
  (a presence-preserving form broke the stored-start replay, so it was reverted; the stored row is never rewritten);
  the descendant `full_message` filter relies on the root emitting the static inventory, which a well-formed static
  pause always does; a pre-2a-1 conversation now resumes under the hop and version-byte budgets.
- A per-alias materialization cache if N nodes calling one alias shows up in browser timing.
- The legacy resolver and `ApplicationContinuation::LegacyUnscoped` can be deleted when no conversation paused by a
  pre-2a-1 Worker can still be resumed (retention window); the PG `older` scenario is the test to delete with it.
