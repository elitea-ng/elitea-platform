# A parked fan-out reads as waiting for a person (FU-284), 2026-11-06

`M/` = `services/elitea-main/internal/`. Main only; no migration, no contract change.

## Business behaviour

Taken from the current platform: a run that stops for a person's decision ends a run-to-completion chain as waiting.
No goal check runs and no continuation starts until the person answers. The run is announced as "needs you", and an
open pause is not something you can fork from.

On the new platform, a Parallel or a Map pauses by parking its members on cards (`fanout_parked_v1`, contract §8)
instead of a root `hitl_interrupt`. Six readers knew only the root pause keys. A parked fan-out therefore read as
finished:
- the chain ran its goal check and could start a continuation while the cards were open; the answered card's wake then
  ended INTERNAL (FU-284, chat 40);
- a parked schedule, webhook or handed-off run was announced as "completed";
- the workspace hand-back outcome said "completed";
- the editor's test run said TERMINAL;
- the conversation list did not say "needs you";
- a fork could copy an open park whose cards belong to the source response's ledger.

**What changed:** one shared definition of "waits for a person" (`M/domain/responsepause`: `hitl_interrupt`,
`hitl_interrupts`, `authorization_requests`, `fanout_parked_v1`), used by every reader that asks it.

**Deliberately unchanged:**
- **Unattended approval.** `lockPausedResponse` (`M/infra/db/repos/approval_unattended_store.go:152-165`) decides only
  the root-pause path. A fan-out card's unattended approval closes its ledger cards and drops `fanout_parked_v1`
  itself (`approval_unattended_store.go:296-325`, `approval_unattended.go:645`).
- **Cancel.** `agent_cancel.sql:66-73` cancels a settled job only when the response holds a root pause, and strips
  only those keys. A parked run has settled, and its cards close by supersede (`M/infra/db/repos/
  execution_interrupts.go:701-727`; T-2A3 browser case 6). Adding the park key there would set the job cancelled
  while the ledger cards stayed open.
- **Mix refusal, strip and decode lists** (`execution_interrupts_wiring.go:38`, `node_recovery_projection.go:30`): not
  "is it waiting" readers.

## Changed paths

| Path | Change |
|---|---|
| `M/domain/responsepause/responsepause.go` | new: `WaitingKeys`, `SQL(alias)` over a closed set of aliases (`Unqualified`, `Response`, `Runs`, `Group`) with an identifier backstop, `Waiting(meta)` |
| `M/infra/db/repos/run_finished.go:322-352` | `readResponseFacts(…, executionID)`: `Paused` = waiting keys, or a fan-out wake **of this park** took the response |
| `M/infra/db/repos/run_chains_settle.go:42` | passes the settled execution |
| `M/infra/db/repos/workspace_handoff_continue.go:427` | hand-back outcome `waiting` via the predicate |
| `M/infra/db/repos/editor_test_runs.go:63` | `PAUSED` via the predicate (plus its own extra keys) |
| `M/infra/db/chatactivity/activity.go:34-46` | "needs you" adds a park arm beside the partial index's exact text |
| `M/infra/db/repos/conversation_fork.go:93-96` | `forkPauseKeys` = waiting keys + `node_recovery_required_v1` |

## Tests

All Main tests listed below ran on a disposable PostgreSQL 18. "Fails on main" was checked by restoring main's version
of the reader and re-running.

| Test | Proves | Fails on main |
|---|---|---|
| `TestPostgresChainWaitsOnAParkedFanout` | real hook and repository: the chain ends `waiting`, no goal check, no continuation, one link; the wake's settle leaves the chain unchanged | yes ("completed") |
| `TestPostgresChainWaitsWhenTheParkWakeAlreadyTookTheResponse` | a wake of this park already took the response: still `waiting`, no goal check | yes ("completed", 1 check) |
| `TestPostgresChainKeepsItsOutcomeWhenAnotherExecutionTookTheResponse` | a non-wake owner, or a wake of another park, keeps today's outcome (completed after one goal check) | no (control) |
| `TestPostgresChainStillContinuesARunThatDidNotPark` | a step-limit run still starts its continuation | no (control) |
| `TestPostgresRunFinishedAnnouncesAParkedRunAsWaiting` | a parked schedule run is announced `waiting` | yes ("completed") |
| `TestConversationActivityFollowsTheTurn` (park case) | "needs you" and `chatactivity.Paused` for a park | yes (idle) |
| `TestPostgresHandoffContinueCommitsTheSendAndTheTurnTogether` (park case) | hand-back outcome `waiting` | yes ("completed") |
| `TestForkPointMustBeSettled` (park case) | a parked point is `fork_point_unsettled` | yes (201) |
| `TestEditorLifecyclePostgres/…/parked_fanout` | the editor test run is `PAUSED` | — (the fixture's disposable listener) |
| `responsepause` unit tests | each key waits; other keys don't; the SQL per alias; the alias backstop | — |
| `TestPausedPredicateNamesEveryWaitingKey` | "needs you" carries every waiting key and the partial index's exact text | — |

The whole `./internal` suite results are in the PR.

## Performance

- **Chain settle and run finished.** `readResponseFacts` stays one statement. The wake check is an `EXISTS` on
  `execution_interrupt_wakes_wake_execution` (`migrations/shared/0202_execution_interrupt_wakes.sql:76`). It reads at
  most the rows of one wake execution, and only for the one response row read by uuid.
- **"Needs you."** The first arm keeps `chat_message_group_paused_idx`'s exact text (tenant/0153:128-132), so the
  planner can still use it. The park arm is a filter on the rows that `conversation_id = ANY($1)` already selects
  through `chat_message_group_conversation_newest_idx`. No new index and no migration (an unmerged migration cannot go
  onto the shared stack).
- **Other readers.** Each is one extra `?|` on a row already read.

## Durability

- Nothing new is written. The chain end is written once, as before (`EndChain`, `TestPostgresUnattendedChainSettleViewAndEndOnce`).
- **What happens when a decision meets a running chain:**
  - *Parked, no decision yet.* The chain ends `waiting` (`M/application/unattendedchain/hook.go:145-146`). The decision
    queues the wake. The wake is not a chain link, so its settle is a no-op for the chain
    (`M/infra/db/repos/run_chains_settle.go:31-33`), the same as a cloud HITL resume. Cloud never writes a `resume`
    link; that kind is desktop-only (`M/application/unattended/unattended.go:62-69`).
  - *A decision already waiting at park time.* The wake is queued in the settlement transaction
    (`execution_interrupts_wiring.go:405-418`), and wake start takes the response (`execution_interrupt_wakes.go:375-378`).
    The settle sees either the park key or a response held by a wake of this very park, and ends `waiting` either way.
  - *A continuation already running when a card is answered.* No continuation starts while a park is open, so a
    park can no longer reach this state.

## Resilience

- A settle replayed after a crash reads the same facts and ends the chain once (`EndChain` refuses a second end).
- Typed and readable: the chain's end state and the run-finished outcome say `waiting`, and the conversation says
  "needs you".

## Security

- **SQL construction.** `responsepause.SQL` builds only from constant keys and one of four constant aliases. A value
  outside the identifier backstop yields `FALSE`. The response and execution are bound parameters (`$1`, `$2`), and the
  schema comes from `tenantSchema` as before.
- No authorization, identity, egress or secret change. No new endpoint.
- A fork can no longer copy an open park, whose cards would answer into another response's ledger.

## Recovery rows

| Component × phase | Class | Enforcing code | Proof |
|---|---|---|---|
| Main × HITL pause and decision (fan-out park in a chain) | R | `hook.go:145-146` with `readResponseFacts` | `TestPostgresChainWaitsOnAParkedFanout` |
| Main × settlement (replayed settle) | I | `EndChain` single end | `TestPostgresUnattendedChainSettleViewAndEndOnce` |
| Main × settlement while a wake runs | I | `run_finished.go` wake check | `TestPostgresChainWaitsWhenTheParkWakeAlreadyTookTheResponse` |
| Web × activity (needs you) | I | `chatactivity.PausedPredicate` | `TestConversationActivityFollowsTheTurn` |

## Browser evidence

To be added after the fanpar pass: main images plus this branch's Main; a run-to-completion chat with a Parallel
whose branch asks; the run mode read from `run_chains`; while parked, the chain `waiting` with one link and no goal
check, and "needs you" in the list; then answer, reload, one answer, ledger CONSUMED once.

## Follow-ups

- Extending `chat_message_group_paused_idx` and `chat_group_meta_paused` (tenant/0153) with `fanout_parked_v1` in a
  later tenant migration would let the index cover the park arm too. This is not needed for correctness, and the
  conversation sync bump already fires on the park's `is_streaming` flip.
