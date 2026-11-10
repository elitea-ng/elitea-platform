# Code as a direct Map worker and Parallel branch (W3-2a), 2026-11-10

Plan and review: `handoffs/point5-wave1-20261008/w3-2/PLAN.md` and `REVIEW-PLAN.md`. W3-2a is §2.1 (the Code arms
only), §2.2, §2.3, §2.4 and §2.5. Saved pipelines with Code, Router inside them and the `ContainsCode` deletion are
W3-2b. §2.6 (per-thread operator recovery) is **not** in this PR; see "Deviations". Stacked on W3-1 (#1879).

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
| Worker real PostgreSQL (`node_recovery_code_wiring_tests.rs`, CI prefix `agents::graph::node_recovery_runtime::tests::code_owner_tests::`) | `real_pg_two_sibling_attempts_on_one_slot_both_complete_never_nested` (R1/C3, slots 1, no failure event), `real_pg_preparation_past_the_window_is_refused_before_any_submission` (N3), `real_pg_a_possibly_started_attempt_is_never_refused_after_preparation` (C2) | the whole prefix with `--ignored`: 27 passed, 0 failed on a disposable PostgreSQL 17 |
| Worker lib | `cargo test --offline --locked --lib --all-features` | 3163 passed, 0 failed, 117 ignored. With `ELITEA_TEST_DATABASE_URL` set as CI sets it: 3160 passed; the 3 `graph_receipts` proofs need the PostgreSQL 18 guard and were not runnable on the PostgreSQL 17 container (unrelated to this change) |
| Main `pipelinelimits` | `TestFanoutChildFamiliesMatchTheSharedContract` (N2), `TestCheckAdmitsACodeMapWorkerAndACodeParallelBranch`, `TestCheckRefusesADirectRouterChildWithItsOwnText` | pass |
| Main `repos` (C1) | `TestFanoutCodeChildScopeResolvesItsOwnerAndFailsClosed`: admitted Map item, Parallel branch, last index; refused scope from another execution, index outside the frozen count, frozen count above `max_items`, empty family, Parallel ordinal outside its branches, branch at the wrong ordinal, foreign project, node id not owned by the scope, visit node differing from the scope, owned node not Code, forged family for a Map node, forged family kind, root thread as child thread, child thread of the other family, visit on another thread, unknown scope field. `TestAMapItemCodeWorkerInputPolicyAdmitsOnlyItsItemAndIndex` | pass |
| Web | `graphExtensions.families.test.ts` (contract parity, Code admitted in both families, Router text in both) | 238 files in `src/features/pipelines`: 2656 passed, 1 expected fail |

Existing tests changed: `map_forms_test.go` `TestCheckRefusesAMapWorkerOfAnUnsupportedForm` drops `code` from its
refused list; `graphMapAdmission.helpers.test.ts` "refuses unreviewed %s worker effects" drops `code`;
`graphMapAdmission.v2.test.ts` pins the Map worker list with `code`. Fixture literals of `CompilerParallelBranches`
gained the two new fields (`code: None, code_recovery: false`) in four test files.

## Performance

- Activation admission is in memory, inside the existing `freeze` after its one `load`: no new round trip, O(children).
- A Code child costs what a root Code node costs plus nothing per sibling: one visit-scoped journal per attempt (the
  `n1:` writer activation), its visit, preparation and submission. Slots bound the Supervisor load (W3-1).
- Not measured here: the plan's +30 ms p99 per-child overhead budget and "64 Code items prepare in 2 transactions".
  The 64-item batching is the adapter's existing `prepare_children` (2 transactions for any child count); a Code
  child adds no row there. Measurement is a follow-up.

## Durability

- Every Code child attempt is journaled before its first effect, under its own child thread and visit; completed items
  replay their durable result and are never re-run (the adapter's child receipts plus the attempt journal).
- An activation is frozen once; admission runs only before that first freeze (`parallel_checkpoint.rs:139`), so a
  restart never refuses started children (`a_frozen_activation_is_never_re_admitted_on_resume`).
- N3 runs before the durable submission marker and only for a fresh attempt (`real_pg_..._never_refused_after_preparation`).

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
| Worker × Code child running (fresh, lost runtime) | R / I (FU-214 retries) | per-child visit-scoped journal, `RecoverableNode` (`parallel_compiler.rs:1034`) | journal path tests (W3-1 wiring suite) on the same `RemoteCodeRuntime`; a fan-out-level process-replacement test is a follow-up |
| Worker × Code child that may have started and needs an operator | F (typed `child_effect_unknown`, never repeated, no slot held) | `node_recovery_runtime.rs` visit rule | `a_code_child_that_needs_an_operator_ends_typed_and_holds_no_slot` |
| Worker × activation refused by deadline | F (typed, nothing started) | `admit` in `freeze` (`parallel_checkpoint.rs:139`) | `a_refused_activation_freezes_and_starts_nothing`, `a_refused_code_fanout_starts_no_item_and_ends_typed`, 64/65 and limit−1 ms |
| Worker × refused after preparation (N3) | F (typed, never submitted) | `code_attempt_remote.rs:380` | `real_pg_preparation_past_the_window_is_refused_before_any_submission` |
| Worker × restart after freeze | R (not re-admitted) | early return in `freeze` | `a_frozen_activation_is_never_re_admitted_on_resume` |
| Main × Code child visit authorization | fails closed | `code_source_definition.go:101` | `TestFanoutCodeChildScopeResolvesItsOwnerAndFailsClosed` |
| Main × operator recovery of two children | **gap**: not reachable in 2a (children never suspend); §2.6 deferred | — | — |

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

1. **§2.6 is deferred.** The 0146 index swap and Main's multi-visit control are not in this PR. Per-child operator
   recovery also needs the Worker to raise a member-scoped `pipeline_node_recovery` pause through the fan-out card
   path, Main to project and poll per activation, and the UI to show several cards; none exists. In 2a a Code child
   that would need an operator ends typed (`child_effect_unknown`, never repeated) as a saved child's journaled call
   does (FU-248). Registered as a follow-up with the Worker pause path.
2. **C1 uses a stateless owner scope** resolved against the run's frozen root source, not the saved-child registry
   (`execution_saved_child_scopes`): that registry has no registration route and no Worker provider in production.
3. **Explicit `recovery:` declarations on a fan-out Code child stay refused** (`node_recovery_definition.rs` unchanged);
   the child uses the default policy, as a root Code node without a declaration does. A failure route stays root only.
4. **N3 applies to the journaled path** (the production profile). The raw path (`execute`, Code tools) keeps W3-1's
   admission only.
5. **Code children do not stream their trace phases** to the chat (no event sender inside the member, like FU-260 for
   nested LLM nodes). The member's lifecycle frames are unchanged.
6. **Web copy.** The generic Parallel refusal sentence and the picker label still read "Agent or LLM node": existing
   tests pin both texts; Main keeps the same sentence for parity. The Router text and a Code branch summary are new.

## Real-browser evidence

Pending (done by the orchestrating session): plan §5 cases 1, 3, 4, 5 and 6 on a disposable production-profile stack.

## Follow-ups

- §2.6: per-thread pending visits (migration, Main control, Worker member pause, UI) with C5's typed refusal.
- Fan-out-level real-PG process-replacement tests (plan §4 tests 1, 2 and 5) and the per-child overhead budget.
- Code child trace phases in the member tier.
- Explicit recovery policies on fan-out Code children.
- The Web picker label and the generic refusal sentence naming Code (needs the pinned tests updated).
