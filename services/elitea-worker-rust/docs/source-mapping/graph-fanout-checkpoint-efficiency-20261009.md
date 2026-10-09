# Graph fan-out observability, checkpoint efficiency and 5b preparation (2026-10-09)

Track C2 of Point 5 Wave 1. Branch `perf/graph-fanout-checkpoint-efficiency`, base `origin/main` (`58abb650c`).
It changes the existing Parallel (`graph.parallel`) runner and the shared PostgreSQL checkpointer, adds spans to
Map, and adds two unwired mechanisms for Wave 2 (progress coalescer, LLM failure classifier). Both admission gates
stay `false` (`FIXED_PARALLEL_INTEGRATION_READY`, `MAP_INTEGRATION_READY`). The batched application-family
activation is live today: every pipeline with nested applications uses it (`agents/session.rs:1199`).

## Business behaviour

| Topic | Current platform (reference only) | New platform after this change |
|---|---|---|
| Sub-agent fan-out bookkeeping | The legacy supervisor writes parent state per park/reconcile round; no bound on parent writes. | At most 2 parent rows per activation visit, enforced in code with a typed refusal. Pause cards ride on the pause row. |
| Child start | One task per child, sequential state reads. | One batched writer activation and one batched receipt read for all children of an activation. |
| Observability | Celery/pylon logs, no fan-out spans. | `graph.fanout.activation` / `graph.fanout.child` spans and short lifecycle logs with safe fields; `agent.checkpoint.persist` carries `payload_bytes`, `round_trips`, `pool_wait_ms`. |
| Streamed child output | One socket frame per delta. | Unchanged in this PR. The per-activation coalescer (250 ms / 64 KiB) is built and tested, and is wired by the Wave 2 runner behind the V2 flag. |
| Model failure retry | Retries whole agent turns. | Unchanged in this PR. The pre-output classifier is built and tested, and is wired by Gate 5b. |

Deliberately **not ported**: per-round parent writes, the park/reconcile/replay supervisor, and per-delta frames.

## Rust code layout (ADR-0027)

| Mechanism | Owning crate | Path |
|---|---|---|
| Fan-out limits and budgets (`MAX_PARALLEL_BRANCHES` 16, `MAX_FANOUT_CHILDREN` 64, `MAX_CHILD_THREADS` 129, `MAX_PARENT_ROWS_PER_VISIT` 2, `MAX_PREPARE_TRANSACTIONS` 2, `MAX_CHILD_TRANSACTIONS` 2, latency budgets) | `libs/rust/agent-runtime` | `src/graph/fanout_budget.rs` |
| Progress coalescer | `agent-runtime` | `src/graph/fanout_progress.rs` |
| Fan-out span and log types (safe fields only) | `agent-runtime` | `src/graph/fanout_trace.rs` |
| Pre-output LLM failure classifier | `agent-runtime` | `src/graph/llm_failure.rs` |
| Parent-row accounting, staged pause cards, receipt-proven child outcomes | Worker | `src/agents/graph/parallel_checkpoint.rs` |
| Head probe, identity append, batched activation and read, I/O accounting | Worker | `src/state/postgres_checkpointer*.rs` |

The Worker re-exports `fanout_budget` and `fanout_trace` (`agents/graph/mod.rs`) and asserts its own limits against
the shared ones at compile time: Map `MAX_ITEMS` (`map_reduce.rs:27`), Parallel `MAX_FIXED_BRANCHES` and
`MAX_BRANCH_THREADS` (`node_events_parallel.rs:14`). The batched bounds derive from them (`child_batch.rs:23,25`).
No new dependency; `Cargo.toml` and `Cargo.lock` are unchanged in both workspaces.

## Changed paths

| Path | Change |
|---|---|
| `libs/rust/agent-runtime/src/graph/{fanout_budget,fanout_progress,fanout_trace,llm_failure}.rs`, `graph/mod.rs` | New shared modules (above). |
| `src/agents/graph/parallel.rs:159` | `ParallelChildCheckpointerFactory::prepare_children` (default: per child; PostgreSQL: batched). `ParallelChildRequest`, `PreparedChildCheckpoint`. |
| `src/agents/graph/parallel.rs:199,263,277` | `ParentHead`, `ParentSaveProbe`; `ParallelCheckpointAppender::probe_parent` and `append_after_head` with safe defaults (a default probe keeps the full row as a snapshot so no store loads twice). |
| `src/agents/graph/parallel.rs:709` | `restore_branches` prepares every child through one `prepare_children` call. |
| `src/agents/graph/parallel.rs:926,943` | A child's completion and pause are proven by its own fenced save (`last_saved`), not a reload. |
| `src/agents/graph/parallel.rs:1024,1363`, `map_reduce.rs:394,619,656` | Activation and child spans and lifecycle logs. |
| `src/agents/graph/parallel_checkpoint.rs:71,81,99` | Visit accounting: `open_visit`, `charge_row` (typed `graph.parallel.parent_write_budget_exhausted`), `staged_for`. |
| `src/agents/graph/parallel_checkpoint.rs:202` | `record_pause` stages the cards for the ADK pause row instead of appending its own row. |
| `src/agents/graph/parallel_checkpoint.rs:206,220` | `record_decisions` records the exact cards the decisions answered. |
| `src/agents/graph/parallel_checkpoint.rs:470` | Wrapped parent save: one head probe; state read only for a frontier re-save (`head_state`, `:319`). |
| `src/agents/graph/parallel_checkpoint.rs:563,577,645` | `SavedBranchRow`; ADK's second empty start probe reuses the first, fenced answer once. |
| `src/state/postgres_checkpointer.rs:1201,1256,1272` | `persist_scoped` (span fields), `begin_transaction` (pool wait), `CheckpointIoCounters`. |
| `src/state/postgres_checkpointer.rs:1372` | `LatestIdentity` append condition. |
| `src/state/postgres_checkpointer/parallel_append.rs:78,109,222` | Identity compare under the writer lock; metadata-only head probe. |
| `src/state/postgres_checkpointer/child_batch.rs:41,173` | Batched activation (`INSERT … SELECT unnest … ON CONFLICT`) and `DISTINCT ON` read. |
| `src/state/postgres_checkpointer/application_children.rs:76,96,315` | Application families activate in one transaction; batched `prepare_children` for the production factory. |
| `src/state/postgres_checkpointer/{parallel_children,map_children,node_attempts}.rs` | Shared counters; PostgreSQL `prepare_children`. |
| `src/agents/pipeline/scope_receipts_parallel.rs:84,92,203` | Receipt authority forwards the probe and batched preparation; retains receipts from the head's metadata. |

## Performance

Budgets come from `fanout_budget.rs` and are asserted by tests. Before/after counts are per wrapped parent save and
per child, from the I/O counters; "before" is the code on `origin/main` read path by path.

| Budget | Mechanism | Proving test | Result |
|---|---|---|---|
| ≤ 2 parent rows per activation visit | `parallel_checkpoint.rs:81` refuses a third row; pause cards staged (`:202`) | `each_activation_visit_appends_at_most_two_parent_rows`, `a_third_parent_row_in_one_visit_is_refused_with_a_typed_error` (in memory); `every_visit_appends_at_most_two_parent_rows_across_crash_windows` (PostgreSQL) | Join visit 2, pause visit 2, resume visit 2. **Fails before:** with the old separate pause-card row both budget tests fail (`3 != 2`). |
| 2 transactions to prepare N children | `child_batch.rs:41,173`; `application_children.rs:315` | `batched_preparation_costs_two_transactions_and_refuses_a_superseded_claim`, `application_families_prepare_in_two_transactions_and_keep_their_own_threads`; restore loop in the load test | 16 children: 2 transactions, 8 round trips. 64 children: 2 transactions every time. Before: 2 per child (activation + load), so 32 for 16 children. |
| ≤ 2 transactions per fresh child | ADK start probe + terminal save; verify reload gone (`parallel.rs:926,943`), second empty probe reused (`parallel_checkpoint.rs:645`) | `a_completed_child_is_proven_by_its_fenced_save_without_a_reload` (child loads counted), load test asserts 24 transactions for one 8-child activation | 2. Before: 4 (prepare load, two ADK empty probes, verify reload) plus its own activation. |
| Wrapped parent save reads no state | `parallel_checkpoint.rs:470`, `parallel_append.rs:109` | load-test transaction totals; span capture | 1 head read (metadata only) + 1 append. Before: by-id load + full latest load + append that re-read and re-serialized the parent (3 full reads, 2 serializations). |
| 0 pool-acquire timeouts at 32 executions × 8 children | Batched preparation; pool sized 32 / 5 s like production | `fanout_load_meets_the_postgres_budgets` | 0 failures and max 2 parent rows in all 3 release runs (wall 428 / 971 / 666 ms). |
| Per-child overhead p99 ≤ 30 ms; restore of 64 children p95 ≤ 500 ms | As above | Same test; wall-clock asserts gated by `ELITEA_FANOUT_LATENCY_BUDGETS=enforce` | Release build, local PostgreSQL 18 in Docker, shared host (load average 11–12 on 16 cores, other stacks running), 96 child samples per run: p50/p99 = 11.0/**21.2**, 16.1/33.6, 24.0/256 ms; restore of 64 p95 = 11.2 / 23.1 / 12.3 ms. **Restore met in every run; per-child p99 met in 1 of 3 runs.** The transaction count was identical in every run (24 per 8-child activation), so the spread is host contention, not work. Not reliably demonstrated on this host; see the follow-up. |
| Coalescing: ≤ 4 progress frames/s per activation, one frame ≤ 64 KiB, lifecycle frames immediate, byte-exact | `fanout_progress.rs` (unwired) | `rate_budget_and_lossless_reassembly`, `byte_bound_at_limit_and_limit_plus_one`, `multibyte_split_stays_valid_and_byte_exact`, `lifecycle_flushes_buffer_first_at_same_instant`, `flush_deadline_is_window_start_plus_interval` | 8 children × 30 deltas/s × 10 s: ≤ 41 frames, ≤ 4 per second, lossless. |

**C2e (`spawn_blocking` for ≥ 256 KiB)** and **C2f (writer-row byte counters)**: **not done**, per the brief's condition.
- C2e: the child path that misses the p99 has no (de)serialization ≥ 256 KiB. Children carry small states, so moving
  large-payload work to `spawn_blocking` would not change the measured child p99. The large work is the 512 KiB
  parent's freeze, read and append, which runs before and after the children (8-child activation p50 60–104 ms in
  release).
- C2f: in this load each child thread has 1–3 rows, so the O(rows) capacity scan is not the cost.

Both stay follow-ups with the numbers above.

## Durability

Crash windows this change touches, and the recovery rule for each (real PostgreSQL, process replacement = a new
claim on the same root):

| Window | Recovery rule | Proof |
|---|---|---|
| After freeze and batched activation, before any child ran | The replacement re-activates idempotently (same rule: same claim or newer) and runs every child once | `every_visit_…crash_windows` (activation window) |
| Children completed, join row not written | Receipts replay; no child re-runs; the join is the visit's second row | same test (after-children window): run counts 1 each, 2 rows |
| Pause computed and cards staged, pause row not saved | Nothing was written; the replacement derives the same cards from child receipts and writes one pause row | same test (pause-lost window): 1 row before, 2 after, same card |
| Pause row saved, decisions applied under a new execution | Decision row records the answered cards; completed children do not re-run | same test (pause/resume): `n0` 1 run, paused children 2 |
| Two writers append against one parent head | Identity compare under the exclusive writer lock admits exactly one | `concurrent_parent_appends_against_one_head_admit_exactly_one` (8 rounds; stale head refused) |
| Takeover during preparation | The superseded claim's batch is refused (`writer_not_current`), its activated child writers are fenced | `batched_preparation_costs_two_transactions_and_refuses_a_superseded_claim` |

### Recovery guarantee (replatform delivery gate §2): component × fan-out phase

| Component down during | Before | After | Enforcing code | Proof |
|---|---|---|---|---|
| Worker, during child preparation | R | **R**, unchanged class; the batch is all-or-nothing and idempotent | `child_batch.rs:41` | activation crash window |
| Worker, after children completed, before join | R | **R** | receipts + `parallel_checkpoint.rs:470` | after-children window |
| Worker, after the pause was computed, before its row | R (row written early) | **R**: cards re-derived from child receipts; no parent row lost because none was written | `parallel_checkpoint.rs:202` | pause-lost window |
| Worker lease lost / takeover mid-preparation | R | **R**: typed `writer_not_current`, control stop | `child_batch.rs:95,144,235` | takeover test |
| PostgreSQL unavailable mid-batch | F (typed, retryable) | **F**, unchanged: one transaction rolls back as a whole; `storage_unavailable` is retryable | `begin_transaction` | — |
| Main / NATS / sandbox supervisor / gateway / Web | Unchanged | Unchanged | — | — |

## Resilience

| Rule | Mechanism | Proving test |
|---|---|---|
| Bounded batches | `MAX_BATCHED_WRITERS` = 16 × 129, `MAX_BATCHED_READS` = 64, derived from shared limits | Typed `resource_exhausted` on empty/oversized input (`child_batch.rs:47,179`) |
| Typed refusal past the parent-row budget | `charge_row` → `graph.parallel.parent_write_budget_exhausted` | `a_third_parent_row_in_one_visit_is_refused_with_a_typed_error` |
| Stale staged cards are refused, never attached to a moved parent | `staged_for` compares the staged parent id | `a_pause_receipt_cannot_attach_to_an_advanced_parent_frontier` |
| Coalescer bounds | ≤ 64 members, ≤ 64 KiB buffered, typed member errors | `invalid_members_are_typed_errors_without_state_change` |
| Control stops are never business failures | `ChildOutcome::LeaseLost`/`Cancelled`; activation outcome by error code | log capture; existing C1 lease tests (unchanged, passing) |

## Security

| Threat | Mechanism | Proving test |
|---|---|---|
| Cross-scope reads/writes in batched SQL | Every statement binds tenant, both projects, capability, family, definition; `same_claim` requires every child authority to share the parent's claim fields; returned rows outside the request set are `CorruptStoredState` | `application_families_…_keep_their_own_threads` (foreign thread write refused), `batched_preparation_…` |
| A superseded claim writes after takeover | Root writer `FOR SHARE` + claim match, the single-thread `ON CONFLICT … WHERE` rule, returned-set equality, `FOR SHARE OF writer` on reads | takeover test |
| Accepting a stale or foreign parent | Identity `(checkpoint_id, save_ordinal)` under the exclusive writer lock; rows are immutable per id (exact-existing check) | `identity_comparison_refuses_another_latest_row`, concurrent append test |
| Prompts, item data, ids or payloads in logs and spans | `fanout_trace` accepts only static labels, counts, ordinals and the node id; checkpoint spans record counts only | `fanout_and_checkpoint_spans_carry_counts_and_no_sensitive_values` (child-process capture; asserts no prompt marker, `p1:` thread ids, claim ids, SQL text or tenant id) |
| Provider text in classification | `classify_adk_error` reads the category only | `adk_error_is_classified_by_category_only` |
| SQL injection | Bound parameters only (`unnest($7::text[])`, `ANY($7::text[])`) | review |

`rules/security.md` items: identity from verified claims only (unchanged); object-level fencing in the same
transaction as the effect (yes); bounded input (yes); parameterized SQL only (yes); no secrets/payloads in logs
(tested); no egress, templates, shell, URL or path building touched (not applicable); no new dependency.

**Reviews.**
- `code-review` (high): 9 findings. Fixed 4: test for the production batched application-family preparation;
  the receipt authority no longer re-probes the candidate per save; outcome label by error code; unused coalescer
  argument. Skipped 5 with reasons: Map wrapper head probe (Map's execution-filtered turn authority needs its own
  head equivalent, Wave 2 unified runner); revision-path PG test (logic unchanged, uses the full row as before);
  `ParentHead::of` copy (test-only default path); production counters (two relaxed atomics, a test seam);
  cached empty probe (only ADK loads the branch thread; the first probe is fenced).
- `security-review`: no finding with confidence ≥ 8 (SQL, isolation, fencing, identity compare, logging,
  deserialization checked).

**Audits.** `cargo deny --all-features check advisories`:
- Worker: RUSTSEC-2023-0071 (`rsa` via `sqlx-mysql`, no fix) — pre-existing, identical on main.
- `libs/rust`: RUSTSEC-2023-0071 and RUSTSEC-2024-0436 (`paste` unmaintained) — pre-existing.
- New: none (no manifest or lockfile change).

## Tests

- New tests: 22.
  - `libs/rust/agent-runtime`: 7 coalescer, 4 classifier.
  - Worker in memory: 3 Parallel budget/proof tests (`parallel_tests.rs`); 1 identity-compare unit test replaces
    the full-content compare test in `parallel_append.rs` (the empty-thread test is kept, rewritten for identity).
  - Worker real PostgreSQL (`state/postgres_checkpointer_tests/fanout_load.rs`): 7 (prepare/takeover, application
    families, concurrent append, crash windows, load, span capture + its child).
- Updated: `a_pause_receipt_cannot_attach_to_an_advanced_parent_frontier` now asserts the refusal at the ADK pause
  save, where staged cards meet the parent.
- `libs/rust`: `cargo test --offline --locked --workspace --all-targets --all-features` **1018 passed, 0 failed**;
  `-p elitea-agent-runtime --features toolkit-sql` **521 passed**; `--features test-preserve-order,toolkit-sql`
  **521 passed**; `fmt --check` and `clippy --workspace --all-targets --all-features -D warnings` clean.
- Worker: `cargo test --offline --locked --all-targets --all-features` with a disposable PostgreSQL 18,
  `ELITEA_REQUIRE_POSTGRES_RECEIPT_TESTS=1` and `ELITEA_TEST_DATABASE_GUARD=disposable-pg18`: **1843 passed,
  0 failed, 71 ignored** (pre-existing `#[ignore]` suites run by separate CI steps; none new). `fmt --check` and
  `clippy --all-targets --all-features -D warnings` clean.
- Fixtures: every PostgreSQL test builds its own isolated database (`IsolatedPostgres`) and claims through
  `CheckpointWriterAuthority::new`; no shared database was touched.

## Real-browser evidence

Parallel and Map are not composed in production (`ParallelCompilerBinding::new` has no production caller; both
gates stay `false`), so their positive browser proof remains the gate-flip criterion (`PLAN.md` §4). The browser
run therefore covers the C1 regression set over the paths this change makes live: the shared PostgreSQL
checkpointer (identity append, I/O span fields) and the batched application-family activation that every pipeline
with nested applications uses (`agents/session.rs:1199` → `with_application_paths`).

**Stack.** A private standalone stack, compose project `elitea-c2-20261009`, browser host
`http://c2.localhost:18320/app/`, NATS command bus, built and run per the real-model runbook.
- Database: restored from `product-real-models-main-c0f2e5f9b.dump` (shared 158, tenants 148). No seeding.
- Main, Web, gateway, scheduler, subapp-host: `main-c0f2e5f9b-verify`. This branch's base `58abb650c` is
  `c0f2e5f9b` plus a documentation-only commit.
- Worker: `ghcr.io/elitea-ng/elitea-worker-rust:c2-20261009-rehearsal` (`sha256:f6d2fd7909d2…`), built from this
  branch at `7d2a3210f` with `WORKER_REHEARSAL_FEATURES=graph-extensions-rehearsal`.
  - §3b binary check: `docker create` + `docker cp`, then `grep -a` found the Worker strings
    `graph.parallel.parent_write_budget_exhausted` and `the batched child activation count is outside its bound`.
  - It also found the `agent-runtime` strings `graph.fanout.activation` and `Fan-out child admitted`.
- Sign-in: the stack's mock OIDC as `admin@centry.user`, project "Private" (id 2).
- Model: the dump's configured `vllm/CONTINUATION-REPAIR-FIXTURE` route through the gateway. No response mocks.

**Fixtures.** Every fixture is a pre-existing entity created through the UI by earlier sessions; none was created
or edited from the database. The database was read only to find ids and settlement states.

| # | Flow | Chat / executions | Result on this branch | After reload |
|---|---|---|---|---|
| A | Pipeline "Gate5 typed parent" (136) calling the saved child pipeline (135) | chat 880, `ec8d1f64` | **Pass.** `4\|2\|3\|for orders\|2\|True`, identical to C1. `SUCCEEDED`, `executed_settled_acked`. Live spans: `operation="activate_children" round_trips=4` (the batched family activation), `save … payload_bytes=1140…1292 round_trips=7`, `load … round_trips=4`, `pool_wait_ms` recorded. | Same output |
| B | Agent "Full Name Resolver" (9): two parallel nested Applications; the surname child pauses on an auth card; Skip Auth | chat 881, `8c8cc683` → `80ddc0cb` | **Pass.** `tool_execution_mode="parallel_applications"`, pause, `direct_hitl_resume`, one combined answer, `executed_settled_acked`. | Card kept while paused; one answer after |
| C | B with `docker restart` of the Worker while paused, then reload, then Skip Auth | chat 883, `263072c0` → `c3206ac7` | **Pass: R across process replacement.** The restarted Worker (same image) resumed and settled `executed_settled_acked`; one combined answer (Marco Rossi). | Card kept across the restart |
| D | Pipeline "Gate5 agent variable parent" (138): two nested agent nodes, each asks the user (two sequential cards), no restart | chat 886, `f11e97ad` → `8561b236` → `97a24318` | **Pass.** Both cards answered; both calls returned (`OVERRIDE CALL` / `DEFAULT CALL`). | Answers kept |
| D′ | D with a Worker restart while the first card is open | chat 884, `1f3e7bb3` → `330af5c8` → `9b189c5c` | First resume after the restart **passes**: the new claim re-activated the family with `activate_children` (4 round trips) and settled. The second resume **fails**: `native_agent.invalid_configuration` ("the nested application graph is invalid"). **Pre-existing:** the identical sequence on the `main-c0f2e5f9b-verify` Worker on this stack fails the same way (chat 887, `8c6af8f6`). Tracked separately. | Failure message kept |
| E | Rehearsal-gated SplitOut pipeline "verify-main gate splitout" (161) | chat 888 | Not runnable here: Main's start admission (production image) refuses `split_out`. The Main, Web and Worker rehearsal flags flip together, and this belongs to Track A's gate, not this track. | — |

**A/B against main on the same stack.** The Worker container was switched to `main-c0f2e5f9b-verify` and back, with
only the image changed (`docker compose up -d --no-deps elitea-worker`). Flow B on main (chat 882) shows the same
`session.invalid_scope` errors on `agent.session.persist operation="get"` and the same 3×
`agent_session_terminal_completion_unavailable` warnings seen on this branch. Both come from the session store,
which this change does not touch, so they are pre-existing.

**Telemetry.** No secrets, prompts or ids appear in the new span fields. The checkpoint span lines carry only
`backend`, `operation`, `outcome`, `payload_bytes`, `round_trips` and `pool_wait_ms`; the log-capture test proves
the same for the fan-out spans. Agent chats (B, C) persist through the session store, so they emit no
`agent.checkpoint.persist` spans, as expected.

**Teardown.** The stack was removed after the run: `docker compose -p elitea-c2-20261009 … down -v`.

## Open limits and follow-ups

1. **Map** keeps per-item activation and its wrapper's two full parent reads per save; the unified Wave 2 runner
   moves Map onto `prepare_children` and the head probe (`MapTurnAuthority` filters loads by execution, so it needs
   its own head equivalent).
2. **Wire the coalescer** in the Wave 2 runner behind the V2 flag, with the aggregated browser frame.
3. **Wire the classifier** in Gate 5b (`llm.rs` emits the model failure only from terminal reporting).
4. **ADK's two empty start probes at a new root parent** (1 extra transaction on a first visit) are left as is.
5. The wrapped save still sends the full candidate twice (exact-existing compare, then insert); a single
   `INSERT … ON CONFLICT` with a byte compare would halve the bytes. Measure before changing.
6. The capacity scan per save is O(rows) (`ensure_save_capacity`); C2f counters remain the fix if long pipelines
   show it.
7. **Pre-existing, tracked separately:** after a Worker restart between two pauses of a pipeline with two nested
   agents, the second continuation fails assembly with `native_agent.invalid_configuration` (identical on main).
   Session-store `session.invalid_scope` / `agent_session_terminal_completion_unavailable` signals also occur on main.
8. **Per-child p99 ≤ 30 ms was met in 1 of 3 release runs on the shared host.** Re-run
   `ELITEA_FANOUT_LATENCY_BUDGETS=enforce cargo test --release … fanout_load_meets_the_postgres_budgets` on a quiet
   host or CI-like runner. CI runs the deterministic budgets only (rows, transactions, failures).
