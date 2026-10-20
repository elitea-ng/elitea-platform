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

## Changed paths

| Path | Change |
|---|---|
| `src/agents/pipeline.rs:73,459-468,496,694-715,754,1220-1251,1799-1830,1837,1854,1893,1979` | Constant deleted; `ApplicationContinuation` threaded through `bind_node_runtimes`; resolver chosen by `match`; legacy resolver renamed; `continuation_for_checkpoint`; static pause under a graph root refused; call owners keyed to the path, not a constant. Test seam `with_fresh_start_continuation` (`#[cfg(test)]`). |
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
- **A nested Agent whose saved pipeline attachment cannot be run under a graph root** (a static pause, a third
  ordinary scope, a pause kind the coordinator does not support) now fails the pipeline run at assembly with
  `native_agent.unsupported_capability`, before any model call. Before this PR the attachment was silently dropped
  and the Agent ran without it. A named degrade, as the ordinary parent has, is a follow-up (below).
- **One extra bounded `SELECT`** per non-fresh resume (the root checkpoint read that decides the continuation).

## Tests

Counts are from `cargo test --offline --locked --no-fail-fast --all-targets --all-features` in
`services/elitea-worker-rust` against a disposable `postgres:18` with the CI environment
(`ELITEA_TEST_DATABASE_URL`, `ELITEA_REQUIRE_POSTGRES_RECEIPT_TESTS=1`, `ELITEA_TEST_DATABASE_GUARD=disposable-pg18`).
**2014 passed, 0 failed, 71 ignored** across all targets (library 1919 passed, 71 ignored). The ignored tests are the
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
| Static pause beneath a graph root is refused before any model call | `a_graph_root_refuses_a_static_saved_pipeline_tool_before_any_model_call` | new refusal (the projector cannot name two owners) |
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
`libs/rust` is unchanged; its workspace suite (`cargo test --offline --locked --workspace --all-targets --all-features`) passes 1640 tests, 0 failed, 1 ignored. `cargo deny --all-features check advisories`: two **pre-existing** findings,
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
| Worker × P12, conversation paused by an older Worker | R (continues on the resolver that paused it) | `pipeline.rs:1854` | PG `postgres_conversation_an_older_worker_paused_continues_unscoped_without_receipts` |
| Worker × tool call, static pause of a tool-invoked saved pipeline | R | `pipeline.rs:1799-1830`, `events.rs:1568` | `an_ordinary_agent_offers_a_static_saved_pipeline_and_continues_it_once`; browser journey (c) |
| Worker × tool call, static pause beneath a graph root | F (typed refusal before any model call; listed as a follow-up) | `pipeline.rs:1816-1829` | `a_graph_root_refuses_a_static_saved_pipeline_tool_before_any_model_call` |

## Browser evidence

EVIDENCE_PLACEHOLDER

## Follow-ups

- #1084 has merged, so the stale `docs/remaining-gates.md` sentence ("Scoped continuation admission remains
  disabled") is updated here. `docs/testing-gaps.md` has no 2a-1 entry. The historical packets
  `scoped-parallel-authority-20261004.md` and `code-assembled-acceptance-20261002.md` carry a superseded note.
- A named degrade (skip and say so) for a nested attachment the Worker cannot run under a graph root, instead of failing
  the run at assembly.
- Static pause of a tool-invoked saved pipeline beneath a graph root: needs the projector to accept a second checkpoint
  owner; refused today.
- FU-226 follow-ups are #1321's (its source mapping). The legacy unscoped resolver still reports a generic node
  failure for a conversation an older Worker paused.
- Review notes accepted as limits: replay equivalence removes the two routing keys rather than requiring equal presence
  (a presence-preserving form broke the stored-start replay, so it was reverted; the stored row is never rewritten);
  the descendant `full_message` filter relies on the root emitting the static inventory, which a well-formed static
  pause always does; a pre-2a-1 conversation now resumes under the hop and version-byte budgets.
- A per-alias materialization cache if N nodes calling one alias shows up in browser timing.
- The legacy resolver and `ApplicationContinuation::LegacyUnscoped` can be deleted when no conversation paused by a
  pre-2a-1 Worker can still be resumed (retention window); the PG `older` scenario is the test to delete with it.
