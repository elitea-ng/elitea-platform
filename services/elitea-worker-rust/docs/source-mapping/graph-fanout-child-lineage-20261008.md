# Graph fan-out child lineage, control stops and no polling (2026-10-08)

Track C1 of Point 5 Wave 1. Branch `fix/graph-fanout-child-lineage`, base `origin/main` (`604a49e1`).
It applies to the existing Parallel (`graph.parallel`) and Map (`graph.map`) runners. Both admission gates stay
`false` (`FIXED_PARALLEL_INTEGRATION_READY`, `MAP_INTEGRATION_READY`), so production behaviour changes only once
the gates flip. This fix is a mandatory gate-flip criterion.

## Business behaviour

| Topic | Current platform (reference only) | New platform after this change |
|---|---|---|
| Parallel sub-agent children across a HITL resume | The legacy async supervisor parks the parent, runs children as separate durable tasks, then reconciles and replays. It loses concurrent pauses, multi-round replay does not converge, and the UI shows phantom instances. | Children keep one identity for the life of the occurrence. A continuation restores completed children from their receipts and resumes a paused child from its own checkpoint. |
| Lease loss of a child | Not modelled. A lost task re-runs from scratch. | A typed control stop with no failure record. A later claim restores the same children. |
| Stop and deadline | Polled. | Event-driven, bounded by the deadline and a cleanup bound. |

Deliberately **not ported**: the park/reconcile/replay supervisor and the complete-set decision model. The existing
complete-set resume code (`parallel_published_resume.rs`) is left untouched and unwired, and Wave 2 replaces it.
`ParallelPublishedContinuation` is not wired.

## Defects fixed

1. **Lineage loss.** Child thread ids hashed the current claim's `execution_id` and `generation`. Every HITL
   continuation is a new execution with generation 1 (Main `agentexecution/continue.go`,
   `execution/submit_job.go`). After any resume, completed children ran again and paused children lost their
   checkpoint.
2. **Lease loss reported as a business failure.** `writer_not_current` became
   `ParallelBranchOutcome::Failed` and then `graph.parallel.branch_failed`. In Map it became a durably recorded
   `MapStop::Failed`. The lifecycle wrappers also wrote a durable `Failed` receipt for a cancelled child.
3. **Busy polling.** Parallel woke every 10 ms. Map's item cancel watcher polled every 50 ms.

## Changed paths (Worker, `services/elitea-worker-rust/src/`)

| Path | Change |
|---|---|
| `agents/graph/parallel.rs:93` | `ParallelChildOrigin` {execution_id, generation} with `validate()`. Factory trait gains `child_origin`, `branch_thread_id`, and `for_branch(.., origin)`. |
| `agents/graph/parallel.rs:457` | `mint_lineage` proposes origin and threads. Restore requires the frozen thread (`:572`). |
| `agents/graph/parallel.rs:924-1066` | `drain_branches` uses `select!` over the latch, deadline, cleanup deadline and children. Adds `ParallelBranchOutcome::LeaseLost` (`:438`, `:1008`) and `begin_stop` (`:1053`). |
| `agents/graph/parallel_checkpoint.rs:18-19,285,295` | Occurrence format v2, legacy v1 refusal, lineage validation. `freeze` keeps the existing lineage (`:86`). The lifecycle wrapper records nothing on lease loss or cancel (`:610`). |
| `agents/graph/map_reduce.rs:394,452` | Origin and threads minted before freeze; restore requires the frozen thread. |
| `agents/graph/map_reduce.rs:362,481,505,586,679` | `settle_stop` (cancel recorded nothing), sibling-stop latch on lease loss, latch `select!` in items. `wait_for_cancellation` deleted. |
| `agents/graph/map_reduce_checkpoint.rs:17,164,171,521` | Map occurrence v2, legacy refusal, lineage validation, lifecycle control stop. |
| `agents/graph/fanout_control.rs:22,46,72` | `FanoutCancellation` latch (lost-wakeup safe) and `is_lease_lost`. |
| `state/postgres_checkpointer/parallel_children.rs:21,37,116,160` | Origin-based derivation. Hash and domain unchanged; generation hashed as i64 big-endian. |
| `state/postgres_checkpointer/map_children.rs:34,70,114,140` | Same for Map, with one shared `check_map_item` validation. |
| `state/postgres_checkpointer/application_children.rs`, `agents/pipeline/scope_receipts_{parallel,map}.rs`, `agents/graph/map_turn.rs` | Delegation only. |
| `agents/graph/mod.rs` | `fanout_control` module; test-only re-exports for the PostgreSQL proofs. |

## Performance

| Rule / budget | Mechanism | Proving test | Measured |
|---|---|---|---|
| No busy polling: zero timer wakes while children run | `parallel.rs:973-986`, `map_reduce.rs:671-684`. The only timers are the absolute deadline and the cleanup bound. The 10 ms `sleep` and 50 ms `wait_for_cancellation` are deleted. | `latch_cancel_stops_inflight_branches_within_the_cleanup_bound_and_admits_nothing_new`, `latch_cancel_stops_pending_items_promptly_without_recording_a_stop` (4 worker threads, channels, no sleeps) | — |
| Cancel latency ≤ 100 ms | Latch `select!` arm | Same tests, each asserting `elapsed ≤ 100 ms` | Parallel: 21.8 / 22.2 / 24.2 ms (20 ms cleanup bound for non-cooperative children). Map: 0.18 / 0.20 / 5.4 ms. |
| Deadline honoured | `sleep_until(deadline)` arm | `deadline_stops_non_cooperative_branches_at_the_deadline_plus_cleanup` (≥ 50 ms, ≤ 50+20+100 ms), `deadline_stops_pending_items_at_the_deadline_and_is_still_recorded` | Parallel: 74.0 / 75.1 / 97.7 ms (50 ms deadline + 20 ms cleanup). Map: 52.2 / 52.8 / 61.3 ms. |
| Freeze cost | 1 `child_origin` read plus N SHA-256 derivations (N ≤ 16 branches / 64 items) per `prepare`. No extra database round trip: the lineage rides inside the existing single occurrence append. | Covered by the lineage tests. Not benchmarked separately (pure hashing). | — |
| Parent writes per activation | Unchanged: 1 freeze append, plus the existing pause or decision appends. | Existing parallel and map tests | — |

## Durability

| Rule | Mechanism | Proving test (real PostgreSQL unless noted) |
|---|---|---|
| Child identity is frozen once per occurrence and reused by every continuation, reclaim and generation bump | `parallel_checkpoint.rs:86-93`, `parallel.rs:572`, `map_reduce_checkpoint.rs:54`, `map_reduce.rs:452` | `parallel_children_survive_a_new_execution_and_resume_from_their_own_checkpoint`: E1 freezes 4 branches (2 completed, 1 paused, 1 never admitted), E2 restores via a later claim, E3 resumes. 0 re-runs of the completed branches; `prep` of the paused branch ran exactly once. `map_items_completed_before_a_takeover_by_a_new_execution_do_not_run_again`: calls `[0,1,2,2]`. **Both failed on main** (`2 != 1`; `[0,1,2,0,1,2]`). |
| Regenerate or rewind gets fresh children | A new occurrence (new step or state) mints from the current claim | `loop_visits_get_distinct_child_checkpoint_threads` (existing, in memory) |
| Writer fencing on every child write; a stale claim cannot overwrite | The unchanged takeover rule `postgres_checkpointer.rs:367-368`; activation under the current claim `parallel_children.rs:104` | `a_superseded_claim_cannot_overwrite_the_later_claims_completed_children` (C1e two-claim race): claim A blocks mid-branch, claim B restores and completes, A then gets `checkpoint.writer_not_current` (not `branch_failed`). The latest child checkpoint is B's, and there are 0 failed receipts. |
| Old format refused by type, never silently re-derived | `parallel_checkpoint.rs:285`, `map_reduce_checkpoint.rs:164` | `a_legacy_occurrence_without_frozen_child_identity_is_refused_by_type`, `a_legacy_map_occurrence_without_frozen_child_identity_is_refused_by_type` (in memory): no child is admitted |

### Recovery guarantee (replatform delivery gate §2): component × fan-out child phase

| Component down during | Before | After | Enforcing code | Proof |
|---|---|---|---|---|
| Worker, after some children completed (process loss) | L: completed children ran again under the new execution | **R**: completed children replay from receipts | `parallel.rs:572`, `map_reduce.rs:452` | Parallel and Map lineage PG tests (crash = node future dropped with a child in flight) |
| Worker, while a child is paused for HITL, then a continuation | L: paused child checkpoint orphaned | **R**: the paused child resumes from its own checkpoint (`prep` not re-run) | Same | Parallel lineage PG test (E3) |
| Worker lease lost mid-child (takeover or lease expiry) | F, wrongly: durable `branch_failed` / `MapStop::Failed` that a replacement could not undo | **R**: control stop, nothing recorded; the replacement claim restores and completes | `parallel.rs:438,1008,1039`, `parallel_checkpoint.rs:610`, `map_reduce.rs:505,586`, `map_reduce_checkpoint.rs:521` | `parallel_lease_loss_mid_branch_is_a_control_stop_a_later_claim_completes`, `map_lease_loss_mid_item_records_no_stop_and_a_later_claim_completes`, two-claim race |
| User Stop / cancel | Parallel: no record. Map: durable `MapStop::Cancelled`. | No record in either runner (cancel ends the turn; nothing is resumed from it) | `map_reduce.rs:362` | `latch_cancel_stops_pending_items_promptly_without_recording_a_stop`, `a_branch_failing_after_cancellation_writes_no_failed_receipt` |
| Effectful tool inside a child, crash between dispatch and the child's next checkpoint | I/C, unchanged (Gate 6 effect receipts) | Unchanged; fan-out does not widen it | — | Out of scope (Gate 6) |
| Main / NATS / PostgreSQL / sandbox supervisor | Unchanged by this PR | Unchanged | — | — |

## Resilience

| Rule | Mechanism | Proving test |
|---|---|---|
| Lease loss is a typed control stop, never a business failure | `fanout_control.rs:72`, `parallel.rs:438,1008,1039`, `map_reduce.rs:586`, lifecycle wrappers `parallel_checkpoint.rs:610`, `map_reduce_checkpoint.rs:521` | `a_lease_lost_branch_is_a_control_stop_not_a_recorded_failure`, `a_lease_lost_item_is_a_control_stop_and_records_nothing`, `only_writer_not_current_is_classified_as_lease_loss`, and the PG lease-revoke tests (`TestStateWriterLease::revoke` mid-branch and mid-item) |
| Lease loss stops admitted siblings | Parallel `cancel_signal` via `begin_stop` `parallel.rs:1053`; Map sibling latch `map_reduce.rs:481,505,679` | `a_lease_lost_item_stops_its_admitted_siblings` (red without the fix: 5 s timeout) |
| No admission after a stop | `begin_stop`, `admission_open && !stopping` | Cancel-latency tests assert the third branch / item never entered |
| Bounded cleanup of non-cooperative children | `cleanup_deadline` arm `parallel.rs:984` | Cancel and deadline tests assert `DropObserver` counts (futures dropped) |
| Bounded lineage data | `validate_lineage` / `valid_lineage`: thread ≤ 512 B, no control characters, distinct, not the root, count = branches/items; origin execution id ≤ 256 B, generation ≥ 1 | Legacy and lineage tests; `occurrence_from` rejection paths |

## Security

| Threat | Mechanism | Proving test |
|---|---|---|
| Frozen identity reused across tenant, project, capability or definition | The origin carries only {execution_id, generation}. Tenant, projects, capability and definition digest always come from the restoring claim's authority (`parallel_children.rs:133-160`, `map_children.rs:114-140`). The stored thread is only compared, never used as an address (`parallel.rs:572`, `map_reduce.rs:452`). | `frozen_child_identity_cannot_be_reused_across_tenant_project_or_definition`: the same origin gives 4 distinct threads in 4 scopes, and a second definition on the same root freezes disjoint children |
| A stale claim writes after takeover | Writer fence on every write (`postgres_checkpointer.rs:367-368`). Even reads through a superseded writer fail. | Two-claim race and lease-revoke PG tests |
| Checkpoint payloads, thread ids, execution ids or checkpoint ids in logs | The fan-out code emits no log lines. Error strings are static codes. | `fanout_restore_and_lease_loss_log_no_payload_or_identity` (TRACE capture over freeze, restore and lease loss). Mutation-checked: logging a child thread id makes it fail. |
| Tampered occurrence metadata | `deny_unknown_fields` on the new types; bounds above; v1 refused | Legacy refusal tests |

**Reviews.**
- `code-review` (high): 6 findings. 2 fixed (Map siblings on lease loss; legacy refusal scoped to the own step). 1
  no change needed (string-prefix classifier, pinned by a unit test). 3 skipped with reasons:
  - Parallel and Map: the parent ADK cancel is no longer polled; see follow-up 1.
  - The duplicated origin types are unified in Wave 2.
- `security-review`: no finding with confidence ≥ 8.

**`cargo deny --all-features check advisories`** (cargo-deny 0.20.2). `Cargo.toml` and `Cargo.lock` are unchanged
from main.
- Pre-existing, identical on main: `h2` RUSTSEC-2026-0258, `rsa` RUSTSEC-2023-0071 (via `sqlx-mysql`), and yanked
  `chacha20`.
- New: none.

## Tests

- New tests: 19.
  - 7 real-PostgreSQL tests in `state/postgres_checkpointer_tests/fanout_lineage.rs`.
  - 5 Parallel and 5 Map tests (in memory, 4-thread Tokio for latency).
  - 2 `fanout_control` unit tests.
- Full suite `cargo test --locked --all-targets --all-features`, with a local disposable PostgreSQL 18 and
  `ELITEA_REQUIRE_POSTGRES_RECEIPT_TESTS=1`: lib **2038 passed, 0 failed, 63 ignored**; every other target passes.
  - The 63 ignored are pre-existing `#[ignore]` tests: sandbox and hydration deadline suites run by separate CI
    steps, NATS live, and the opt-in load tests. None of them is new.
  - One earlier run had a single failure in `reverse_completion_collects_original_order_and_preserves_structured_values`.
    That pre-existing test orders items by 8/7/6/5 ms real sleeps; it passed 5/5 in isolation and in the two
    following full runs.
- `cargo fmt --check` and `cargo clippy --locked --all-targets --all-features -D warnings`: clean.
- Fixtures: every PG test builds its own isolated database through `IsolatedPostgres`, plus claims via `writer()`
  and `writer_at()`. No UI was involved.

## Real-browser regression

Parallel and Map cannot run in a browser until their Wave 2 composition, so positive browser proof of this fix is
the gate-flip criterion in `PLAN.md` §4. This PR runs the regression set over the unchanged checkpoint paths.

_Filled in after the rehearsal deploy; see the section below._

## Open limits and follow-ups

1. **Wire the latch before any gate flip.** No production code calls `with_cancellation` yet.
   - Without the latch, a parent ADK Stop is seen only by children that check `is_cancelled` cooperatively, or
     when a branch settles.
   - Before this change, Parallel polled every 10 ms. Map polled every 50 ms, but only when a deadline was set.
   - Wave 2 must fire `FanoutCancellation` from the claim lease probe on a durable Stop.
2. Wave 2 unifies `ParallelChildOrigin` and `MapExecutionIdentity` in the single `FanoutRunner` lineage module.
3. `is_lease_lost` classifies by the `checkpoint.writer_not_current` code because ADK `GraphError` has no typed
   lease variant. A unit test pins the format.
4. Track C2 (observability, O(1) parent writes, batched activation, PG load test) follows this PR.
