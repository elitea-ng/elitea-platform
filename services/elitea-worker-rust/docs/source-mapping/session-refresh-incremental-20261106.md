# Incremental root-session refresh (FU-287) - 2026-11-06

Branch `perf/session-refresh-incremental` from `main` `8d4edfcfd`. Plan:
`handoffs/point5-wave1-20261008/fu287/PLAN.md` (candidate 1, ordinal-based, no migration). Worker and vendored
`adk-runner` only; no Main, proto, Helm, compose or CI change, no migration.

## 1. Business behaviour taken from the current platform, and what was not ported

The Runner's model view after every persisted event is unchanged: the complete, identity-scoped active history
(`RunnerSessionService::get` = `PostgresSessionService::get` + `runner_history::project`). Only how the Worker reads it
changed. Nothing is ported from the current platform; this is a Worker-internal read path.

## 2. Changed paths

| Path | Change |
|---|---|
| `libs/rust/vendor/adk-runner/src/runner.rs`, `lib.rs` | Optional `SessionEventRefresh` hook (`with_session_event_refresher`). Unset, the refresh is today's full `get`. Both call sites (main loop, transfer loop) pass it. |
| `libs/rust/vendor/README.md`, `libs/rust/agent-runtime/src/lib.rs` | Patch note and compile guard. |
| `src/state/postgres_session_classified_events.sql` (new), `postgres_session_active_events.sql` | The row-local classification is one fragment with an ordinal lower bound inside `scoped`; the retirement rules stay in the active-events tail. |
| `src/state/postgres_session.rs` | `lock_writer_row` returns `next_event_ordinal`; `events_since`, `seed_active`, `decode_stored_event`, `refresh_span`, `limits()`. |
| `src/agents/runner_history.rs` | `project_events` split out; `project` calls it. One projection for both paths. |
| `src/agents/runner_view.rs` (new) | `IncrementalRunnerView`. |
| `src/agents/session.rs`, `ordinary.rs` | `RunnerSessionService::{with_event_source, refreshing}`; the three Runner build sites call `refreshing`; the source travels in `OrdinaryRuntimeBindings` and `PipelineStateServices`. |

## 3. Tests

See the test names in `src/agents/runner_view_pg_tests.rs` and `src/agents/native_history_tests.rs`. A cross-check mode
(`runner_view::cross_check_enabled`, on for the whole test build) makes every refresh also run the full read and
compare event for event, so every existing PostgreSQL suite which drives a Runner through the view
(`pipeline_fanout_effect_pg_tests`: Parallel, Map, FU-269/FU-265) proves it without changed assertions. A refresh skips
the comparison only when another writer appended between the two reads.

## 4. Performance

- Mechanism: `runner_view.rs` `refresh_locked`; `postgres_session.rs` `read_events_since`. A refresh is BEGIN, lock + head,
  at most one range scan of the new rows, COMMIT. Rows parsed: the new rows only.
- Proof: `each_refresh_reads_only_the_new_rows_and_a_whole_run_at_most_twice_the_rows` (64 steps: each refresh reads
  exactly its new rows, the run reads at most 2n, one seed, no fallback).
- Measured (64 saved Agent items, 100 ms model, same machine, same build, view off then on): concurrency 8
  8.84 s to 3.82 s; concurrency 1 11.49 s to 9.93 s. Root refreshes of the run: 321, 322 rows read, 0 fallbacks.
  The plan's targets (8: <= 2.0 s, 1: <= 7.5 s) are NOT met; the remaining time is outside the root refresh.

## 5. Durability, resilience

- The cache is per run and in memory, never durable. A new process (restart, takeover, wake) starts from a full read.
- A new row which could bring back a dropped row (the first branch row after a snapshot cut, a snapshot of another
  agent), a gap in the ordinals, or a view over the full read's bounds (`max_events`, retained bytes) makes the refresh
  read in full (the span field `fallback`). There is no in-memory resurrect path.
- Over `max_events` the refresh returns the full read's typed `session.resource_exhausted`. Over the retained-byte bound
  the view keeps nothing resident and reads in full on every refresh, as before.
- The shared writer lock is held only inside the refresh's transaction (`driven`), never across model or tool work.
  `the_writer_lock_is_released_when_a_refresh_returns`, `a_concurrent_foreign_append_is_never_lost_or_duplicated`.
- Lease loss: `session.writer_not_current`, no stale view (`a_refresh_after_the_lease_moved_...`, `a_revoked_local_lease_...`).

Recovery guarantee rows are unchanged from the plan (section 6): Worker x model call R; effectful tool R/C; HITL R;
fan-out child R; settlement I (an exact replay allocates no ordinal); lease loss F then R; no new L.

## 6. Security

The hook refuses any identity other than the one it is bound to (`a_view_serves_only_its_own_session`); no new caller
input; the span carries counts only (`rows_read`, `fallback`); SQL stays parameterized (the only spliced value is the
literal `0` or the bind name `$11`); no new dependency, no `unsafe`.

## 7. Fixtures and follow-ups

Fixtures are created through the session service (append API); the SQL-equivalence test inserts raw rows copied from an
appended row because a NULL author or NULL branch cannot be decoded into an `Event`. Follow-ups: the plan's "after"
targets for the 64-item run need a profile of the remaining time; the vendored-runner hook must be carried on an ADK
upgrade; browser confirmation is pending.
