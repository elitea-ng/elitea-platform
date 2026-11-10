# Code as a direct Map worker and Parallel branch (W3-2a), 2026-11-10

Plan and review: `handoffs/point5-wave1-20261008/w3-2/PLAN.md` and `REVIEW-PLAN.md`. W3-2a is §2.1 (the Code arms
only), §2.2, §2.3, §2.4 and §2.5. Saved pipelines with Code, Router inside them and the `ContainsCode` deletion are
W3-2b. §2.6 (per-thread operator recovery) is **not** in this PR: it is FU-310 (exit work), see "Deviations" and the
F row below. Stacked on W3-1 (#1879).

Decisions applied: Q2 the sum of a child's Code timeouts (a direct Code child is its one timeout; the helper sums, for
2b), Q3 the smallest `code_slots` among the child's targets, Q4 a second check after preparation, Q5 a task-local
marker, Q7 a direct Router stays refused. Q6 does not arise in 2a (no child suspends for an operator).

## Business behaviour

Taken from the current platform: a Code node is a pipeline step whose result lands in declared state. The current
platform has no fan-out of Code; a Map or Parallel of Code is new behaviour, built on the replatform's fan-out adapter.

Not ported, on purpose: nothing of the current platform's failure behaviour. A Code child that cannot be proven safe
ends typed and is never re-run blindly.

## Changed paths

- `libs/rust/agent-runtime/src/graph/fanout/admission.rs`: `code_attempt_scope` (R1), `CodeRefusalCause::{Preparation,
  NestedSlot}`, `StartedSlotRefusal`, `recheck_after_preparation` (N3).
- `libs/go/faults/registry/worker.json` (+ generated `codes_gen.go`, `docs/errors/README.md`): `pipeline.code.nested_slot`.
- Worker `src/agents/graph/`: `parallel_compiler.rs`, `map_compiler.rs`, `compiler.rs` (Router refusal), new
  `fanout_code_child.rs`, `parallel.rs`, `parallel_checkpoint.rs`, `code_runtime.rs`, `code_remote.rs`,
  `code_attempt_remote.rs`, `code_state.rs`, `mod.rs`; `src/agents/pipeline/{saved_child_http,saved_child_scope_provider}.rs`;
  `src/transport/code_intent_content.rs`; tests listed below.
- `libs/proto/contracts/pipeline-fanout-child-families-v1.json` (new, next to `pipeline-node-admission-v1.json`).
- Main: `internal/domain/pipelinelimits/limits.go`; `internal/infra/storage/runtime_fanout_code_child.go` (new),
  `runtime_saved_code_input_policy.go`; `internal/infra/db/repos/code_source_definition.go`, `code_sandbox_intent.go`.
- Web: `src/features/pipelines/lib/{graphExtensions.types,graphMapAdmission.helpers,graphParallelAdmission.helpers,graphParallel.helpers}.ts`,
  `ui/settings/parallel/ParallelBranches.tsx`, `src/shared/i18n/en.json` (one new key).
- `docs/upgrading/2026-11-10-0-code-fanout-children.md` (number assigned at PR time).

## Design as built

- **Families (§2.1).** Map admits `Code` as its worker (`map_compiler.rs:70`); a fixed Parallel admits it as a branch
  (`parallel_compiler.rs:725`, bound at `:385` and `:490` as `OwnedBranch::Code`). A direct Router keeps the code
  `graph.fanout.child_kind_unsupported` with its own text and cause detail `fanout_child_router`
  (`FanoutChildRefusal::Router`, `compiler.rs`; used at `map_compiler.rs:75`, `parallel_compiler.rs:729`). A Code child
  needs the run's sandbox runtime (`require_code_runtime`, `parallel_compiler.rs:603`). Main: `parallelBranchTypes`
  and `mapWorkerTypes` (`limits.go:182`, `:242`), Router text `routerChildText` (`:186`). Web: one
  `FANOUT_CHILD_FAMILIES` (`graphExtensions.types.ts:14`) used by both admission helpers and the branch picker.
- **A Code child (§2.2).** `compile_code_branch` (`parallel_compiler.rs:1034`) compiles the owned Code node alone under
  the child's thread, its definition pinned to the compiled definition as root Code is (`code_child`, `:623`). With
  node recovery on, the node is a `RecoverableNode` over the branch family's journal (`execution.node_recovery()`,
  which `ApplicationCheckpointers` lends to every branch in production). A child thread is not the run root, so its
  journal is visit-scoped (`node_attempts.rs` `in_child` rule): `project_input` stamps a visit derived from the branch
  key, the node and the frozen input (`code_child_visit`, `fanout_code_child.rs:94`). N items therefore get N distinct
  activations and journals. A Map worker reads its item as any JSON (read-only, never written: `MAP_ITEM_INPUT_KIND`,
  `code_state.rs:20`) and its index as `int`.
- **Owner scope on the visit (C1).** The child's runtime is bound to a `FanoutCodeChildScopeProvider`
  (`fanout_code_child.rs:37`), which serves only that child's own thread and node (`:75`). The visit carries
  `{schema: elitea.runtime.fanout-code-child-scope.v1, kind, node_id, owned_node_id, ordinal, item_count,
  parent_thread_id, child_thread_id}` (`saved_child_http.rs`). Main resolves it inside the visit's (or intent's, or the
  Supervisor read's) own transaction (`code_source_definition.go:50`, `:101`): the parent thread must be this run's
  root thread, the visit must run on the scope's child thread in the family's `m1:`/`p1:` form, its node must be the
  owned node, and the run's frozen root source must declare that fan-out node owning that Code node at that ordinal
  inside the frozen family (`FanoutCodeChildOwner`, `runtime_fanout_code_child.go:29`: Map worker with
  `1 ≤ item_count ≤ max_items`, `ordinal < item_count`; Parallel branch at `ordinal` with `item_count` = branch count;
  the owned node is `code`; anything else, including a forged family, fails closed). A Map worker's input policy also
  admits its item and index (`OriginalMapItemCodeInputPolicy`, `runtime_saved_code_input_policy.go:35`, chosen by
  `originalVisitInputPolicy`, `code_sandbox_intent.go:318`).
- **Activation admission (§2.3, D3).** `ParallelOccurrenceCheckpointer::freeze` takes an `admit` closure
  (`parallel_checkpoint.rs:111`) and calls it only on a first freeze, after the "already frozen" return and before the
  occurrence row is appended (`:139`). `AdkParallelBranchRuntime::prepare_source` passes
  `admit_activation(branches, max_concurrency, node_id)` (`parallel.rs:703`). `CompilerParallelBranches` counts only
  Code children (`parallel_compiler.rs:1140`) and calls `CodeSandboxRuntime::admit_fanout`, which `RemoteCodeRuntime`
  implements (`code_remote.rs:716`): per child the Code time is the sum of its Code nodes' timeouts and the slots the
  smallest among its targets (`child_code_window`, `:377`, Q2/Q3); over the family, the longest child time and the
  smallest slots; then `admit_fanout_window` against the runtime's own run deadline. A refusal is one WARN fault line
  `graph.fanout.deadline_insufficient` with `node_id`, `items`, `concurrency`, `slots`, `required_ms`, `remaining_ms`,
  `support_ref`, and the run ends with `runtime.fanout_deadline_insufficient` (`prepare_reporting`, `parallel.rs:2103`).
  Nothing is frozen, activated or started. A restored occurrence is never re-admitted (C2).
- **R1 (§2.4).** `code_attempt_scope` (`admission.rs:291`) wraps each Code attempt future only: the four call sites in
  `code_remote.rs:790-821` (journaled attempt, tool, raw, raw node). `CodeSlots::admit_fresh`/`admit_started` refuse at
  once (`NestedSlot`, `:408`, `:443`) inside an attempt that already holds a slot; the scope sets the marker after the
  first acquire (`:426`, `:450`). A nested attempt in the same task keeps the outer marker. The refusal is one ERROR
  fault line `pipeline.code.nested_slot` (`log_nested_slot`, `code_remote.rs:400`) and an internal failure, never the
  deadline text. **The marker does not follow `tokio::spawn`**: a nested acquire from a spawned task is not caught.
  The backstop is a structural test: no fan-out activation or child source takes a Code slot.
- **N3 (§2.5).** After preparation and hydration, immediately before `mark_submission_started` (the durable marker
  that a submission may reach the Supervisor), a fresh attempt is re-admitted (`code_attempt_remote.rs:380`,
  `recheck_after_preparation`, `code_remote.rs:351`, `admission.rs:315`): `remaining ≥ timeout + 90 s`. A refusal is
  the W3-1 refusal with `cause: preparation`, the hydrated job is released and the slot dropped. It is skipped when
  `prior_dispatch` is true (C2). Main's intent is already finalized at that point (hydration needs it); an intent with
  no submission is inert.
- **D5, a recovered intent without a submission marker.** `prior_dispatch` is read from the dispatch journal's
  submission marker, not from Main's intent. An attempt recovered after a crash between the intent and the marker has
  no marker, counts as fresh, and may be refused by N3. That is correct: an intent without a submission is inert (the
  Supervisor never received the job), so refusing it starts nothing and repeats nothing.
- **D1, an item without owner proof.** A Code child whose attempt may have started and whose owner proof is missing
  (the journal's operator decision inside a visit-scoped child) ends with `pipeline.node_recovery.child_effect_unknown`
  (registered, `worker.json`). `CodeBranchNode` (`fanout_code_child.rs:150`) writes one WARN fault line with the code,
  `node_id` and the item's `support_ref` (the one its failed join entry carries, `support_ref_for(child thread)`) and
  fails the member with that code; `graph_error_code` keeps it (`parallel.rs:2660`) and the join entry carries its
  readable sentence (`CHILD_EFFECT_UNKNOWN_MESSAGE`, `parallel.rs:2426`). The journal keeps the stop, so the item is
  never run again; the other items keep their results (On Error `continue` lands them, `stop` ends the run typed with
  the completed children's durable receipts kept). Where owner proof exists the item is reconciled from it (C), and an
  ordinary Worker or Supervisor loss is R/I; see the real-PG proofs.
- **Canonical visit.** `code_child_visit` hashes the input through a sorted map (`fanout_code_child.rs:101`). The first
  version hashed the `State` hash map directly, whose order differs per process; the Worker-replacement real-PG test
  found that a restored item derived another visit and the replacement refused the frozen occurrence
  (`graph.parallel.stale_activation`). Fixed in `61580c376`; `a_code_child_visit_is_canonical_over_its_input_order`.
- **N1.** `CodeNode` records its registered failure code into a slot (`with_failure_slot`, `code_runtime.rs:277`);
  `CodeBranchNode` (`fanout_code_child.rs:120`) names a Code deadline refusal by that code, and `graph_error_code`
  keeps it as the member's failure code (`parallel.rs:2650`, restored receipts accept it).

## Tests

| Where | Tests | Result |
|---|---|---|
| `admission.rs` | `a_nested_acquire_inside_a_held_attempt_is_refused_at_once` (paused clock, zero elapsed, fresh and started, nested scope), `sibling_attempts_in_one_task_wait_for_each_other_never_nested` (slots 1), `the_marker_does_not_follow_a_spawned_task`, `the_post_preparation_check_admits_exactly_required_and_refuses_one_ms_less`; W3-1's `a_started_attempt_takes_a_slot_without_the_deadline_check` now destructures `StartedSlotRefusal::Expired` (assertion unchanged) | agent-runtime 1829 passed, 0 failed, 1 ignored |
| `code_slots_tests.rs` (D3 on `RemoteCodeRuntime`) | `a_fanout_of_64_items_on_one_slot_is_admitted_and_65_refused`, `a_fanout_is_admitted_at_exactly_required_and_refused_one_ms_less`, `mixed_languages_use_the_smallest_slots_and_a_child_sums_its_code_time` (Q2, Q3, no profile), `a_fanout_refusal_is_one_fault_line_with_its_numbers`, `no_fanout_activation_or_child_path_takes_a_code_slot` (R1 backstop) | pass |
| `fanout_code_child_tests.rs` (compiled graph on the in-memory fan-out authority) | `a_map_runs_its_code_worker_once_per_item`, `a_parallel_runs_its_code_branches`, `a_refused_code_fanout_starts_no_item_and_ends_typed`, `a_map_without_code_children_is_not_admitted_against_code_time`, `a_per_attempt_refusal_names_the_failed_item` (N1), `a_code_child_without_a_sandbox_runtime_is_refused`, `a_direct_router_child_is_refused_with_its_own_text` | pass |
| `parallel_tests.rs` | `a_refused_activation_freezes_and_starts_nothing`, `a_frozen_activation_is_never_re_admitted_on_resume` | pass |
| `node_recovery_code_debug_caller_tests.rs` (real remote staging path) | `a_code_child_visit_names_its_fanout_owner_scope` (C1 Worker half), `a_code_child_scope_serves_only_its_own_visit`, `a_code_child_that_needs_an_operator_ends_typed_and_holds_no_slot` (C4) | pass |
| `code_state_tests.rs` | `a_map_item_channel_is_read_as_any_json_and_never_written` | pass |
| `fanout_families_parity_tests.rs` (N2) | `the_compiler_admits_exactly_the_contract_fanout_families` | pass |
| Worker real PostgreSQL (`node_recovery_code_wiring_tests.rs`, CI prefix `agents::graph::node_recovery_runtime::tests::code_owner_tests::`) | `real_pg_two_sibling_attempts_on_one_slot_both_complete_never_nested` (R1/C3, slots 1, no failure event), `real_pg_preparation_past_the_window_is_refused_before_any_submission` (N3), `real_pg_a_possibly_started_attempt_is_never_refused_after_preparation` (C2) | see the next row |
| Worker real PostgreSQL, fan-out (`node_recovery_code_fanout_pg_tests.rs`, same prefix): a Map compiled by the production compiler over the production checkpoint family (`PostgresCheckpointer` + application family lending each branch its node journals), the production `RemoteCodeRuntime`, fake Main and Supervisor; a Worker replacement is a dropped run plus a later claim (fence) resuming the frozen occurrence | `real_pg_a_map_of_8_code_items_survives_worker_replacement` (plan test 1: killed with 4 items completed and 2 submitted; after the takeover 8 dispatch identities (the first 6 kept), 8 dispatch rows all resolved, 10 submissions = 4 completed once + 2 re-attached + 2 fresh, one landed answer per item: R), `real_pg_a_supervisor_losing_code_items_reruns_each_once` (plan test 2: two runtimes lost, each re-run once with a new identity, 6 submissions, 6 resolved rows, 4 answers: I), `real_pg_a_platform_client_code_item_killed_after_dispatch_is_reconciled_never_rerun` (plan test 5: killed after dispatch, then runtime lost; with the owner journal proving nothing unresolved it settles typed (C); without that proof it ends `child_effect_unknown` (F); claims 2 and 3 each, one identity, never re-run), `real_pg_per_child_code_overhead_is_within_its_budget` | the whole prefix with `--ignored`: **31 passed, 0 failed** on PostgreSQL 17 and three times on PostgreSQL 18 |
| `graph_receipts` (PostgreSQL 18 guard) | `cargo test --lib --all-features graph_receipts` with `ELITEA_REQUIRE_POSTGRES_RECEIPT_TESTS=1`, `ELITEA_TEST_DATABASE_GUARD=disposable-pg18` on `postgres:18` (18.4) | 3 passed, 0 failed |
| D1, D3 unit (`fanout_code_child_tests.rs`, `code_state_tests.rs`) | `an_unproven_code_branch_fails_typed_and_the_others_finish_under_continue` (code, readable sentence, 32-hex support_ref), `an_unproven_code_item_stops_the_map_typed_and_keeps_completed_items`, `a_map_item_reaches_code_only_within_the_code_input_byte_bound` (512 KiB exactly vs +1 byte: Code never called), `a_map_item_is_bounded_by_the_code_input_depth_and_field_limits` (128 vs 129 nested values), `a_64_item_code_map_logs_no_item_values_or_index` (markers in every item value and as the index channel's name; none in the log; the D1 stop is one WARN line with its sentence and support_ref), `a_code_child_visit_is_canonical_over_its_input_order` | pass |
| Worker lib | `cargo test --offline --locked --lib --all-features` | **3170 passed, 0 failed, 121 ignored**; the same with `ELITEA_TEST_DATABASE_URL` and the PostgreSQL 18 guard set as CI sets them: 3170 passed, 0 failed |
| Main `pipelinelimits` | `TestFanoutChildFamiliesMatchTheSharedContract` (N2), `TestCheckAdmitsACodeMapWorkerAndACodeParallelBranch`, `TestCheckRefusesADirectRouterChildWithItsOwnText` | pass |
| Main `repos` (C1) | `TestFanoutCodeChildScopeResolvesItsOwnerAndFailsClosed`: admitted Map item, Parallel branch, last index; refused scope from another execution, index outside the frozen count, frozen count above `max_items`, empty family, Parallel ordinal outside its branches, branch at the wrong ordinal, foreign project, node id not owned by the scope, visit node differing from the scope, owned node not Code, forged family for a Map node, forged family kind, root thread as child thread, child thread of the other family, visit on another thread, unknown scope field. `TestAMapItemCodeWorkerInputPolicyAdmitsOnlyItsItemAndIndex` | pass |
| Main `pipelinelimits` (D7) | `TestTheBranchRefusalSentenceNamesCodeAsTheWebEditorDoes` | pass |
| Web | `graphExtensions.families.test.ts` (contract parity, Code admitted in both families, Router text in both); the copy naming Code in `graphParallelAdmission.helpers.test.ts:97`, `ParallelSettings.test.tsx`, `ParallelNode.test.tsx` | `src/features/pipelines` + i18n: 245 files, 2790 passed, 1 expected fail; `tsc --noEmit`, oxlint and the i18n check clean |

Existing tests changed: the label and sentence assertions in `graphParallelAdmission.helpers.test.ts:97`,
`ParallelSettings.test.tsx` (4) and `ParallelNode.test.tsx` (1) now name Code (D7, by decision);
`map_forms_test.go` `TestCheckRefusesAMapWorkerOfAnUnsupportedForm` drops `code` from its
refused list; `graphMapAdmission.helpers.test.ts` "refuses unreviewed %s worker effects" drops `code`;
`graphMapAdmission.v2.test.ts` pins the Map worker list with `code`. Fixture literals of `CompilerParallelBranches`
gained the two new fields (`code: None, code_recovery: false`) in four test files.

## Performance

- Activation admission is in memory, inside the existing `freeze` after its one `load`: no new round trip, O(children).
- A Code child costs what a root Code node costs plus nothing per sibling: one visit-scoped journal per attempt (the
  `n1:` writer activation), its visit, preparation and submission. Slots bound the Supervisor load (W3-1).
- **Per-child overhead budget, measured** (`real_pg_per_child_code_overhead_is_within_its_budget`). Method: user code
  excluded (the fake Supervisor answers at once); every run on a fresh thread and claim over real PostgreSQL with the
  production compiler, checkpoint family, node journals and `RemoteCodeRuntime`. One sample runs a root chain of 1 and
  of 9 Code nodes and a Map (concurrency 1) of 1 and of 9 Code items; the marginal cost of a root Code node is
  `(root9 − root1)/8`, of a Code child `(map9 − map1)/8`, and their difference is the per-child overhead, so the Map's
  one-time activation (freeze, join) is not charged to its children (it is reported separately). 20 samples after one
  warm-up. Gate: p99(overhead) ≤ 30 ms (`CHILD_OVERHEAD_P99`).

  | Run (MacBook, Docker PostgreSQL) | root Code p50 / p99 | Code child p50 / p99 | overhead p50 / p99 | Map activation p50 / p99 |
  |---|---|---|---|---|
  | PostgreSQL 17 | 17.0 / 31.7 ms | 18.5 / 36.9 ms | 1.6 / **8.6 ms** | 15.6 / 36.1 ms |
  | PostgreSQL 18, run 1 | 17.8 / 41.1 ms | 20.4 / 29.4 ms | 1.7 / **5.7 ms** | 16.0 / 56.1 ms |
  | PostgreSQL 18, run 2 | 17.5 / 32.9 ms | 19.8 / 31.5 ms | 1.7 / **9.8 ms** | 15.7 / 27.8 ms |
  | PostgreSQL 18, run 3 | 17.9 / 124.8 ms | 18.5 / 52.6 ms | 0.0 / **3.7 ms** | 16.9 / 29.6 ms |

  The samples ran with the other tests of the prefix in parallel, so the absolute p99s are noisy; the per-sample
  difference cancels most of that noise. The 64-item preparation is the adapter's existing `prepare_children`
  (2 transactions for any child count); a Code child adds no row there.

## Durability

- Every Code child attempt is journaled before its first effect, under its own child thread and visit; completed items
  replay their durable result and are never re-run (the adapter's child receipts plus the attempt journal).
- An activation is frozen once; admission runs only before that first freeze (`parallel_checkpoint.rs:139`), so a
  restart never refuses started children (`a_frozen_activation_is_never_re_admitted_on_resume`).
- N3 runs before the durable submission marker and only for a fresh attempt (`real_pg_..._never_refused_after_preparation`).
- A replacement Worker resumes the frozen occurrence: completed items replay, in-flight items re-attach to their own
  dispatch identity, and every item lands once (`real_pg_a_map_of_8_code_items_survives_worker_replacement`).

## Resilience

- Bounded: ≤ 64 children per activation (Main checks `item_count` ≤ 64 and ≤ `max_items`), the W3-1 slots, the run
  deadline.
- Typed failures: `runtime.fanout_deadline_insufficient` (activation), `runtime.code_deadline_insufficient` and the
  member code `pipeline.code.deadline_insufficient` (N1), `pipeline.code.nested_slot` (operator only),
  `pipeline.node_recovery.child_effect_unknown` (a child that would need an operator).
- R1 holds by construction (activations never take a slot) and is enforced by the marker; slots are never held across
  a pause: a Code child never pauses, and its slot is a local of its attempt (C4 test).

## Security

- C1: the owner scope is a selector, never a grant; Main resolves every field against its own frozen state in the
  same transaction and fails closed (17 negative cases). The Worker serves the scope only for its own child's Code
  visit.
- No new egress, no new parsing of untrusted structure beyond a strict 4 KiB JSON decode (unknown fields refused).
- Logs: the fan-out refusal line and the nested-slot line carry ids and numbers only
  (`a_fanout_refusal_is_one_fault_line_with_its_numbers`); Code source and item values never reach a log line (the
  W3-1 marker test covers the attempt path).
- Map item JSON reaches only the sandbox input, bounded by the existing Code input depth/size limits; it can never be
  written back to state.

## Recovery guarantee rows (rules §2)

| Component × phase | Class | Enforced by | Proven by |
|---|---|---|---|
| Worker lost mid-Map, items completed or in flight (P10 in P13) | R (completed replay, in-flight re-attach to their own identity) | frozen occurrence + per-child receipts, per-child visit-scoped journal (`parallel_compiler.rs:1034`), canonical visit (`fanout_code_child.rs:101`) | `real_pg_a_map_of_8_code_items_survives_worker_replacement` |
| Supervisor loses a Code item's runtime (P12/P13) | I (re-run once, new identity, FU-214) | the child's `RecoverableNode` with the default policy | `real_pg_a_supervisor_losing_code_items_reruns_each_once` |
| Platform-client Code item killed after dispatch, owner proof present (P11 in P13) | C (reconciled from the owner's journal, settles typed, never re-run) | the attempt's owner query, same as root Code | `real_pg_a_platform_client_code_item_killed_after_dispatch_is_reconciled_never_rerun` (Effects::None) |
| A Code item that may have started, owner proof missing | **F** (typed `child_effect_unknown`, readable sentence, support_ref, never repeated, partial results kept, no slot held) | `node_recovery_runtime.rs` visit rule, `CodeBranchNode` (`fanout_code_child.rs:150`) | `..._is_reconciled_never_rerun` (Effects::Possible), `an_unproven_code_branch_..._under_continue`, `an_unproven_code_item_stops_the_map_typed_and_keeps_completed_items`, `a_code_child_that_needs_an_operator_ends_typed_and_holds_no_slot` |
| Worker × activation refused by deadline | F (typed, nothing started) | `admit` in `freeze` (`parallel_checkpoint.rs:139`) | `a_refused_activation_freezes_and_starts_nothing`, `a_refused_code_fanout_starts_no_item_and_ends_typed`, 64/65 and limit−1 ms |
| Worker × refused after preparation (N3) | F (typed, never submitted) | `code_attempt_remote.rs:378-380` | `real_pg_preparation_past_the_window_is_refused_before_any_submission` |
| Worker × restart after freeze | R (not re-admitted) | early return in `freeze` | `a_frozen_activation_is_never_re_admitted_on_resume`, the Worker-replacement test |
| Main × Code child visit authorization | fails closed | `code_source_definition.go:101` | `TestFanoutCodeChildScopeResolvesItsOwnerAndFailsClosed` |
| Main × operator recovery of two children | not reachable in 2a: no child suspends; the item that would need an operator is the F row above | — | FU-310 (exit work) |

**Why F is honest here.** F applies only where the attempt may have started and no owner proof decides its effect:
inside a member there is no operator route yet (FU-310 adds the member-scoped pause, Main's per-thread visits and the
UI). Without it the alternatives are a blind re-run (forbidden: the effect may have happened) or a silent wait. F
stops that one item typed with a support reference, never repeats it, keeps the other items, and holds no Code slot.
Wherever proof exists it is used: an ordinary Worker loss re-attaches (R), a lost runtime re-runs once (I), and a
platform-client item with the owner's proof is reconciled (C).

## Conditions

- **C1** — as above (Main half `code_source_definition.go:101`, `runtime_fanout_code_child.go:29`; Worker half
  `fanout_code_child.rs:75`). The operator recovery routes did not change shape, so the §3a matrix was not re-run.
- **C2** — N3 is skipped when `prior_dispatch` (`code_attempt_remote.rs:378-380`), proven on real PostgreSQL.
- **C3** — the marker wraps only the four Code attempt futures (`code_remote.rs:790-821`); slots-1 tests show no
  nested refusal (unit `sibling_attempts_in_one_task_...`, real-PG `..._never_nested`, both with no failure); the
  structural test proves no activation/child source takes a slot. The marker does not follow `tokio::spawn`.
- **C4** — a Code child never suspends while holding a slot: an attempt that needs an operator ends typed in its own
  visit and its slot is free at once (`a_code_child_that_needs_an_operator_ends_typed_and_holds_no_slot`).
- **C5** — met by construction: the Worker produces no pending operator visit for a fan-out child (C4 test: an error,
  never the `pipeline_node_recovery` interrupt), so the 0146 single-pending index is never challenged by 2a. The index
  swap moves with §2.6.
- **N1** — `a_per_attempt_refusal_names_the_failed_item`. The per-attempt check still guards an underestimated Code
  time (a Router loop in 2b, or preparation): a late attempt is refused typed, it never overruns.
- **N2** — `libs/proto/contracts/pipeline-fanout-child-families-v1.json`, asserted by the Worker, Main and Web.
- **N3** — `pipeline.code.nested_slot` is registered in the same commit as its use (`2073b4125`); the FU-299 gates
  pass (`go test ./...` in `libs/go/faults`).

## Deviations from the plan

1. **§2.6 is deferred** to **FU-310 (exit work)**, registered before ETL acceptance. The 0146 index swap and Main's multi-visit control are not in this PR. Per-child operator
   recovery also needs the Worker to raise a member-scoped `pipeline_node_recovery` pause through the fan-out card
   path, Main to project and poll per activation, and the UI to show several cards; none exists. In 2a a Code child
   that would need an operator ends typed (`child_effect_unknown`, never repeated) as a saved child's journaled call
   does (FU-248); the F row above records it and its justification.
2. **C1 uses a stateless owner scope** resolved against the run's frozen root source, not the saved-child registry
   (`execution_saved_child_scopes`): that registry has no registration route and no Worker provider in production.
3. **Explicit `recovery:` declarations on a fan-out Code child stay refused** (`node_recovery_definition.rs` unchanged,
   FU-311); the child uses the default policy, as a root Code node without a declaration does. A failure route stays
   root only (FU-312).
4. **N3 applies to the journaled path** (the production profile). The raw path (`execute`, Code tools) keeps W3-1's
   admission only.
5. **Code children do not stream their trace phases** to the chat (FU-313; no event sender inside the member, like
   FU-260 for nested LLM nodes). The member's lifecycle frames are unchanged.
6. **Web copy (D7, done).** The generic Parallel refusal sentence, the branch picker label and the owner hint name Code;
   Main's sentence is the same, pinned by `TestTheBranchRefusalSentenceNamesCodeAsTheWebEditorDoes`.

## Browser-pass defects (after `44c792f4f`)

| Id | Root cause | Fix | Test (failed before, passes after) |
|---|---|---|---|
| B1 (blocker) | `ApplicationBranchCatalog::for_child` refused any Parallel branch with no application path: a Code branch has none, so the gather node failed `checkpoint.invalid_scope` ("the owned application checkpoint family is not admitted") before any Code attempt. Only a path-free Map worker was allowed. Test 1 used a Map, so it never met this. | Every path-free child (State Modifier, Code) gets an empty family in both kinds (`application_children.rs` `for_child`); Agent and inline LLM children keep their paths, admitted at assembly. | Real assembly path: `postgres_a_parallel_of_code_branches_starts_each_branch_through_assembly` (`AuthorizedNativeAssembler` with a `CodeRuntimeFactory`; before: `agent.legacy` + `invalid_scope`; after: each branch runs its attempt and fails typed as a member against the unreachable sandbox, `graph.fanout.member_failed`). Real-PG production family: `real_pg_a_parallel_of_code_branches_runs_on_the_production_family` (before: `invalid_scope`). Unit: `a_path_free_child_gets_no_family_in_a_parallel_and_a_map`. |
| B2 | A failed child's cause was logged nowhere: only "Fan-out failed". | One WARN line per failed child, registry code `graph.fanout.child_failed`: `member_code`, `member_sentence` (the code's fixed text), `cause` (error kind, node ids and a leading dotted code only, never free text), `execution_id`, `support_ref` (= execution id), `node_id`, `item`, `item_ref` (the join entry's support reference) (`parallel.rs` `log_child_failure`, `safe_graph_cause`). A committed Code stop names its `failure_class` on "pipeline Code node stopped". | `a_64_item_code_map_logs_no_item_values_or_index` (two child lines with every field; no item marker logged), `a_child_failure_cause_keeps_codes_and_node_ids_only`. |
| B3 | Main's Code visit and intent registrations are Serializable and wait on the run's claim lock with a snapshot taken before the wait, so the items of one Map registering together abort with 40001: answered as a silent `Unavailable`, or as a refusal when the conflict surfaced in a read Main maps to `ErrRejected`/`ErrContentUnauthorized`. The Worker logged "Main was unavailable". Retrying alone cannot settle a burst: every waiter loses to every earlier holder. | (Superseded below: READ COMMITTED with the claim row lock first.) Main: a per-run session advisory lock taken before `BEGIN` (`WithinRunSerializedTx`, snapshot after the lock); a 40001/40P01 met by any statement is recorded and the transaction retried at most 3 times with jitter (`code_registration_retry.go`); still contended → one WARN fault line `code.registration_contended` (execution id, support_ref) and a retryable `Unavailable` whose message names the code. Worker: the retry line is the fault line `pipeline.code.main_retrying` with `main_code` parsed from Main's `"<sentence> (<code>)"` status only when the registry knows it. | Real PostgreSQL 18: 8 concurrent registrations of one run, the first held until the other 7 wait on the lock (deterministic): `TestOriginalCodeVisitRegistrationsConflictWithoutRetryPostgres` (without the fix 7 of 7 fail 40001), `TestOriginalCodeVisitRegistrationsOfOneRunAllCommitPostgres` (all 8 commit; measured with retry alone and no lock: 1 of 8 still contended after 3 attempts). `TestAContendedCodeRegistrationIsRetryableAndLoggedOnce`, `a_retryable_main_answer_names_its_registered_code_only`. |
| B4 | The Map worker hint did not name Code. | "It can be a State Modifier, an LLM node, a Code node, or a saved agent or pipeline." | `graphExtensions.families.test.ts` pins the catalogue text and the component default. |
| B5 (blocker) | **Not root-caused in this change**; see below. | — | `real_pg_a_worker_killed_mid_registration_is_resumed_by_the_replacement` (passes; does not reproduce). |

**Reruns after B1-B4 (PostgreSQL 18.4, disposable).** Worker lib with `ELITEA_TEST_DATABASE_URL`: 3171 passed,
123 ignored; the 3 `graph_receipts` tests need `ELITEA_REQUIRE_POSTGRES_RECEIPT_TESTS=1` and the PG18 guard as CI sets
them, and then pass (3/3). Real-PG prefix `--ignored`: **33 passed, 0 failed** (adds the B1 and kill-mid-registration
tests); run alone, the overhead gate measured p99 6.4 ms (root Code 18.1/21.8, child 20.2/28.2, activation 15.7/33.1
ms). With Main's full suite on the same database it read 441.7 ms (activation p99 2.9 s) and failed: contention, not
the code. Agent-runtime 1829 passed. `fmt`, `clippy -D warnings`, `cargo doc -D warnings` clean. Main `go vet ./...`
clean; `go test ./...` passes, `authflow`/`authsession`/`migrate` only with `-parallel 1`/`-p 1` on this container
(53200 "out of shared memory" when many tests apply all migrations at once; those packages are untouched). Web
`vitest run pipelines`: 274 files, 2961 passed, 1 expected fail; `tsc`, oxlint and the i18n check clean.

**B3 lock: superseded, not reverted.** The session-level advisory lock (`4e6664cf2`, hardened in `9c2fd2048`) is
gone: the next commit replaces `WithinRunSerializedTx` and the executor wrapper. The registration transaction now runs
**READ COMMITTED** (`codeRegistrationTx`, `code_registration_retry.go`); its first statement is `lockAccess`'s
`FOR UPDATE OF j,c,ws,command` on the run's job and claim rows, so registrations of one run run one after another and
every later statement takes a fresh snapshot after the wait. PostgreSQL releases those row locks on commit, rollback or
connection loss. Invariants: the per-run visit cap counts under that row lock; visit and intent rows are inserted
`ON CONFLICT DO NOTHING` (primary key and the `(execution, generation, activation, attempt)` unique key) and read back
`FOR UPDATE` and compared byte for byte; the claim, fence, lease and permission are read under the lock, and the intent
re-reads the claim before signing. None of them needs SERIALIZABLE. A 40001/40P01 still met is retried at most 3 times
(raw errors only, no wrapper); after that, `code.registration_contended`.

**B6 (blocker, a regression from the B3 change).** On w32b (images at `4e6664cf2`) a plain concurrent Map was refused:
`pipeline.code_authorization_failed`, class `AuthorizationDenied`, and the run had **0** rows in
`original_code_visits`. Root cause: Main's production permission check `checkRecoveryPermission`
(`node_recovery.go:49`) type-asserts the transaction executor to `pgxExecutor`; the B3 change handed the registration
callback a `conflictRecordingExecutor` wrapper, so the assertion failed → `ErrNotAllowed` → `lockAccess` →
`ErrContentUnauthorized` → `Aborted` → the Worker's `AuthorizationDenied`. Every visit registration was refused,
concurrent or not. It is the B3 change, not the C1 child-scope resolver: the 17 C1 negatives are unchanged and still
refuse (`TestFanoutCodeChildScopeResolvesItsOwnerAndFailsClosed`). The B3 tests missed it because they stubbed the
permission check, and the Worker tests fake Main. Fix: no wrapper; the callback gets the store's own executor. Test on
the production path (`code_registration_real_path_postgres_integration_test.go`: `NewCodeIntentRepository`'s shared
store and `checkRecoveryPermission` over seeded RBAC, the captured root source, `CodeDefinitionSourceAdapter`, real
claim): `TestFourConcurrentMapItemsRegisterOnTheProductionPathPostgres` (4 Map items' visit + intent at once) **failed
at `9c2fd2048` with all 4 refused** (`input content authorization failed`, 0 visits), passes after (4 visits, 4
intents, replay idempotent, nothing locked).

**The w32b leaked lock** (pid 219, `classid 1354237349 objid 3365584093` = `hashtextextended('code_registration:7f64…e0e4:1',0)`,
idle in the pool, a session later used by the admission publisher) came from `4e6664cf2`'s session lock surviving on a
pooled connection; every later registration of that run waited until the Worker's 10 s timeout ("canceling statement
due to user request" every ~11 s) and ended Unavailable with no code. With row locks there is nothing to leak:
`TestRunRegistrationCancelledWhileWaitingLeavesNoLockPostgres` (cancel while waiting) and
`TestRunRegistrationsCancelledInFlightLeaveNoLockPostgres` (one holding, three waiting, all cancelled as a killed Worker
would) end with 0 advisory locks, 0 lock waits, 0 open transactions in the database, and the next registration of the
run commits within 2 s. The 8-way test (`..._OfOneRunAllCommitPostgres`) commits all 8 in one attempt and leaves the
same zeros.

**Main fault lines (B6 observability).** Every refused or failed Code visit or intent registration writes one WARN line
(`runtime_code_intent.go`): `code.registration_refused` (Aborted), `code.registration_failed` (retryable, its status
now names the code) or `code.registration_contended`; with the registry sentence, `stage` (the step that refused:
`claim`, `source`, `policy`, `configuration`, `prepared`, `input_policy`, `input`, `visit_cap`, `visit_mismatch`,
`dispatch`, `workspace`, `broker`, `request_digest`, `binding`, `intent_mismatch`, `claim_recheck`, …, or
`claim_identity`, `busy`, `request` before the repository), the safe cause chain, `execution_id`, `generation`,
`support_ref` (= execution id) and, for a visit, `node_id`, `graph_thread`, `activation_id`, `attempt`; for an intent
`visit_id`, `dispatch_activation`. Never Code source, input, configuration or the prepared job.
`TestEveryRefusedOrFailedCodeRegistrationWritesOneFaultLine` captures the handler's log for refused, failed and
contended visits and intents carrying a marker in their source, input and configuration: one line each, every field,
no marker.

**B5, Main's half on the production path.** `TestATakeoverReRegistersEveryMapItemOnTheProductionPathPostgres`: a
Worker registers 4 of 8 Map items; item 4 holds the claim lock and items 5-7 wait when it is killed (ctx cancelled);
the old claim is released and a new claim (attempt 2, epoch 2) re-registers all 8 at once: all succeed, the 4
registered items re-attach to the same visit ids, one visit and one intent per item, nothing locked. On w32b the
takeover failed because of B6 (items 2, 4, 5 refused) and the leaked lock (item 1 retried Main until its deadline:
`DependencyUnavailable` → `pipeline.code_failed`). The first B5 run (`44c792f4f`, SERIALIZABLE, before any B3 change)
fits the same mechanism without the leak: at takeover the 4 re-admitted items re-register together and only the
claim-lock holder commits each round, so an item can exhaust its Main retries (`DependencyUnavailable`), or meet the
conflict in a read Main maps to a refusal. The Worker-side real takeover still runs against a fake Main
(`real_pg_a_map_of_8_code_items_survives_worker_replacement`); real-browser confirmation of R/I is pending the next
pass.

Advisory-lock users in the repo (`git grep pg_advisory / pg_try_advisory`), kept for reference (the registration no longer takes any advisory lock):

| Form | Users and keys |
|---|---|
| two int4 (`objsubid` 2, shares the space with this lock) | Main personalproject `ensurer.go:551` class `0x454C5041` ("ELPA"); Main deepwiki history `deepwiki_history.go:100,192` class `0x0DEE0417`; Main nativeauth `store.go:915-917` class `250025`; deepwiki-engine `storage/build.rs`, `storage/delete.rs` `hashtext(<name>)` first key; inventory-engine `store/mod.rs:557`, `store/sources.rs:483` `hashtext('elitea_inventory.graph' / '.ingest')` (both engines use their own databases); test-only deepwiki-engine fixtures |
| one bigint (`objsubid` 1, a separate space) | Main: `hashtextextended(...)` keys in oidc, agent_memory, agent_proposals, agent_steering (`'steering:'`), agent_workforce_policy, conversation_fork, conversations, index_registry, local_child_turns, vector_claim_tokens (`'elitea_memory_conversation:'`), web_push (`'web_push_user:'`), workspace_store, workspace_handoffs, execution_suspensions, pgvault, code_debug_artifacts (seed 701); `hashtext(...)` in cloud_executor_attestation/settings, connector_inventory, connector_requests; constants in secrets_header_value, supportassistant, compiled_snapshots, platform_preamble, migrate/lock.go, dbtest/template.go, auth_provisioning.sql; session locks in auditexport/relay.go and securityaudit/relay.go; subapp-host `spi/pgstore.go`; scheduler `pricesync`; Rust `pg-migrate`; Worker test `640064`; two database scripts |

**Raw i18n key on the Map card** (`pipelines.graphExtensions.values.` beside "Owned worker" and "Worker output 1"):
`ExtensionChoice` renders the empty choice as `t('pipelines.graphExtensions.values.' + '', '')`; i18next ignores the
empty default and shows the key. Unchanged from `origin/main` (`ExtensionFields.tsx:27`), not introduced here; not
fixed (report only).

**B5 — what is known.** Takeover log "Fan-out started resumed:false" is not evidence of a lost occurrence:
`resumed` is a constant `false` passed to `fanout_trace::activation_span` by the shared 2a-2 adapter
(`parallel.rs:2009`, unchanged from `origin/main`), for every family. "start fresh" is the correct label for an
in-flight child with no receipt. Main held 8 visits and 8 intents with distinct activations, so the frozen
occurrence and the child identities were restored: the shared takeover is not the cause, and the STOP condition does
not apply. Item 1 ended `pipeline.code_failed` at **attempt 1** on the journaled path, i.e. the replacement Worker
re-attached the started attempt and that reconciliation failed with a class mapped to `pipeline.code_failed` under the
default Stop policy (DependencyUnavailable, Unknown, InvalidResult, AttemptTimeout, RuntimeLost exhausted, invalid
configuration or input), not an authorization class (that would be `pipeline.code_authorization_failed`). The line did
not name the class, and the stack was torn down, so the class is not known.

Ruled out against code: Main intent replay across claims (binding claim-independent, `ON CONFLICT` + exact compare,
fresh signature per claim); the Supervisor ledger binding (claim-independent); the Worker dispatch scope (execution and
generation); the visit record (deterministic fields incl. the fan-out scope; canonical child visit fixed earlier); N3
(skipped when `prior_dispatch`). Why test 1 passed while the browser failed: test 1 drives the production compiler,
checkpoint family, node journals and `RemoteCodeRuntime` but fakes Main and the Supervisor, which answer every replay
at once; the real answers on the re-attach path (Main under concurrent registration, B3; the Supervisor for a job a
dead connection submitted) are where it can diverge. Tests 2 and 5 exercise the same production Worker path with the
same fakes, so they share that limit.

Most likely contributor, now fixed: at takeover the four in-flight items re-register at once; B3 made Main answer them
`Unavailable` or, when the conflict hit the claim-lock read, as a refusal. Instrumentation added so the next browser
pass names the cause: `failure_class` on "pipeline Code node stopped", the `graph.fanout.child_failed` line (member
code and safe cause), and `main_code` on every Main retry. Real-browser confirmation of B5 is open.

## Real-browser evidence

Pending (done by the orchestrating session): plan §5 cases 1, 3, 4, 5 and 6 on a disposable production-profile stack.

## Follow-ups

- **FU-310 (exit work):** per-child operator recovery: the 0146 index swap to per-thread pending visits, Main control
  (resume at zero, a typed refusal on the old index for C5), the Worker's member-scoped `pipeline_node_recovery` pause
  and the UI for several cards. Until then the F row above applies.
- **FU-311:** a `recovery:` block on a fan-out Code child.
- **FU-312:** failure routes resolved inside a child.
- **FU-313:** streaming Code trace phases from children.
- The raw path (`execute`, Code tools) has no N3 re-check.
