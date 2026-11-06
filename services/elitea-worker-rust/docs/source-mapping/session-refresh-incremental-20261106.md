# Incremental root-session refresh (FU-287) - 2026-11-06

Branch `perf/session-refresh-incremental` from `main` `8d4edfcfd`; head `a65a68671` when this was written. Worker and
vendored `adk-runner` only. No Main, proto, Helm, compose or CI change, and no migration. Plan:
`handoffs/point5-wave1-20261008/fu287/PLAN.md` (candidate 1, ordinal-based). Delivery gate:
`rules/replatform-delivery-gate.md` sections 2, 3 and 3a. Security checks: `rules/security.md`.

## 1. Business behaviour taken from the current platform, and what was not ported

Reference: the current platform runs agents through the same ADK runner (`elitea-sdk` and its indexer service). The current
platform has no incremental refresh; its behaviour is the contract this change keeps.

**What the runner's refresh guarantees (unchanged).** After every persisted non-partial event the vendored runner
replaces its in-memory history with the session's *complete, identity-scoped model view*
(`libs/rust/vendor/adk-runner/src/runner.rs:245`, called at `:1086` main loop and `:1370` transfer loop). That view is
what a full `PostgresSessionService::get` plus `runner_history::project` returns:
- **Retirement.** Only the active rows: a superseded recovery marker is out; a history snapshot cuts the user and agent
  rows before it; a first branch row, a control row or a row of another agent keeps rows in.
- **Snapshots.** The latest history snapshot is spliced in once (`runner_history.rs:64`).
- **Compaction.** The durable context-compaction record projects its replacement view (`runner_history.rs:112`).
- **Confirmation content** is stripped from the model view (`runner_history.rs:47`).
- **Identity.** One session, one bound identity.
- **Failure.** A storage failure stops the run; a stale view is never served.

**What changed.** How the Worker produces that view. A refresh reads only the rows after its cursor (ordinal, under
the shared writer lock) and applies the retirement rules to them, instead of parsing every stored payload again.

**What deliberately did not change.**
- No migration, no new column, no new table: the per-session ordinal and `next_event_ordinal` already exist
  (`services/elitea-main/migrations/agentstate/0002_agent_sessions.sql`).
- An **unset hook is a byte-identical runner**: the optional `SessionEventRefresh` defaults to `None`, and then
  `refresh_events_from_storage` does exactly today's full `get`. `InvocationLocal` (in-memory) sessions never set it.
- The projection code is shared, not copied: both paths end in `runner_history::project_events`.
- The initial session read, the append path's rules, Main's contracts and the wire formats.

## 2. Changed paths

| Path | Change |
|---|---|
| `libs/rust/vendor/adk-runner/src/runner.rs:27` | `SessionEventRefresh` trait; `:229` `with_session_event_refresher`; `:245` the refresh uses it when set; `:1086`, `:1370` both call sites pass it. |
| `libs/rust/vendor/adk-runner/src/lib.rs:65` | Exports the trait. |
| `libs/rust/vendor/README.md:142` | Patch note beside the other local patches. |
| `libs/rust/agent-runtime/src/lib.rs:114` | Compile guard: a build without the patched runner fails to compile. |
| `src/state/postgres_session_classified_events.sql` (new) | Row-local classification with an ordinal lower bound inside `scoped`. |
| `src/state/postgres_session_active_events.sql` | The global retirement rules only. |
| `src/state/postgres_session.rs:769` | `lock_writer_row` returns `next_event_ordinal` (the head) from the existing `FOR SHARE`/`FOR UPDATE` statements (`:2289`, `:2310`). |
| `src/state/postgres_session.rs:1057`, `:1071` | `events_since` / `read_events_since`: the new rows and the head in one short transaction; bounded before decoding. |
| `src/state/postgres_session.rs:1136`, `:1150` | `seed_active` / `read_active_seed`: the active rows with their classification. |
| `src/state/postgres_session.rs:269`, `:277`, `:302`, `:318` | One-time (`LazyLock`) SQL text for the classified, active, tail and seed queries. |
| `src/state/postgres_session.rs:1367` | `ensure_event_capacity`: the stored-totals fast path (reviewer-approved, commit `2296f1778`). |
| `src/state/postgres_session.rs:1726`, `:1740` | `session_read_error`, `refresh_span` (span `agent.session.persist`, `operation = refresh`, fields `rows_read`, `fallback`). |
| `src/agents/runner_history.rs:12`, `:47` | `read` and `project_events`, shared by the full and incremental paths. |
| `src/agents/runner_view.rs:268` | `IncrementalRunnerView` (the cache, `refresh` `:340`, `refresh_locked` `:361`, `refreshed_events` `:438`); `ViewFacts` `:104` (the mirrored rules). |
| `src/agents/session.rs:2388` | `RunnerSessionService::refreshing`; the three Runner build sites call it: pipeline `:3009`, ordinary `:3733`, direct HITL resume `:4238`. |
| `src/agents/ordinary.rs:326`, `:437` | The concrete Postgres session travels to the Runner assembly in `OrdinaryRuntimeBindings`. |
| `src/agents/context_compaction.rs` | `extend_digest` is `pub(super)` (a test builds a real compaction record). |
| Tests | `src/agents/runner_view_pg_tests.rs`, `session_capacity_pg_tests.rs` (new), `native_history_tests.rs`, `pipeline_fanout_effect_pg_tests.rs` (measurement tests only). |

## 3. Tests (counts and skips)

**New tests, with what each proves.**

`src/agents/runner_view_pg_tests.rs` (real PostgreSQL; the `runner_view` filter runs 18 tests):
- `generated_event_sequences_give_the_same_view_incrementally_and_in_full`: 6 seeded sequences of 70 steps with user,
  agent, other-agent, control, marker, snapshot (two agents), branch, branch-marker, compaction-delta and exact-replay
  events; after each step the served view, the cached rows and the compaction record equal the full read.
- `mirrored_retirement_rules_equal_the_sql_rules_including_null_author_and_branch`: raw rows with NULL author and NULL
  branch (not decodable as an `Event`, so only SQL sees them) keep the same ordinals under the SQL `active_events`
  and the Rust mirror.
- `each_refresh_reads_only_the_new_rows_and_a_whole_run_at_most_twice_the_rows`: the rows-read CI gate (section 5).
- `a_snapshot_cut_a_first_branch_and_another_agents_snapshot_each_read_in_full_once`: a cut and a same-agent snapshot
  need no re-read; the first branch row after a cut re-reads exactly once.
- `a_snapshot_of_another_agent_brings_back_rows_and_reads_in_full`: rows dropped behind a cut return, by a full read.
- `compaction_between_refreshes_gives_the_full_projection`: a real compaction record yields the same `context-view-*`
  projection as the full read.
- `a_concurrent_foreign_append_is_never_lost_or_duplicated`: 60 appends from another task during refreshes; none lost,
  none duplicated, in order, and none waited past 5 s.
- `the_writer_lock_is_released_when_a_refresh_returns`: after each refresh `FOR UPDATE NOWAIT` on the writer row
  succeeds and no connection is idle in transaction.
- `limit_and_limit_plus_one_active_rows_follow_the_full_read`: five active rows are served, six give the full read's
  typed `session.resource_exhausted`, and nothing stays resident.
- `an_active_view_over_the_byte_bound_is_read_in_full_and_not_kept`: a view over the retained-byte bound is served by the
  full read and not cached.
- `a_tail_over_the_event_or_byte_bound_is_not_decoded`: limit and limit+1 on events and on bytes; an undecodable row
  inside the tail is hit within the bound and never touched over it.
- `a_view_passing_the_byte_bound_reads_in_full_at_once_and_caches_again_when_it_fits`: no discarded seed decode; the
  probe while still over decodes no row; a snapshot cut brings the view back to incremental.
- `a_replayed_settle_changes_nothing_and_a_changed_one_conflicts`: an exact replay allocates no ordinal and reads no row;
  a changed one is `session.event_conflict`.
- `a_refresh_after_the_lease_moved_is_a_typed_failure_with_no_stale_view`, `a_revoked_local_lease_stops_the_refresh`:
  `session.writer_not_current`.
- `a_view_serves_only_its_own_session`: a foreign session or user is `session.not_found` and reads nothing.
- `the_runner_assembly_refreshes_incrementally_and_reports_rows_and_fallback_in_its_span` (with the child test
  `refresh_span_child`, in a child process with a global subscriber): a Runner built through `refreshing` makes one
  `agent.session.persist` / `refresh` span per refresh carrying `rows_read` and `fallback`.

`src/agents/session_capacity_pg_tests.rs` (4 tests, 1 ignored):
- `stored_totals_bound_the_active_totals_and_the_fast_path_decides_like_the_exact_check`: 3 seeds x 45 steps; stored
  count and bytes bound the active ones from the real SQL at every step; the fast-path decision equals the exact
  classification at caps below, at and above both the stored totals and each candidate's exact turning point
  (about 1,300 decisions per seed). A deliberate off-by-one in the fast path made it fail.
- `no_worker_source_deletes_or_truncates_stored_session_events` and
  `the_guard_recognizes_every_spelling_of_a_delete_or_truncate`: no non-test source deletes or truncates the table; the
  failure message says the fast path relies on append-only rows and must be revisited.
- `the_event_table_migrations_have_no_delete_trigger_or_rule`: the `agentstate` migrations add no trigger or rule on
  the table; the only deletion path is the `ON DELETE CASCADE` of a whole session.
- `measure_long_chat_append_latency_past_the_stored_caps` (ignored, manual): section 5.

`src/agents/native_history_tests.rs` (vendored hook):
- `session_event_refresher_is_optional_and_replaces_only_the_full_read`: no refresh, a full-read refresh and a
  hook-served refresh deliver the same events, store the same events and show the model the same history; the hook is
  asked once per persisted non-partial event, and only in the hook run.
- `session_event_refresher_is_asked_in_the_main_loop_and_the_transfer_loop`: both loops call the hook.

**Whole-suite cross-check (test-only).** `runner_view::cross_check_enabled` (`runner_view.rs:454`, on by default in
every test build): each refresh also runs the full read and compares the events one by one; it skips the comparison
only when another writer appended between the two reads. In this build it covers every in-process Runner that goes
through a PostgreSQL session: the Parallel, Map and FU-269/FU-265 proofs in `pipeline_fanout_effect_pg_tests.rs`, and
all `runner_view_pg_tests`. Two-process phases inherit the same flag in their child; I could not show an in-process
refresh in `ordinary_effect_pg_tests` or `pipeline_scoped_pg_tests`, so for the ordinary and direct-resume sites the
evidence is the shared `refreshing` wiring and the assembly test above. The two timing tests switch it off with a
guard (`disable_cross_check`, `:467`) that restores it on drop.

**Counts.** `cargo test --offline --locked --no-fail-fast --all-targets --all-features -- --test-threads=4` with the
PostgreSQL env at `a65a68671`: **3121 passed, 0 failed, 110 ignored**. The ignored tests are marked
`#[ignore = "requires ..."]`: sandbox/Docker/Kubernetes live tests (need a runner image, a TLS bundle or a cluster),
tests needing an isolated agentstate database, and measurements (`measure_*`, run by hand). Run with `--ignored`, 30
sandbox tests fail only for missing environment variables (`NotPresent`); none touches the session path. fmt and
clippy `-D warnings` are clean. **Stability:** the `postgres_session` (13 tests) and `runner_view` (18 tests) filters
passed 3 times in a row, with no failure.

## 4. Accepted duplication

The retirement rules exist once in SQL (`postgres_session_classified_events.sql` for the row-local classification and
`postgres_session_active_events.sql` for the global rules) and once in Rust (`ViewFacts`, `runner_view.rs:104`, with
`absorb` `:115`, `from_rows` `:139`, `is_active` `:148`). The reviewer accepted this on three conditions, each a test:
1. the generated-sequence property test, `generated_event_sequences_give_the_same_view_incrementally_and_in_full`;
2. the SQL-versus-Rust NULL author and NULL branch test,
   `mirrored_retirement_rules_equal_the_sql_rules_including_null_author_and_branch` (its oracle calls the production
   `ViewFacts`, not a copy);
3. the whole-suite cross-check (section 3).

Row-local classification is never recomputed in Rust: the view uses the columns the SQL returns.

## 5. Performance

**Mechanism.** A refresh is BEGIN (with timeouts), lock + head, at most one range scan of the new rows, COMMIT:
`runner_view.rs:361` and `postgres_session.rs:1071`. Rows decoded: the new rows only.

**Rows-read CI gate (deterministic).** `each_refresh_reads_only_the_new_rows_and_a_whole_run_at_most_twice_the_rows`:
64 steps of two or three rows; each refresh reads exactly its k new rows, the run reads at most 2n, one seed, zero
fallbacks. In the 64-item Map: 321 refreshes, 322 rows read, 0 fallbacks (asserted at most 1 in the measurement test,
from the views of that run's own database).

**64 saved Agent items, 100 ms model (`measure_64_saved_agent_items_with_model_latency_at_concurrency_{8,1}`).**

| Stage | Concurrency 8 | Concurrency 1 |
|---|---|---|
| Before FU-287 (view off, same build and machine) | 8.84 s | 11.49 s |
| After the refresh only | 3.82 s | 9.93 s |
| After the capacity fast path (3 rounds) | 1.91, 2.07, 2.33 s | 9.25, 9.33, 8.84 s |

The three rounds ran at a load average of up to 30, so they are noisy. The plan's targets were 8: at most 2.0 s and
1: at most 7.5 s.

**Per-span profile of the run after the refresh change, concurrency 8** (temporary span timing, one run of 4.27 s
including the profiler; quarters are the first to fourth quarter of each span's own occurrences):

| Span | Count | Total | Mean | Mean by quarter (ms) |
|---|---|---|---|---|
| `graph.fanout.child` | 64 | 18.4 s | 287 ms | 190, 173, 335, 450 |
| `agent.pipeline.application_node` | 64 | 16.3 s | 255 ms | 171, 160, 304, 386 |
| `agent.nested_application.invoke` | 64 | 8.0 s | 125 ms | 124, 119, 124, 132 |
| `agent.model.request` | 64 | 6.5 s | 100.9 ms | flat |
| `agent.session.persist[append_event]` | 450 | 3.79 s | 8.4 ms | 5.2, 5.8, 8.5, 14.2 |
| `graph.fanout.activation` | 1 | 2.71 s | - | - |
| `agent.checkpoint.persist[save]` | 256 | 0.92 s | 3.6 ms | flat |
| `agent.session.persist[refresh]` | 321 | 0.71 s | 2.2 ms | 2.2, 1.7, 2.1, 2.8 |
| `agent.checkpoint.persist[load]` | 259 | 0.64 s | 2.5 ms | flat |

Spans nest, so totals do not add. The model stub is flat, a 4 to 16 connection pool changes nothing, and refresh,
save and load are flat. **Root cause:** every append ran the full classification over all stored payloads just to count
active rows and bytes (the exact query at `postgres_session.rs:1420`, called from the append at `:1333`). It grows with
the stored history (append 5 to 14 ms in the body; 14 to 20 ms for the 64 serial result appends of the join, about
1.2 s after the last model call). The capacity fast path (`postgres_session.rs:1373`) removes it while the stored
totals are within the caps.

**Long chat past the stored caps** (`measure_long_chat_append_latency_past_the_stored_caps`, ignored; one session,
default `SessionLimits`, per turn a user, an agent and a recovery marker with a 64 KiB request; the two arms
alternate append by append on the same session and database, n = 100 per arm). Times in ms.

| Phase | Run | Fast path on: mean / p50 / p95 | Off: mean / p50 / p95 | Mean diff |
|---|---|---|---|---|
| Below caps (600 rows, 13.4 MB stored; 401 rows active) | 1 | 8.5 / 8.5 / 11.5 | 68.4 / 67.8 / 81.7 | -87.6% |
| | 2 | 6.2 / 5.7 / 8.2 | 58.6 / 60.2 / 74.9 | -89.3% |
| | 3 | 7.3 / 6.9 / 11.6 | 25.6 / 17.4 / 58.3 | -71.3% |
| Over caps (4200 rows, 93.7 MB stored; 2801 rows, 1.36 MB active) | 1 | 68.2 / 68.0 / 74.8 | 67.8 / 67.5 / 73.6 | +0.6% |
| | 2 | 70.1 / 69.6 / 75.6 | 68.5 / 67.8 / 73.5 | +2.3% |
| | 3 | 66.1 / 65.6 / 70.0 | 65.1 / 64.7 / 71.0 | +1.4% |

Below the caps the fast path cuts an append by about 70 to 90%. Past them it falls through to the exact check and costs
1 to 2% more (the extra stored-totals query). Run 3's off arm below the caps had a low p50 (17 ms against about 60
to 68 ms); I read it as a cold-cache effect. Past the caps an append still costs about 65 to 70 ms; that is FU-294.

**Remaining time.** About 45 ms of flat per-item persistence (child session and checkpoint writes, the join's result
appends) is outside this change: **FU-293**. Durable O(1) stored and active totals, so the exact check never scans:
**FU-294**.

## 6. Durability, Resilience, Security

| Property | Mechanism | Proving test |
|---|---|---|
| **Lock scope.** The shared writer lock (`FOR SHARE`, `postgres_session.rs:2289`) is taken only inside the refresh's own short transaction (`read_events_since` `:1071`, `read_active_seed` `:1150`), run by `driven` (`:560`) so it finishes even if the caller stops polling. It is released at the commit before the view projects anything and so never held across an await on model or tool work. Writers wait at most one refresh transaction (lock wait bounded by `WRITER_LOCK_TIMEOUT`, `:45`). | same | `the_writer_lock_is_released_when_a_refresh_returns`; `a_concurrent_foreign_append_is_never_lost_or_duplicated` |
| **Completeness.** Ordinals are gap-free and committed in order; the head proves the rows are the whole prefix; a gap or non-continuing ordinal re-reads in full (`runner_view.rs:206`). | `ViewCache::extend` | `each_refresh_reads_only_the_new_rows_...`, the generated-sequence test |
| **Bounds.** The cached view is bounded by the full read's caps: more than `max_events` active rows gives the typed `session.resource_exhausted` (the full read's code); the tail is bounded by `max_events` rows and `max_retained_event_bytes` bytes **before any decode** (`:1071` to `:1098`, SQL `:302`); an active view over the byte bound is read in full and not kept, and the next refresh probes the active bytes without decoding them (`runner_view.rs:389`, `:407`) and caches again once it fits. | `read_events_since`, `read_active_seed`, `refresh_locked` | `a_tail_over_the_event_or_byte_bound_is_not_decoded`, `limit_and_limit_plus_one_active_rows_follow_the_full_read`, `an_active_view_over_the_byte_bound_...`, `a_view_passing_the_byte_bound_...` |
| **Fallback rules.** One full read (span field `fallback = true`) when a new row could bring back a dropped one: the first branch row after a snapshot cut, or a snapshot of another agent (`runner_view.rs:228`); when the rows do not continue the cursor; when the tail is too large; when the view passes the bounds. There is no in-memory resurrect path. The first read is a seed, not a fallback. | `ViewCache::extend`, `refresh_locked` | `a_snapshot_cut_a_first_branch_and_another_agents_snapshot_each_read_in_full_once`, `a_snapshot_of_another_agent_brings_back_rows_and_reads_in_full` |
| **Identity fails closed.** A view serves its one bound identity; any other is `session.not_found` before any read (`runner_view.rs:438`). The session service keeps its 10-key scope binds and the writer fence on every statement, and takes no new caller input except an internal `i64`. | `refreshed_events` | `a_view_serves_only_its_own_session` |
| **Lease and takeover.** A moved or revoked claim is `session.writer_not_current`; the cache is not served. | the shared fence and `commit_current` | `a_refresh_after_the_lease_moved_is_a_typed_failure_with_no_stale_view`, `a_revoked_local_lease_stops_the_refresh` |
| **Cache is not durable.** Per run and in memory; a new process, takeover or wake starts from a full read. | by construction | the two-process suites (section 7) |
| **Cross-check is test-only.** `cross_check`, `CROSS_CHECK`, `disable_cross_check` and the per-database stats registry are `#[cfg(test)]` (`runner_view.rs:454` onward); the release build contains none of it. | `cfg(test)` | Release-image binary check (section 8): the Worker binary extracted from image `fu287-a65a68671` (`sha256:350f66524d04…`, `docker create` + `docker cp`) holds 0 matches each for `session.refresh_cross_check`, `cross_check` and `CROSS_CHECK`, and 1 each for `agent.session.persist` and `rows_read`. |
| **Data in logs.** The refresh span carries counts only (`rows_read`, `fallback`); errors are the existing data-free session errors. | `refresh_span`, `postgres_session.rs:1740` | the span test |
| **Injection.** SQL stays parameterized; the only spliced values are the literal `0` or the bind name `$11` of the ordinal bound, computed once. | `postgres_session.rs:269` | all PG tests |
| **Supply chain.** No new dependency, no `unsafe`. | `Cargo.toml` unchanged | clippy `-D warnings` |

Section 3a of the gate, by surface: a new route or RPC (none: no route, no RPC, no wire change); forged identity
(the foreign-identity test above); parser of untrusted structure (stored payloads were already bounded by
`decode_event`; the tail is now bounded by events and bytes before decoding); outbound calls, URL or path building,
unauthenticated expensive endpoint, web rendering, config (none touched).

## 7. Recovery guarantee rows (section 2)

The cache is never durable state, so no class changed and nothing new is **L**.

| Component x phase | Class | Enforcing code | Proving test |
|---|---|---|---|
| Worker x model call (before or after first token) | R | The marker is a row read by ordinal (`runner_view.rs:361`); a new process seeds from storage (`:399` to `:420`). | `generated_event_sequences_give_the_same_view_incrementally_and_in_full`; the crash and takeover phases in `pipeline_fanout_effect_pg_tests.rs` (cross-check on) |
| Worker x resume or replay (HITL, wake, FU-269 / FU-265 continuation) | R | A resume is a new run, hence a new Runner and a new cache seeded from the durable rows (`session.rs:3733`, `:4238`). | the FU-265 and FU-269 continuation proofs in `pipeline_fanout_effect_pg_tests.rs` (unchanged assertions, cross-check on); `a_refresh_after_the_lease_moved_...` |
| Worker x fan-out child | R | A child's projected events are appended by the runner and read by ordinal; a branch row triggers one re-read (`runner_view.rs:228`). | the Parallel and Map proofs in `pipeline_fanout_effect_pg_tests.rs`; `a_snapshot_cut_a_first_branch_...`; the 64-item run (0 fallbacks) |
| Worker x settlement and output | I | An exact replay allocates no ordinal (`postgres_session.rs`, `is_already_stored`), so the view is unchanged. | `a_replayed_settle_changes_nothing_and_a_changed_one_conflicts` |
| Worker x lease loss during a refresh | F, then R by takeover | The shared fence and commit check. | `a_refresh_after_the_lease_moved_...`, `a_revoked_local_lease_stops_the_refresh` |
| PostgreSQL unavailable during a refresh | F, then R | A typed `StorageUnavailable` stops the run (`runner.rs:1086`). | existing session tests |
| Main, sandbox supervisor, NATS, LLM gateway, Web | not touched | - | - |

## 8. Browser evidence

Run on 2026-11-06 in Claude in Chrome, in my own tab, with no response mocks, on a standalone stack `fu287`
(`http://fu287.localhost:19400`) built from the real-model dump with the production profile (recovery, owner recovery
and Code on; verify-profile 50/50). The stack has been torn down (`stack.sh down --volumes`).

- **Images** (tag `fu287-a65a68671`, built from this branch): Main `sha256:06d963ac2cfd…`, Worker
  `sha256:350f66524d04…`, Web `sha256:bdf7d0eff499…`. The Worker binary holds `rows_read` (this branch's span
  field) and no cross-check code (section 6).
- **No migration**, so the dump was not snapshotted. The dump's known tenant-migration gap (FU-279) was worked around
  on this throwaway database only, by adding the five missing `centry.audit_events` columns before startup.
- **Long chat (conversation 887, gpt-5.4-mini).** 6 turns that each depend on the earlier ones (a code word, a
  number, "add 17", "spell the code word backwards"); the answers were right (58, `7-DIHCRO`). After a reload all
  turns render. All 6 executions SUCCEEDED (1.2 to 1.8 s each). Worker refresh spans: `rows_read` 2 or 3,
  `fallback=false`.
  - Observation, not this change: in "To completion" mode the goal check said "no original number in the
    transcript" for the "add 17" turn and continued twice; the model's answer was right.
  - Conversation 487 from the dump was refused at admission ("This agent turn requires the current execution path"):
    it is bound to a legacy pipeline participant, so a fresh chat was used.
- **64-item Map (conversation 888, run-to-completion off, `aria-pressed=false` before send).** Fixture app 170:
  an inline-LLM Map of 64 items (`w00` to `w63`) at concurrency 8 on gpt-5.4-mini. The page was reloaded mid-run.
  After it: 64 entries, indexes 0 to 63 in source order, 64 succeeded, 0 failed, each answer its own item.
  Execution `8e9cdf04…` SUCCEEDED in 10.9 s wall-clock (real model). Worker: 199 refresh spans, 0 with
  `fallback=true`; `rows_read` 1 in 192, 2 in 2, 3 in 5. No WARN or ERROR lines.

## 9. Fixtures

All fixtures are code: events appended through the session service (`Fixture::append`), and in one test raw rows
inserted by SQL copied from an appended row (a NULL author or NULL branch cannot be decoded into an `Event`).
The browser fixture (app 170) was created through the UI's own REST endpoint from the signed-in page
(`POST /api/v2/elitea_core/applications/prompt_lib/2`), not through the database.

## 10. Follow-ups

- **FU-293:** the remaining about 45 ms of flat per-item persistence in a Map item (child session creation, checkpoint
  saves and the join's per-item result appends).
- **FU-294:** durable O(1) stored and active totals for the session, so the capacity check never classifies payloads.
- The vendored-runner hook must be carried on an ADK upgrade (the compile guard and the vendor README flag it).
