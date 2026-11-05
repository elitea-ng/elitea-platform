# Map on the shared fan-out adapter, admitted (Point 5 Wave 2a-3, T-2A3)

Date: 2026-11-04. Branch `feat/graph-fanout-map-admitted`. Plan: `handoffs/point5-wave1-20261008/map/PLAN.md` (review
`map/PLAN-REVIEW.md`, M1–M3; design `map/COMMIT2-DESIGN.md`).

## Business behaviour

Taken from the current platform's Map / for-each node: run one worker node once per item of a list from state, with
the item and its index in child-local channels and a set of broadcast state values, then collect the item results in
source order into a destination key.

On the new platform the Map is not a second engine: it runs on the fixed Parallel's shared fan-out adapter (one child
per source item, keyed by its decimal index), so it inherits Parallel's guarantees: per-interrupt cards answered in any
order, the park and the claim-fenced wake, crash recovery of each child from its own checkpoint, typed failures, and the
same limits (64 children, 8 concurrent, 16 open cards).

- Worker families: a State Modifier, a saved Agent (admitted by Parallel's participant proof) or an inline LLM node.
  Code, a direct Router and a saved worker pipeline holding Code wait for Wave 3 and are refused readably by name
  (Main at save: `PIPELINE_MAP_FORM_UNSUPPORTED`; Worker at bind: `graph.fanout.child_contains_code`).
- v1 documents (`reduction: ordered_collection`) keep their meaning and digest: stop on the first failed item, and the
  ordered `{index, outputs}` collection written through the destination key's own (overwrite) reducer (M2).
- v2 documents: `on_error` (continue | stop) and `join` (append | keyed), landing through the destination's declared
  reducer, checked at save time. One shape per document: `reduction` with `join` (M2) or with `on_error` is refused with
  the fix ("remove `reduction`") in all three layers.
  - **Deliberate narrowing of the brief: a Map never joins by `merge`.** Every item lands the same output keys, so a
    merge of 2 or more items always conflicts (`graph.fanout.join_merge_conflict`), and only after every item ran. It is
    refused readably at save in all three layers (`map_yaml.rs` `MAP_JOIN_MERGE`; Main `PIPELINE_MAP_FORM_UNSUPPORTED`;
    the Web offers append and keyed only). The shared `plan::land` is unchanged.
  - No saved document can hold `reduction` with `on_error`: on main (`0c41d5b65`) the Map YAML was
    `#[serde(deny_unknown_fields)]` with no `on_error` field
    (`services/elitea-worker-rust/src/agents/graph/map_yaml.rs:12-13`), so such a document never compiled.
- A Map item's delegated sign-in meets the same `credential_unavailable` gate as a Parallel member (M1).
- An empty source lands an empty collection and starts no child.

**Deliberately not ported:** the current platform's "lost, run again from scratch" behaviour (each item restores from its
own checkpoint instead); Map-only HITL restrictions (a Map item may now pause, per card); the old Map's non-pausing,
effect-free Agent-worker cohort (removed: Agent workers use Parallel's participant proof).

## Design in one paragraph

`FanoutSource` (`src/agents/graph/fanout_source.rs:29`) is a closed descriptor, Parallel or Map, that holds every
per-family rule: children (a Map's are read from state at activation), frozen inputs (`:185`, main's
`map.item-input.v1` digest), the activation's Map identity (`:167`), the output filter (`:221`), the v1 landing
(`:272`) and the empty short-circuit (`:239`). The adapter in `parallel.rs` reads only the descriptor. The Map identity
travels in the activation (`ParallelActivation.map`, `src/agents/graph/parallel.rs:118`, set only by the Map source at
`:151`), so the Postgres factory derives main's Map child id (`m1:`, field order unchanged) from the activation alone
(`src/state/postgres_checkpointer/parallel_children.rs:165,177`) and every scoped wrapper passes it through unchanged. The
shared branch factory gains the State Modifier family (`src/agents/graph/parallel_compiler.rs:968`) and the compiler
binds a Map as a fan-out node (`bind_map_node`, `:460`).

## Changed paths (by component)

- Worker: `src/agents/graph/{fanout_source,parallel,parallel_compiler,map_compiler,map_reduce,map_yaml,compiler}.rs`,
  `src/agents/session.rs`, `src/agents/pipeline.rs`, `src/agents/pipeline/{scope_receipts,scoped_applications}.rs`,
  `src/state/{mod,postgres_checkpointer}.rs`, `src/state/postgres_checkpointer/{parallel_children,application_children}.rs`.
  Deleted: `map_reduce_checkpoint.rs`, `map_turn.rs`, `map_authority.rs`, `node_events_map.rs`,
  `state/postgres_checkpointer/map_children.rs`, `pipeline/scope_receipts_map.rs`.
- agent-runtime: `libs/rust/agent-runtime/src/graph/yaml.rs` (`ParallelBranchDefinition::map_item`).
- Main: `internal/domain/pipelinelimits/limits.go` (`checkMapForms`, `:225`).
- Web: `features/pipelines/lib/graphMapAdmission.helpers.ts` (`mapPolicyIssues`, `:85`),
  `ui/settings/graph-extensions/{MapSettings,MapWorkerInputs}.tsx`, `lib/flow-editor/constants/runtimeContract.constants.ts`,
  `features/chat-messages/ui/chat-box/ApplicationAnswerThinking.tsx` (status grouping, `:238`), `shared/i18n/en.json`.
- Contract: `libs/proto/contracts/pipeline-node-admission-v1.json` (`map` admitted, drift record removed).

## Tests

| Layer | Tests | Result |
|---|---|---|
| Worker | full suite | 2669 passed at c6af819cb (no Worker change after it); PostgreSQL tests are ignored locally and run in CI. New: `map_fanout_tests.rs` (22), `parallel_map_identity_tests.rs` (3), `parallel_compiler_map_tests.rs` (3), `parallel_compiler_map_agent_tests.rs` (4), `map_yaml_tests.rs` (7). The Parallel golden (`parallel_cards_tests.rs` `shared_adapter_golden`) passes unchanged across the whole branch and the merge with main. |
| Worker, real PostgreSQL 18 | `fanout_lineage` (11: incl. the two Map tests ported to the adapter and the three production-factory tests, one per worker family) | green before and after the merge with main (throwaway database) |
| Main | `pipelinelimits` (`map_forms_test.go`, 3), `executioninterrupt` (`map_card_test.go`, 1), real PostgreSQL `TestAMapItemAuthorizeIsGatedToAnOptedInWorkerAndCarriesOnlyItsGrant` | green |
| Web | full suite on final head `52ff237a7` | node project: 1889 files, 18622 passed, 6 expected fails, 1 skip; storybook project: 69 files, 278 passed. New: `graphMapAdmission.v2.test.ts` (10 cases), `MapSettings.policy.test.tsx` (4), `ApplicationAnswerThinking.grouping.test.tsx` (6). |

Existing tests edited (each approved by the orchestrator): 12 struct literals gained `map: None` (mechanical); the
Parallel golden's `display` capture moved from Debug text to serialized bytes (digest recomputed before the merge,
unchanged after it); `'llm'` removed from the refused Map worker list; seven Web assertions that pinned "Map not
admitted" (commit 9); the E2E J16 Add-node menu list gained `Map` (also excluded from the saveable list, as
Parallel is) in `52ff237a7`.

The 6 expected fails (`it.fails`) are all on `origin/main`, in files this branch does not change (`git diff
origin/main HEAD` is empty for each); none is new here:

- `apps/elitea-web/src/features/pipelines/ui/settings/InputMappings/DataTypeValueField.test.tsx:146` (issue 1930)
- `apps/elitea-web/src/features/chat-messages/lib/canvasFileSource.test.ts:85` (#5846/#5940)
- `apps/elitea-web/src/features/chat-messages/lib/canvasFileSource.test.ts:92` (#5845)
- `apps/elitea-web/src/pages/toolkits/EditToolkit.test.tsx:492` (elitea_issues 5722)
- `apps/elitea-web/src/pages/credentials/CredentialForm.test.tsx:383` (elitea_issues 4864/5937)
- `apps/elitea-web/src/widgets/sidebar/__tests__/ProjectSwitcher.test.tsx:489` (elitea_issues 6561)

**Which Map tests use stubs, and whether the production path also covers them** (three defects so far were hidden
by stubs: the frame kind, concurrent saved-agent items with the wake's resume install, and the member event scope's 16 bound):

| Test file | What is stubbed | Production-path cover |
|---|---|---|
| `map_fanout_tests.rs` (most tests) | Item graphs and child checkpoints (`ItemGraphs`, `MemoryAuthority`) | Ordering, admission, bounds and join land: `fanout_lineage` real-PG production-factory tests (State Modifier, Agent, LLM items) and the fanpar case 1 run |
| `parallel_compiler_map_agent_tests.rs` | Application resolver (stub participant) | `postgres_map_saved_agent_items_run_concurrently_configured_and_all_pause` (3 concurrent items, park and wake), the 16- and 24-item no-leak tests and the Parallel same-agent test, all through the production assembler |
| `parallel_compiler_map_tests.rs`, `map_compiler_tests.rs` | Compile only, no run | Save and compile refusals: Main and Worker admission tests; the run is case 1 to 7 |
| `parallel_map_identity_tests.rs` | None (pure digest) | `fanout_lineage` B1 pins main's ids through PostgreSQL |
| `map_yaml_tests.rs` | None (parse and validate) | n/a |
| `a_map_of_more_than_sixteen_items_streams_map_member_frames` | Memory authority; the event channel and frames are real | fanpar case 1 (64 items) |

## Performance

- Admission and per-child cost are Parallel's: one batched activation and one batched receipt read for N items (two
  transactions, `src/state/postgres_checkpointer/parallel_children.rs` `prepare_children`), at most two parent rows per
  visit (`MAX_PARENT_ROWS_PER_VISIT`). Proven by the shared Parallel budget tests; no Map-specific path remains.
- The Map's digests are computed once per activation (`MapSource::new` for the config; `map_identity` and
  `frozen_inputs` for source and items, `src/agents/graph/map_reduce.rs:173`); bounded by `MAX_ITEM_BYTES` (512 KiB per
  item) and `MAX_PLAN_BYTES` (2 MiB per source).
- Admission bound: `a_map_admits_at_most_its_concurrency` (6 items, concurrency 2: peak 2).
- **Measured on fanpar** (production profile; Worker `map-f106bbe31`): a 64-item State Modifier Map at concurrency
  4 ran from the first admission to the join in 577 ms wall-clock, with 64 children admitted and 64 completed
  (Worker trace, 2026-11-05 14:26:33.610 to 14:26:34.187 UTC). The activation span was 16 ms busy and 561 ms idle.
- **One saved Agent, N concurrent invocations (like LangGraph `Send`).** Each Map item is its own invocation of the
  worker's saved Agent.
  - **Shared, behind `Arc` and never copied per item:** the built Agent (`BuiltApplication`: the lazy agent, its
    toolsets including MCP with credentials by reference, its model handle and limiter, its instructions and
    catalogs); the node's scope, prepare gate and call owner; the node's resume coordinator, keyed by invocation and
    call; and the tool presentation catalog. The catalog is now copy-on-write (`W/agents/events.rs`
    `ApplicationToolPresentationCatalog`, `Arc` plus `make_mut` at its two mutation sites).
  - **Per item:** a bounded event channel (`APPLICATION_EVENT_CHANNEL_CAPACITY`, 64 signals) with its receiver, a
    tool carrying the item's sender, the projection that holds them, and the item's own checkpoint thread (`m1:`)
    and input.
  - **Measured.** Per-item bind (`a_member_invocation_of_a_saved_agent_costs_a_small_constant`, 6,400 bound members
    held alive, resident set read with `ps`):
    - 8.1 KB per item, the same with or without the catalog. It was 13.4 KB before the catalog moved to
      copy-on-write, of which 5.4 KB was the one-entry catalog clone.
    - The rest is the channel's first slot block.
    - Bind time: 0.81 µs on average, 10 µs at worst, against the 30 ms per-child budget.
  - **Measured: memory.** 64-item saved-agent Map at concurrency 8 against concurrency 1, with an instant mock
    model (`measure_a_64_item_saved_agent_map_*`; ignored, run alone under `/usr/bin/time -l`; debug build, real
    PostgreSQL, 3 runs each). Peak resident memory was 111.1–112.4 MB at concurrency 8 against 108.9–111.8 MB at
    concurrency 1.
  - **Measured: concurrency.** 64 items with the model answering after a fixed 100 ms, on a multi-thread runtime
    (`measure_64_saved_agent_items_with_model_latency_at_concurrency_*`):
    - The model calls overlap 8-way. The first 32 items' calls finished within about 0.6 s, against about 3.2 s one
      at a time; 8 calls started within the first 46 ms.
    - Total wall time: 7.0–8.8 s at concurrency 8 against 10.4 s at concurrency 1.
    - The total is not 8× because each item's non-model cost grows with the items already completed: later items
      start 0.6–1.1 s apart, and the join takes about 2 s after the last call.
    - **Pinned by a span profile.** After every root event, the native runner re-reads the whole root session
      (`libs/rust/vendor/adk-runner/src/runner.rs:222-235`, called at `:1054`), which every native run enables
      (`W/agents/runtime.rs:1393`). Root `get` ×322 took 2.3, 6.2, 10.4 then 14.6 ms by quarter, 2.7 s in total,
      while appends and child checkpoint I/O stayed flat.
    - **Known gap, tracked by FU-287 (HIGH, right after Map, before W3-1).** That remaining O(n) root refresh per
      event is O(n²) per run, for every native run and not only Map. A correct fix changes shared session semantics:
      the candidates are an incremental refresh by a durable sequence, a version check before the full read, or a
      bounded tail.
    - **What Map itself delivers:** the 8-way model-call overlap. The first 32 items' calls finished in about 0.6 s
      against about 3.2 s one at a time.
  - **Bounded:** live items stay within `max_concurrency` (≤ 8, `a_map_admits_at_most_its_concurrency`), and model
    calls stay within the run's model limiter and child permits. 64 items are never 64 live model calls.

## Durability

- Each item's child thread is a pure function of the frozen activation (Map identity included) and the frozen origin:
  `derive_child_thread_id` (`src/state/postgres_checkpointer/parallel_children.rs:177`); main's `m1:` ids are kept
  byte-for-byte (`a_map_child_on_the_shared_adapter_keeps_mains_child_id` pins main's three ids).
- **Where the activation is persisted:** only inside the frozen occurrence, `FrozenOccurrence.activation`
  (`src/agents/graph/parallel_checkpoint.rs:45`), in the parent checkpoint's metadata under
  `elitea.graph.parallel.occurrence.v3`, read back only by the same adapter; a restore compares the whole activation,
  `map` included. The `map` field is present only for a Map (skipped when absent, so Parallel bytes are unchanged:
  `a_parallel_source_never_carries_the_map_identity`). Frame labels never include it.
- **Through the production child factory**, one test per worker family on real PostgreSQL (the code review found
  that a Map State Modifier or LLM worker was refused at activation by `ApplicationCheckpointers`, which every other
  Map test bypassed with a test double): `map_state_modifier_items_run_through_the_production_application_factory`,
  `map_agent_items_run_through_the_production_application_factory`,
  `map_llm_items_run_through_the_production_application_factory`
  (`src/state/postgres_checkpointer_tests/fanout_lineage.rs`); the admitted paths are derived from the pipeline
  definition exactly as the session derives them. The fix: Map-owned LLM workers are admitted fan-out paths
  (`src/agents/graph/compiler.rs` `parallel_llm_branch_ids`), and a Map item whose worker owns no subgraph gets the
  empty family, only when the activation carries the Map identity (`src/state/postgres_checkpointer/application_children.rs`
  `for_child`).
- Completed items are never re-run after a takeover by a new execution, and lease loss mid-item records no failure
  and a later claim completes: `map_items_completed_before_a_takeover_by_a_new_execution_do_not_run_again`,
  `map_lease_loss_mid_item_records_no_stop_and_a_later_claim_completes` (real PostgreSQL).

## Resilience

- Every bound is the shared one and refused typed before any child: items over `max_items` (≤ 64), concurrency outside
  1..=8, an item over 512 KiB, a structure over the depth guard, a non-list source (`an_invalid_or_oversized_source_is_refused_before_any_child`,
  `a_map_over_its_configuration_limits_is_refused`); an item result over 512 KiB fails that item
  (`an_item_result_over_its_bound_fails_that_item`); the joined result over 8 MiB fails typed
  (`a_joined_result_over_its_bound_fails_typed`).
- On Error: v1 stops on the first failed item and names it (`a_failed_item_under_v1_stop_fails_the_map_by_its_index`);
  v2 continue lands typed failed entries (Parallel's join).
- Cancellation, deadline and lease loss are the shared node's (Parallel tests cited in the coverage table).
- A join that cannot land is refused at save time, by name (`a_map_join_is_checked_against_its_destination_reducer_at_save_time`).
- **Concurrent saved-agent items (found by the fanpar browser pass, fixed in this PR).**
  - **Root cause.** A saved Agent node had one nested-event channel.
    - `PipelineApplicationNodeRuntime::execute` takes the channel's receiver for the whole child run, and
      `ApplicationEventReceiver::take` refuses a second taker.
    - `rebind_participant` gave each fan-out member a new event scope over the node's shared tool and receiver.
    - A Map runs one node as many concurrent items, so every item that started while another held the receiver
      failed at once (`graph.node_execution_failed`). On fanpar, items 0 and 2 of 3 failed.
    - On a wake, `ApplicationResumeCoordinator::install_root` refused a second item's resume.
    - A saved-agent member past ordinal 16 was refused `graph.invalid` by the member event scope's Parallel bound.
  - **Fix.**
    - Each member is its own invocation. `ScopedAgentMember::bind` (`W/agents/application_tools.rs:4667`) and
      `PipelineApplicationNodeRuntime::for_member` (`W/agents/pipeline/scoped_runtime.rs:180`) are called from the
      ScopedAgent arm of `rebind_participant` (`W/agents/pipeline/parallel_application_resolver.rs:165`).
    - The shared coordinator accepts each item's own call resume and still refuses a second resume for the same
      call (`W/agents/application_tools.rs:611`).
    - The member event scope is bounded by the fan-out maximum
      (`W/agents/graph/node_events_parallel.rs:62`, `MAX_FANOUT_MEMBERS = MAX_FANOUT_CHILDREN`).
  - **Nested saved applications.**
    - A saved Agent's nested saved agents and pipelines are built once with the node's channel.
    - Inside a member, every application send goes to the member's own channel. A task-local
      `MEMBER_EVENT_SENDER` (`W/agents/application_tools.rs:293`) is set around the member's child run
      (`W/agents/pipeline/scoped_runtime.rs:324`) and read first by every send site (`routed_sender`,
      `W/agents/application_tools.rs:298`; the nested saved pipeline tool in `W/agents/application_pipeline.rs`).
    - It is carried across the one spawn on that path, `DrivenTask` (`W/agents/driven.rs:84`). The vendored agent
      runs tools inline.
    - **Proof:** `postgres_map_items_of_a_saved_agent_with_a_nested_saved_agent_keep_their_own_events`. 3 items whose
      agent calls a nested saved agent all complete with their own answers, and every nested answer frame sits
      under its own item's `sibling_ordinal`. Without the routing, no nested frame reaches the run.
    - Parallel drains each branch node's own channel, which its nested tools share, so it never needed this.
  - **No silent hang.** A send into a full, undrained channel is ended by the node's deadline, typed
    `deadline_exceeded` (`items_blocked_on_a_full_channel_end_typed_at_the_deadline`).
  - **Parallel isn't affected.** Its branches own distinct nodes. On fanpar, a Parallel with the same saved agent in
    two branches paused both (probe pipeline 26); with per-member invocation it still does
    (`postgres_parallel_branches_of_the_same_saved_agent_both_pause_concurrently`). Saved-pipeline workers already
    had a channel per member: 3 concurrent items paused, probe pipeline 27.
  - **Proof** (production assembler, real PostgreSQL):
    - `postgres_map_saved_agent_items_run_concurrently_configured_and_all_pause`: 3 concurrent items, 3 cards and a
      park. The cards are answered last first, and the wake resumes each with its own answer. Without the
      per-member bind there are 2 cards; without the coordinator change the wake is refused.
    - `postgres_a_sixteen_item_saved_agent_map_completes_without_cross_item_state` and
      `postgres_a_saved_agent_map_of_more_than_sixteen_items_completes_every_item` (24 items), both at concurrency 8:
      each model request carries exactly its own item's input, and each entry holds its own answer. Without the
      ordinal fix, items 16 and up fail `graph.invalid`.
- **Frame kind per family (found by the fanpar browser pass, fixed in this PR).**
  - Each member frame carries its family's kind. `W/agents/graph/parallel_frames.rs:71` sets `map` when the activation
    has a Map identity and `parallel` otherwise. The frame check caps members per kind: `parallel` at 16 and `map`
    at 64 (`W/agents/graph/fanout_frames.rs:310-313`), matching Main's `fanout-member.schema.json`.
  - Before the fix, every Map member frame said `parallel`, so a Map of 17 or more items failed at activation with
    `graph.parallel.frame_undelivered`. That showed in the chat as INTERNAL. On fanpar, 2 and 16 items ran and
    17, 32 and 64 failed.
  - **Why the earlier tests missed it:** frames exist only when the run binds a node-event channel. Every Map test
    compiled without one, and the real-PostgreSQL tests used 2–3 items.
  - **Proof:** `a_map_of_more_than_sixteen_items_streams_map_member_frames` runs a 17-item Map with the channel
    bound. It fails without the fix with `frame_undelivered` and passes with it.
  - Parallel frames keep `parallel`: the shared-adapter golden test is green and unchanged.
  - **Web:** it parses any member kind with ordinals up to 64 (`fanoutFrames.ts:40,56`).
    `ApplicationAnswerThinking.mapFrames.test.tsx` streams 17 and 64 `map` member frames through the reducer into
    the status groups.

## Security

- **Authorization / identity:** an Agent worker is admitted only by Parallel's frozen participant proof
  (`a_map_agent_worker_without_a_proven_participant_is_refused_readably`). Map item cards go through the same card
  contract and decide path (`TestParseCardAcceptsAMapItemCardOnTheSharedContract`); a Map member is an index 0..63
  (`libs/jsonschema/runtime/v1/fanout-member.schema.json:83`).
- **Credentials (M1):** a Map item never resumes "authorized" without its credential
  (`a_map_item_authorized_without_its_credential_ends_typed`); with it, only that item resumes and the reference reaches
  no checkpoint (`a_map_item_authorized_with_its_credential_resumes_only_that_item`); the wake is claimable only by an
  opted-in worker and carries only the actor's grant for that toolkit and resource (real PostgreSQL,
  `TestAMapItemAuthorizeIsGatedToAnOptedInWorkerAndCarriesOnlyItsGrant`). Presentation only to the toolset of its
  resource is the shared MCP rule (`a_projected_token_reaches_only_the_toolset_of_its_resource`, agent-runtime
  `mcp_tests.rs:2204`).
- **Untrusted input:** the source and broadcast values are bounded and depth-guarded before any child; strict YAML
  (`deny_unknown_fields`); the save-time checks refuse unsupported worker forms with messages that echo only bounded,
  printable ids.
- **No user code outside the sandbox:** a Code worker, or a saved worker pipeline holding Code, is refused by name.
- Limits are unchanged and shared: 16 open cards per response (`MaxOpenInterrupts`,
  `services/elitea-main/internal/domain/executioninterrupt/limits.go:13`), 16 wake references
  (`configurations_materializer.go:310`).
- **Pending gate items:** `/security-review` on the branch and the browser pass (below).

## Recovery rows (replatform-delivery-gate §2)

A Map item is a fan-out child of the shared adapter, so the Parallel rows in `docs/recovery-guarantees.md` apply to it
by construction (the same node, runtime, receipts, ledger and recovery frontier). Map-specific proof:

| Component × phase | Class | Proof |
|---|---|---|
| Worker × fan-out child (Map item), lost mid-run, new execution takes over | R (completed items never re-run) | `map_items_completed_before_a_takeover_by_a_new_execution_do_not_run_again` (real PostgreSQL) |
| Worker × Map item, lease lost mid-item | I (no recorded failure; a later claim completes once) | `map_lease_loss_mid_item_records_no_stop_and_a_later_claim_completes` (real PostgreSQL) |
| Worker × Map item HITL pause and decision | R (one card per item, each resumes only its item) | `a_map_agent_worker_pauses_per_item_and_each_card_resumes_only_its_item` |
| Worker × Map item delegated sign-in | F typed without the credential; R with it | `a_map_item_authorized_without_its_credential_ends_typed`, `a_map_item_authorized_with_its_credential_resumes_only_that_item` |
| Main × Map item card decide and wake | I (retry-later for a non-opted-in worker) | `TestAMapItemAuthorizeIsGatedToAnOptedInWorkerAndCarriesOnlyItsGrant` (real PostgreSQL) |
| Worker × Map in a saved child pipeline | R as FU-249 (the run's fan-out binding, child authority passes the Map identity) | `a_saved_child_pipeline_with_a_map_binds_through_the_runs_fanout`; recovery leaf `a_recovery_leaf_may_be_an_unowned_map_but_not_its_worker` |

**Gaps (not L, stated):** mid-model and in-flight-tool crash of a Map item are proven for the adapter on the Parallel
path, not with a Map-specific two-process PostgreSQL test; the browser pass has not run yet.

## Browser evidence

**Pending.** The Map browser pass runs on the shared stack `fanpar` after it moves to `main` (snapshot first, main's
migrations, then the Map images), per the plan. Until then this mapping claims no browser evidence.

## Fixtures

All test fixtures are code (in-memory stores and a throwaway PostgreSQL 18 container); none were created through the UI.

## Deleted tests and their coverage

### B2: the old Map scheduler and its tests

New tests: `MF` = `agents/graph/map_fanout_tests.rs` (13), `PM` = `parallel_compiler_map_tests.rs` (B2a, 3),
`ID` = `parallel_map_identity_tests.rs` (B1, 3). `PG` = `postgres_checkpointer_tests/fanout_lineage.rs`, whose two Map
tests were **ported** to the shared adapter (same names, same scenarios, run green on real PostgreSQL 18).
`P:` = an existing Parallel test of the same shared node/runner that the Map now runs on.

#### `map_reduce_tests.rs` (31 deleted, file removed)

| Deleted test | Covered by |
|---|---|
| eight_items_refill_four_slots_without_batch_barrier | MF a_map_admits_at_most_its_concurrency; P: admission_bound_is_structural_and_all_started_branches_drain |
| reverse_completion_collects_original_order_and_preserves_structured_values | MF map_items_land_in_source_order_when_they_finish_in_reverse; MF a_v1_map_lands_index_and_outputs_through_its_key_reducer (structured values) |
| completed_item_receipts_survive_parent_loss_without_worker_replay | PG map_items_completed_before_a_takeover_by_a_new_execution_do_not_run_again |
| failed_item_drains_admitted_items_and_keeps_successful_receipts | MF a_failed_item_under_v1_stop_fails_the_map_by_its_index; P: failure_drains_a_slow_inflight_sibling_without_admitting_pending_work |
| unexpected_pause_is_typed_non_success_and_does_not_dispatch_on_replay | **Removed by design:** a Map item may now pause (per-interrupt HITL on the shared adapter, plan D-decisions); pause handling is the adapter's (P: the_parked_keys_are_minted_from_the_frozen_child_and_its_pause_checkpoint, child_pause_and_completion_survive_loss_before_parent_pause_save) |
| blocked_terminal_stays_non_success | P: a_blocked_branch_is_a_typed_failure_and_the_adk_parent_cannot_report_success (same `CompilerParallelBranches::project_result` Blocked path) |
| source_change_at_original_activation_fails_without_new_children | P: failed_branch_is_stable_and_changed_input_is_refused; the restored occurrence compares the whole activation, Map identity (source digest) included (`parallel_checkpoint.rs:125`) |
| collector_returns_one_delta_and_parent_reducer_runs_once | MF a_v1_map_lands_index_and_outputs_through_its_key_reducer; MF a_compiled_map_runs_its_state_modifier_worker_on_the_adapter |
| empty_list_emits_empty_collection_without_children | **Gap until commit 6** (PLAN §3 row 6 "Map short-circuit for 0 items"); today the shared runner refuses 0 children typed. Map is not admitted until then. |
| candidate_validation_failure_keeps_parent_state_and_committed_children | Landing is the typed join: `plan::land` refuses a type mismatch (`graph.fanout.join_type_mismatch`); a failed node writes no update (MF a_failed_item_under_v1_stop…); save-time check lands in commit 5 (`JoinMode::check`) |
| foreign_outputs_and_worker_routes_are_non_success | MF map_items_land_in_source_order… (a worker output the Map does not select is not landed); PM a_map_state_modifier_item_lands_its_typed_outputs (wrong type refused); routes: kept map_yaml_rejects_ownership_routes_pauses_and_local_global_collisions |
| invalid_sources_and_resource_limits_fail_before_dispatch | MF an_invalid_or_oversized_source_is_refused_before_any_child; MF a_map_over_its_configuration_limits_is_refused |
| collisions_reserved_controls_and_duplicate_channels_are_rejected | kept map_yaml_rejects_ownership_routes_pauses_and_local_global_collisions; `MapDefinition::validate` kept unchanged |
| duplicate_items_step_worker_and_sequence_binding_are_distinct | MF duplicate_items_get_distinct_child_threads; ID a_map_child_on_the_shared_adapter_keeps_mains_child_id (step, worker, source, index bound) |
| compiler_pausing_admission_fails_closed | **Removed by design:** the non-pausing/effect-free Map cohort is gone (PLAN §1; Agent workers use Parallel's participant proof) |
| child_scope_collision_fails_before_worker_invocation | ID a_map_activation_refuses_a_child_that_is_not_its_item |
| atomic_appender_rejects_stale_parent_and_preserves_exact_replay | P: freeze_rejects_a_competing_parent_before_any_child_is_admitted (same `ParallelOccurrenceCheckpointer`) |
| real_parent_adk_transition_applies_once_and_terminal_replay_skips_map | MF a_compiled_map_runs_its_state_modifier_worker_on_the_adapter; P: immutable_pre_freeze_replay_stays_unchanged_and_does_not_replace_latest |
| collected_memory_limit_stops_admission_and_preserves_parent_state | MF a_joined_result_over_its_bound_fails_typed |
| incompatible_reducer_fails_before_any_item_dispatch | **Closed in commit 5:** MF a_map_join_is_checked_against_its_destination_reducer_at_save_time (save time, before any item); MF a_v2_map_appends_through_its_declared_append_reducer |
| adversarial_compact_source_and_broadcast_values_fail_structure_guard | MF an_invalid_or_oversized_source_is_refused_before_any_child (nested depth 60) |
| adversarial_projected_result_fails_before_collection_and_keeps_parent_state | MF an_item_result_over_its_bound_fails_that_item; PM a_map_state_modifier_item_lands_its_typed_outputs |
| adversarial_worker_updates_interrupts_and_events_fail_before_adk_capture | Shared adapter: child receipts and strict channels (P: a_pause_receipt_cannot_attach_to_an_advanced_parent_frontier, sixteen_open_cards_park_and_a_seventeenth_fails_typed) |
| adversarial_runtime_controls_fail_before_worker_dispatch | kept map_yaml_rejects_ownership… (reserved keys); P: applying_decisions_writes_no_parent_row_and_changed_business_state_is_refused |
| adversarial_checkpoint_metadata_fails_before_store_clone_or_serialization | kept map_checkpoint_json_roundtrip_reserves_codec_envelope_depth; the occurrence is Parallel's, bounded there |
| a_legacy_map_occurrence_without_frozen_child_identity_is_refused_by_type | **Removed by design:** the Map's own occurrence format is no longer written or read; a Map occurrence is Parallel's (P: a_legacy_occurrence_without_frozen_child_identity_is_refused_by_type). No Map occurrence exists in production (gate was off). |
| a_lease_lost_item_is_a_control_stop_and_records_nothing | PG map_lease_loss_mid_item_records_no_stop_and_a_later_claim_completes; P: a_lease_lost_branch_is_a_control_stop_not_a_recorded_failure |
| a_lease_lost_item_stops_its_admitted_siblings | PG map_lease_loss_mid_item… (no later item admitted: calls == [0]) |
| deadline_stops_pending_items_at_the_deadline_and_is_still_recorded | P: deadline_stops_admission_and_owned_future_cleanup_is_bounded (same `DurableParallelNode` deadline) |
| original_deadline_stops_and_drops_owned_effectfree_futures_without_replay | P: deadline_stops_non_cooperative_branches_at_the_deadline_plus_cleanup |
| latch_cancel_stops_pending_items_promptly_without_recording_a_stop | P: latch_cancel_stops_inflight_branches_within_the_cleanup_bound_and_admits_nothing_new |

#### `map_compiler_tests.rs` (9 deleted, 6 kept)

| Deleted test | Covered by |
|---|---|
| map_production_binding_and_missing_authority_fail_before_dispatch | MF a_map_without_its_fanout_binding_is_refused_readably |
| map_foreign_parent_pointer_refuses_before_child_activation | P: compiler_refuses_missing_or_different_underlying_parent_authority (Map now binds through the same `bind_parallel_authority`) |
| map_actual_adk_graph_collects_unknown_items_and_terminal_replay_once | MF a_compiled_map_runs_its_state_modifier_worker_on_the_adapter |
| map_expired_original_deadline_preserves_parent_business_state | P: deadline_stops_admission_and_owned_future_cleanup_is_bounded |
| map_nonpausing_cohort_refuses_static_pause_and_effectful_saved_definitions | **Removed by design** (non-pausing cohort removed, PLAN §1) |
| map_typed_turn_reclaim_retains_capabilities_and_exact_original_execution | P: parallel_binds_through_the_fanout_turn_authority; Map composition through `FanoutTurnAuthority` is commit 6 |
| map_saved_agent_refuses_alias_and_zero_fingerprint_before_dispatch | MF a_map_agent_worker_without_a_proven_participant_is_refused_readably (Parallel's proof refuses a zero/unproved fingerprint) |
| map_in_place_collection_and_overlapping_broadcast_use_original_frozen_values | ID a_map_child_on_the_shared_adapter_keeps_mains_child_id (frozen item inputs equal main's); PM a_map_state_modifier_item_reads_only_its_declared_inputs |
| map_fresh_turn_keeps_exact_physical_parent_cas_and_old_receipts | Commit 6 (Map through `FanoutTurnAuthority`); P: fanout_turn_tests |

Kept: map_yaml_child_channels_are_opaque_and_not_parent_descriptors, map_yaml_rejects_ownership_routes_pauses_and_local_global_collisions,
map_checkpoint_json_roundtrip_reserves_codec_envelope_depth, map_keeps_explicit_root_declaration_order_and_selected_terminal_key_seam,
map_saved_agent_rejects_deep_fixed_value_before_definition_hashing, map_worker_kind_refusal_is_typed_and_names_the_node.

#### `map_item_scope_tests.rs` (3 deleted, file removed)

The file had no module declaration: these tests were never compiled or run. Map item event scope is now the Parallel member
scope (`node_events_parallel_tests.rs`). map_event_scope_requires_exact_original_parent_not_captured_label,
map_event_scope_preserves_exact_descendant_call_lineage, map_item_completion_receipt_belongs_only_to_root_checkpoint.

**Counts:** deleted 43 (31 + 9 + 3; 40 of them compiled), added 13 (MF), ported 2 (PG), kept 6.
Required behaviours: ordering (MF ×2), limits (MF ×5: items, concurrency, item bytes, child result, joined), v1 stop (MF),
restore without re-run (PG), lineage/identity (MF, ID ×3), readable refusals (MF ×3, kept kind refusal) — each covered.

### Commit 6: the B2 left-overs

| Deleted test | Covered by |
|---|---|
| scope_receipts_map_tests: map_freeze_and_decision_appends_retain_original_receipts_and_immutable_replays | The Map's scope receipts are Parallel's (`PipelineGraphReceiptAuthority`, `scope_receipts_parallel_tests.rs`), which a Map now binds through; MF a_compiled_map_runs_its_state_modifier_worker_on_the_adapter |
| scope_receipts_map_tests: map_atomic_append_refuses_receipt_invention_replacement_and_unproved_parent | `scope_receipts_parallel_tests.rs` (same authority type for both families) |
| scope_receipts_map_tests: map_real_graph_call_revision_requires_exact_frontier_on_atomic_path | `scope_receipts_parallel_tests.rs` |
| scope_receipts_map_tests: map_ordinary_save_and_child_factory_keep_the_same_underlying_authority | `scope_receipts_parallel_tests.rs`; P: compiler_refuses_missing_or_different_underlying_parent_authority |
| scope_receipts_map_tests: map_receipt_overlay_rejects_deep_metadata_before_store_mutation | `scope_receipts_parallel_tests.rs` (the Parallel receipt overlay) |
| application_children map_catalog_tests: map_state_modifier_has_no_synthetic_application_authority | **Removed by design:** the Map-only catalog lookup (`for_map_worker`) is gone; a Map item resolves its worker through the shared branch factory (PM a_map_state_modifier_item_resolves_to_its_worker_with_mains_worker_digest) |
| application_children map_catalog_tests: map_application_requires_exact_original_owned_family | `application_children` branch_catalog_selects_only_exact_admitted_parent_and_owned_node (kept; the same catalog serves Map Agent workers); MF a_map_agent_worker_without_a_proven_participant_is_refused_readably |
| map_compiler_tests: map_checkpoint_json_roundtrip_reserves_codec_envelope_depth | **Removed by design:** the Map's own checkpoint codec boundary (`validate_checkpoint_boundary`) is gone; a Map's checkpoints are the Parallel occurrence and child checkpoints, bounded there |

Changed (own branch test, not from main): ID a_map_child_on_the_shared_adapter_keeps_mains_child_id now pins main's three
`m1:` ids (captured from main's `map_child_thread_id` immediately before it was deleted) instead of calling it.

Counts (commit 6): deleted 8 (5 + 2 + 1), added 4 (MF: empty Map, siblings, saved child pipeline, recovery leaf),
changed 1 (ID pinned vectors).

## Code review (`/code-review` high on the full diff)

Ten findings, triaged in `handoffs/point5-wave1-20261008/map/REVIEW-CODE-20261104.md`: the production-factory refusal,
the LLM-worker mapping check, the Map failure wording (`graph.map.item_failed`), the repeated source reads and per-item
worker checks, the `join: null` drift between Main and the Worker, and the Web grouping scope and order are fixed with
tests; `merge` and `reduction` + `on_error` are refused (above). Kept, with reasons: the shared pause text
`Parallel branches paused.` (a protocol sentinel the Web matches to hide the park summary,
`apps/elitea-web/src/features/chat-messages/lib/hitlInterrupts.ts:240`), and the constant-size double configuration
check on the node (no I/O; Parallel tests rely on the order).

## Follow-ups

- Browser pass on `fanpar` (moves `fanpar` to `main` first) and a 64-item load measurement.
- A Map-specific two-process PostgreSQL crash test for mid-model and in-flight tool (the adapter's classes).
- The Wave 3 forms: Code and direct Router workers, saved worker pipelines holding Code (W3-1, W3-2).
- `parallel_in_child_pipeline` is now also the refusal for a Map in a fan-out member's saved pipeline; a Map-specific
  code could be added in the polish pass.
